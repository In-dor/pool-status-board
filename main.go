package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"
)

//go:embed web/*
var webFS embed.FS

type AppConfig struct {
	ListenAddr             string
	RefreshIntervalSeconds int
	ViewPasscode           string
	CPAMPBaseURL           string
	CPAMPAdminKeyEnv       string
	CPAMPAdminKeyFile      string
	CPAAuthsDir            string
	CPAPriorityDir         string
	CPALamplighterDir      string
	CPAContainerAuthDir    string
	AISBaseURL             string
	AISEnvFile             string
	QuotasFile             string
}

func loadConfig() AppConfig {
	interval := 60
	if raw := strings.TrimSpace(os.Getenv("REFRESH_INTERVAL_SECONDS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 15 && n <= 3600 {
			interval = n
		}
	}
	return AppConfig{
		ListenAddr:             envOrDefault("LISTEN_ADDR", ":8080"),
		RefreshIntervalSeconds: interval,
		ViewPasscode:           strings.TrimSpace(os.Getenv("VIEW_PASSCODE")),
		CPAMPBaseURL:           envOrDefault("CPAMP_BASE_URL", "http://cpa-manager-plus:18317"),
		CPAMPAdminKeyEnv:       strings.TrimSpace(os.Getenv("CPAMP_ADMIN_KEY")),
		CPAMPAdminKeyFile:      envOrDefault("CPAMP_ADMIN_KEY_FILE", "/mnt/secrets/cpamp_admin_key"),
		CPAAuthsDir:            envOrDefault("CPA_AUTHS_DIR", "/mnt/cpa/auths"),
		CPAPriorityDir:         envOrDefault("CPA_PRIORITY_DIR", "/mnt/cpa/priority"),
		CPALamplighterDir:      envOrDefault("CPA_LAMPLIGHTER_DIR", "/mnt/cpa/lamplighter"),
		CPAContainerAuthDir:    envOrDefault("CPA_CONTAINER_AUTH_DIR", "/root/.cli-proxy-api"),
		AISBaseURL:             envOrDefault("AIS_BASE_URL", "http://aistudio2api:2048"),
		AISEnvFile:             envOrDefault("AIS_ENV_FILE", "/mnt/ais/.env"),
		QuotasFile:             envOrDefault("QUOTAS_FILE", "/app/quotas.json"),
	}
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

type BoardState struct {
	mu           sync.RWMutex
	snapshot     Snapshot
	snapshotJSON []byte
}

func (b *BoardState) Set(snap Snapshot) {
	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[board] marshal snapshot error: %v", err)
		return
	}
	b.mu.Lock()
	b.snapshot = snap
	b.snapshotJSON = raw
	b.mu.Unlock()
}

func (b *BoardState) GetJSON() []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snapshotJSON
}

func main() {
	cfg := loadConfig()

	cpa := NewCPACollector(CPAConfig{
		AuthsDir:         cfg.CPAAuthsDir,
		PriorityDir:      cfg.CPAPriorityDir,
		LamplighterDir:   cfg.CPALamplighterDir,
		ContainerAuthDir: cfg.CPAContainerAuthDir,
		CPAMPBaseURL:     cfg.CPAMPBaseURL,
		AdminKeyEnv:      cfg.CPAMPAdminKeyEnv,
		AdminKeyFile:     cfg.CPAMPAdminKeyFile,
	})
	// Prime secret file mtime tracker
	cpa.CheckSecretKeyChanged()

	ais := NewAISCollector(AISConfig{
		BaseURL:    cfg.AISBaseURL,
		EnvFile:    cfg.AISEnvFile,
		QuotasFile: cfg.QuotasFile,
	})

	state := &BoardState{}
	refreshTrigger := make(chan struct{}, 1)

	runRefresh := func(parentCtx context.Context) {
		ctx, cancel := context.WithTimeout(parentCtx, 25*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		var agPool AntigravityPool
		var aisPool AIStudioPool

		wg.Add(2)
		go func() {
			defer wg.Done()
			agPool = cpa.Collect(ctx)
		}()
		go func() {
			defer wg.Done()
			aisPool = ais.Collect(ctx)
		}()
		wg.Wait()

		snap := Snapshot{
			GeneratedAtMs:          time.Now().UnixMilli(),
			RefreshIntervalSeconds: cfg.RefreshIntervalSeconds,
			Antigravity:            agPool,
			AIStudio:               aisPool,
		}
		state.Set(snap)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initial synchronous collection before serving traffic (with bounded timeout)
	log.Printf("[board] starting initial snapshot collection...")
	runRefresh(rootCtx)
	log.Printf("[board] initial snapshot ready")

	// Periodic refresh + secret hot-reload worker
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.RefreshIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				runRefresh(rootCtx)
			case <-refreshTrigger:
				log.Printf("[board] secret key update detected, refreshing snapshot immediately")
				runRefresh(rootCtx)
			}
		}
	}()

	// Lightweight 3s mtime watcher for CPAMP admin key file
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				if cpa.CheckSecretKeyChanged() {
					select {
					case refreshTrigger <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	mux.HandleFunc("GET /api/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"passcode_required":        cfg.ViewPasscode != "",
			"refresh_interval_seconds": cfg.RefreshIntervalSeconds,
		})
	})

	checkPasscode := func(w http.ResponseWriter, r *http.Request) bool {
		if cfg.ViewPasscode == "" {
			return true
		}
		provided := strings.TrimSpace(r.Header.Get("X-View-Passcode"))
		if subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.ViewPasscode)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"passcode_required","message":"请输入访问口令"}`))
			return false
		}
		return true
	}

	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !checkPasscode(w, r) {
			return
		}
		payload := state.GetJSON()
		if len(payload) == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"initializing","message":"数据初始化中"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	})

	mux.HandleFunc("GET /api/antigravity/history", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !checkPasscode(w, r) {
			return
		}
		q := r.URL.Query()
		accountID := strings.TrimSpace(q.Get("account"))
		windowKind := strings.TrimSpace(q.Get("window"))
		rangeName := strings.TrimSpace(q.Get("range"))
		if accountID == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"missing_account","message":"缺少账号参数"}`))
			return
		}
		hist, ok := cpa.GetAccountHistory(accountID, windowKind, rangeName)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found","message":"未找到指定账号历史数据"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(hist)
	})

	subWeb, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("[board] embed web fs error: %v", err)
	}
	fileServer := http.FileServer(http.FS(subWeb))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" || p == "/index.html" || strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".svg") {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-rootCtx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	log.Printf("[board] listening on %s (refresh=%ds, passcode=%v)", cfg.ListenAddr, cfg.RefreshIntervalSeconds, cfg.ViewPasscode != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[board] server error: %v", err)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
