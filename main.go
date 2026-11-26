package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
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

const (
	defaultRefreshIntervalSeconds = 15
	freshMinGapDuration           = 4 * time.Second
	fileWatchInterval             = 2 * time.Second

	staticGzipMinBytes = 256
	apiGzipMinBytes    = 512

	snapshotViewFull    = "full"
	snapshotViewSummary = "summary"
	snapshotViewCompact = "compact"
	snapshotViewTUI     = "tui"
)

func loadConfig() AppConfig {
	interval := defaultRefreshIntervalSeconds
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

func normalizeSnapshotView(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case snapshotViewSummary:
		return snapshotViewSummary
	case snapshotViewCompact:
		return snapshotViewCompact
	case snapshotViewTUI:
		return snapshotViewTUI
	default:
		return snapshotViewFull
	}
}

func gzipBytes(src []byte, level int) ([]byte, error) {
	var buf bytes.Buffer
	gw, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		return nil, err
	}
	if _, err := gw.Write(src); err != nil {
		_ = gw.Close()
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var gzipWriterPool = sync.Pool{
	New: func() any {
		gw, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return gw
	},
}

func gzipBytesPooled(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzipWriterPool.Get().(*gzip.Writer)
	gw.Reset(&buf)
	_, writeErr := gw.Write(src)
	closeErr := gw.Close()
	gw.Reset(io.Discard)
	gzipWriterPool.Put(gw)
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return buf.Bytes(), nil
}

func acceptsGzip(r *http.Request) bool {
	for _, hdr := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(hdr, ",") {
			item := strings.TrimSpace(part)
			if item == "" {
				continue
			}
			coding, params, hasParams := strings.Cut(item, ";")
			coding = strings.ToLower(strings.TrimSpace(coding))
			if coding != "gzip" && coding != "x-gzip" {
				continue
			}
			if !hasParams {
				return true
			}
			q := 1.0
			for _, p := range strings.Split(params, ";") {
				k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
				if ok && strings.EqualFold(strings.TrimSpace(k), "q") {
					if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						q = parsed
					}
				}
			}
			if q > 0 {
				return true
			}
		}
	}
	return false
}

func normalizeWeakETag(tag string) string {
	tag = strings.TrimSpace(tag)
	if strings.HasPrefix(tag, "W/") || strings.HasPrefix(tag, "w/") {
		tag = strings.TrimSpace(tag[2:])
	}
	return tag
}

func ifNoneMatchMatches(headerVal, currentETag string) bool {
	headerVal = strings.TrimSpace(headerVal)
	if headerVal == "" || currentETag == "" {
		return false
	}
	if headerVal == "*" {
		return true
	}
	target := normalizeWeakETag(currentETag)
	inQuote := false
	start := 0
	for i := 0; i <= len(headerVal); i++ {
		if i < len(headerVal) {
			if headerVal[i] == '"' && (i == 0 || headerVal[i-1] != '\\') {
				inQuote = !inQuote
			}
			if headerVal[i] != ',' || inQuote {
				continue
			}
		}
		candidate := strings.TrimSpace(headerVal[start:i])
		start = i + 1
		if candidate == "*" {
			return true
		}
		if candidate != "" && normalizeWeakETag(candidate) == target {
			return true
		}
	}
	return false
}

func requestMatchesETag(r *http.Request, currentETag string) bool {
	for _, val := range r.Header.Values("If-None-Match") {
		if ifNoneMatchMatches(val, currentETag) {
			return true
		}
	}
	return false
}

func writeDynamicJSON(w http.ResponseWriter, r *http.Request, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Vary", "Accept-Encoding")
	if len(raw) >= apiGzipMinBytes && acceptsGzip(r) {
		if gz, err := gzipBytesPooled(raw); err == nil && len(gz) < len(raw) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", strconv.Itoa(len(gz)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(gz)
			}
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(raw)
	}
}

func computeHistorySemanticETag(hist *AntigravityHistoryResponse) (string, error) {
	sem := *hist
	sem.FromSec = 0
	sem.ToSec = 0
	if len(hist.Series) > 0 {
		sem.Series = make([]AntigravityHistorySeries, len(hist.Series))
		for i, s := range hist.Series {
			sc := s
			if len(s.EstimatedPoint) >= 2 {
				// Strip wall-clock timestamp while retaining estimated remaining percentage value
				sc.EstimatedPoint = []float64{0, s.EstimatedPoint[1]}
			}
			sem.Series[i] = sc
		}
	}
	b, err := json.Marshal(sem)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf(`W/"hist-%s"`, hex.EncodeToString(sum[:])[:16]), nil
}

type cachedSnapshotView struct {
	rawJSON       []byte
	gzipJSON      []byte
	etag          string
	generatedAtMs int64
}

func buildCachedSnapshotView(viewName string, payload any, semanticPayload any, generatedAtMs int64) (cachedSnapshotView, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return cachedSnapshotView{}, err
	}
	semRaw, err := json.Marshal(semanticPayload)
	if err != nil {
		return cachedSnapshotView{}, err
	}
	sum := sha256.Sum256(semRaw)
	etag := fmt.Sprintf(`W/"snap-%s-%s"`, viewName, hex.EncodeToString(sum[:])[:16])

	var gz []byte
	if len(raw) > 0 {
		if compressed, gErr := gzipBytes(raw, gzip.BestCompression); gErr == nil && len(compressed) < len(raw) {
			gz = compressed
		}
	}
	return cachedSnapshotView{
		rawJSON:       raw,
		gzipJSON:      gz,
		etag:          etag,
		generatedAtMs: generatedAtMs,
	}, nil
}

type BoardState struct {
	mu       sync.RWMutex
	snapshot Snapshot
	views    map[string]cachedSnapshotView
}

func (b *BoardState) Set(snap Snapshot) {
	semSnap := snap
	semSnap.GeneratedAtMs = 0
	semSnap.Antigravity.UpdatedAtMs = 0
	semSnap.AIStudio.UpdatedAtMs = 0

	viewSpecs := []struct {
		name string
		pub  any
		sem  any
	}{
		{snapshotViewFull, snap, semSnap},
		{snapshotViewSummary, buildSnapshotSummaryView(snap), buildSnapshotSummaryView(semSnap)},
		{snapshotViewCompact, buildSnapshotCompactView(snap), buildSnapshotCompactView(semSnap)},
		{snapshotViewTUI, buildSnapshotTUIView(snap), buildSnapshotTUIView(semSnap)},
	}

	built := make(map[string]cachedSnapshotView, len(viewSpecs))
	for _, spec := range viewSpecs {
		entry, err := buildCachedSnapshotView(spec.name, spec.pub, spec.sem, snap.GeneratedAtMs)
		if err != nil {
			log.Printf("[board] marshal snapshot view %s error: %v", spec.name, err)
			return
		}
		built[spec.name] = entry
	}

	b.mu.Lock()
	b.snapshot = snap
	b.views = built
	b.mu.Unlock()
}

func (b *BoardState) GetJSON() []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.views == nil {
		return nil
	}
	return b.views[snapshotViewFull].rawJSON
}

func (b *BoardState) GetView(view string) (cachedSnapshotView, bool) {
	norm := normalizeSnapshotView(view)
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.views == nil {
		return cachedSnapshotView{}, false
	}
	entry, ok := b.views[norm]
	if !ok || len(entry.rawJSON) == 0 {
		return cachedSnapshotView{}, false
	}
	return entry, true
}

type staticAsset struct {
	contentType string
	raw         []byte
	gzipped     []byte
	etagRaw     string
	etagGzip    string
}

type staticAssetStore struct {
	files map[string]*staticAsset
}

func detectStaticContentType(name string, raw []byte) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".json":
		return "application/json; charset=utf-8"
	case ".ico":
		return "image/x-icon"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return http.DetectContentType(raw)
}

func newStaticAssetStore(sub fs.FS) (*staticAssetStore, error) {
	files := make(map[string]*staticAsset)
	err := fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		raw, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		hash16 := hex.EncodeToString(sum[:])[:16]
		asset := &staticAsset{
			contentType: detectStaticContentType(p, raw),
			raw:         raw,
			etagRaw:     fmt.Sprintf(`"psb-%s"`, hash16),
		}
		if len(raw) >= staticGzipMinBytes {
			if gz, gErr := gzipBytes(raw, gzip.BestCompression); gErr == nil && len(gz) < len(raw) {
				asset.gzipped = gz
				asset.etagGzip = fmt.Sprintf(`"psb-%s-gz"`, hash16)
			}
		}
		files[p] = asset
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &staticAssetStore{files: files}, nil
}

func (s *staticAssetStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	clean := path.Clean("/" + r.URL.Path)
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" {
		rel = "index.html"
	}
	asset, ok := s.files[rel]
	if !ok {
		http.NotFound(w, r)
		return
	}

	useGzip := len(asset.gzipped) > 0 && acceptsGzip(r)
	etag := asset.etagRaw
	if useGzip {
		etag = asset.etagGzip
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("ETag", etag)

	if requestMatchesETag(r, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", asset.contentType)
	if useGzip {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(asset.gzipped)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(asset.gzipped)
		}
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(asset.raw)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(asset.raw)
	}
}

// RefreshCoordinator serializes snapshot collections (singleflight), supports
// mid-flight rerun coalescing for file-change triggers (e.g., lamplighter state.json),
// and enforces a minimum debounce interval for on-demand ?fresh=1 requests.
type RefreshCoordinator struct {
	mu             sync.Mutex
	inFlight       bool
	pendingRerun   bool
	doneCh         chan struct{}
	lastFinishedAt time.Time
	minFreshGap    time.Duration
	nowFn          func() time.Time
	collectFn      func(ctx context.Context)
}

func NewRefreshCoordinator(minFreshGap time.Duration, collectFn func(ctx context.Context)) *RefreshCoordinator {
	return &RefreshCoordinator{
		minFreshGap: minFreshGap,
		nowFn:       time.Now,
		collectFn:   collectFn,
	}
}

func (rc *RefreshCoordinator) launchLocked(parentCtx context.Context) chan struct{} {
	rc.inFlight = true
	ch := make(chan struct{})
	rc.doneCh = ch
	go func() {
		rc.collectFn(parentCtx)
		rc.mu.Lock()
		rc.lastFinishedAt = rc.nowFn()
		close(ch)
		if rc.pendingRerun && parentCtx.Err() == nil {
			rc.pendingRerun = false
			rc.launchLocked(parentCtx)
			rc.mu.Unlock()
			return
		}
		rc.inFlight = false
		rc.doneCh = nil
		rc.mu.Unlock()
	}()
	return ch
}

// RunBlocking executes (or joins) one collection synchronously and waits until it finishes.
func (rc *RefreshCoordinator) RunBlocking(ctx context.Context) {
	rc.mu.Lock()
	var ch chan struct{}
	if rc.inFlight {
		ch = rc.doneCh
	} else {
		ch = rc.launchLocked(ctx)
	}
	rc.mu.Unlock()

	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// TriggerBackground starts an asynchronous collection if idle.
// If rerunIfInFlight is true and a collection is already running, one follow-up
// collection is coalesced immediately after the current one completes.
func (rc *RefreshCoordinator) TriggerBackground(parentCtx context.Context, rerunIfInFlight bool) {
	if parentCtx.Err() != nil {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.inFlight {
		if rerunIfInFlight {
			rc.pendingRerun = true
		}
		return
	}
	rc.launchLocked(parentCtx)
}

// RefreshIfStale serves ?fresh=1 requests:
// - If a collection is currently in flight, waits for it to finish (singleflight).
// - If the last collection completed < minFreshGap ago, returns false immediately (debounced).
// - Otherwise starts a new collection and waits for completion (respecting waitCtx).
func (rc *RefreshCoordinator) RefreshIfStale(waitCtx, parentCtx context.Context) bool {
	rc.mu.Lock()
	if rc.inFlight {
		ch := rc.doneCh
		rc.mu.Unlock()
		select {
		case <-ch:
			return true
		case <-waitCtx.Done():
			return false
		}
	}
	if !rc.lastFinishedAt.IsZero() && rc.nowFn().Sub(rc.lastFinishedAt) < rc.minFreshGap {
		rc.mu.Unlock()
		return false
	}
	ch := rc.launchLocked(parentCtx)
	rc.mu.Unlock()

	select {
	case <-ch:
		return true
	case <-waitCtx.Done():
		return false
	}
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
	// Prime mtime/size trackers for secret key and lamplighter state.json
	cpa.CheckSecretKeyChanged()
	cpa.CheckLamplighterStateChanged()

	ais := NewAISCollector(AISConfig{
		BaseURL:    cfg.AISBaseURL,
		EnvFile:    cfg.AISEnvFile,
		QuotasFile: cfg.QuotasFile,
	})

	state := &BoardState{}

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

	coordinator := NewRefreshCoordinator(freshMinGapDuration, runRefresh)

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Initial synchronous collection before serving traffic (with bounded timeout)
	log.Printf("[board] starting initial snapshot collection...")
	coordinator.RunBlocking(rootCtx)
	log.Printf("[board] initial snapshot ready")

	// Periodic background refresh worker (default 15s)
	go func() {
		ticker := time.NewTicker(time.Duration(cfg.RefreshIntervalSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				coordinator.TriggerBackground(rootCtx, false)
			}
		}
	}()

	// Lightweight 2s read-only os.Stat watcher for CPAMP admin key & lamplighter state.json
	go func() {
		ticker := time.NewTicker(fileWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-rootCtx.Done():
				return
			case <-ticker.C:
				if cpa.CheckSecretKeyChanged() {
					log.Printf("[board] secret key update detected, refreshing snapshot immediately")
					coordinator.TriggerBackground(rootCtx, true)
				}
				if cpa.CheckLamplighterStateChanged() {
					log.Printf("[board] lamplighter state.json update detected, refreshing snapshot immediately")
					coordinator.TriggerBackground(rootCtx, true)
				}
			}
		}
	}()

	subWeb, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("[board] embed web fs error: %v", err)
	}
	handler, err := newAppHandler(cfg, state, coordinator, cpa, rootCtx, subWeb)
	if err != nil {
		log.Fatalf("[board] init handler error: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
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

func newAppHandler(
	cfg AppConfig,
	state *BoardState,
	coordinator *RefreshCoordinator,
	cpa *CPACollector,
	rootCtx context.Context,
	subWeb fs.FS,
) (http.Handler, error) {
	assets, err := newStaticAssetStore(subWeb)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	writeJSONError := func(w http.ResponseWriter, r *http.Request, status int, body string) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	}

	checkPasscode := func(w http.ResponseWriter, r *http.Request) bool {
		if cfg.ViewPasscode == "" {
			return true
		}
		provided := strings.TrimSpace(r.Header.Get("X-View-Passcode"))
		if subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.ViewPasscode)) != 1 {
			writeJSONError(w, r, http.StatusUnauthorized, `{"error":"passcode_required","message":"请输入访问口令"}`)
			return false
		}
		return true
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	})

	mux.HandleFunc("GET /api/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		raw, mErr := json.Marshal(map[string]any{
			"passcode_required":        cfg.ViewPasscode != "",
			"refresh_interval_seconds": cfg.RefreshIntervalSeconds,
		})
		if mErr != nil {
			writeJSONError(w, r, http.StatusInternalServerError, `{"error":"internal_error"}`)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(raw)
		}
	})

	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if !checkPasscode(w, r) {
			return
		}
		q := r.URL.Query()
		freshParam := strings.TrimSpace(q.Get("fresh"))
		if coordinator != nil && (freshParam == "1" || strings.EqualFold(freshParam, "true")) {
			coordinator.RefreshIfStale(r.Context(), rootCtx)
		}

		view := normalizeSnapshotView(q.Get("view"))
		entry, ok := state.GetView(view)
		if !ok {
			writeJSONError(w, r, http.StatusServiceUnavailable, `{"error":"initializing","message":"数据初始化中"}`)
			return
		}

		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("ETag", entry.etag)
		w.Header().Set("X-Generated-At-Ms", strconv.FormatInt(entry.generatedAtMs, 10))

		if requestMatchesETag(r, entry.etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if len(entry.gzipJSON) > 0 && acceptsGzip(r) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", strconv.Itoa(len(entry.gzipJSON)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(entry.gzipJSON)
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(entry.rawJSON)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(entry.rawJSON)
		}
	})

	mux.HandleFunc("GET /api/antigravity/history", func(w http.ResponseWriter, r *http.Request) {
		if !checkPasscode(w, r) {
			return
		}
		q := r.URL.Query()
		accountID := strings.TrimSpace(q.Get("account"))
		windowKind := strings.TrimSpace(q.Get("window"))
		rangeName := strings.TrimSpace(q.Get("range"))
		if accountID == "" {
			writeJSONError(w, r, http.StatusBadRequest, `{"error":"missing_account","message":"缺少账号参数"}`)
			return
		}
		if cpa == nil {
			writeJSONError(w, r, http.StatusNotFound, `{"error":"not_found","message":"未找到指定账号历史数据"}`)
			return
		}
		hist, ok := cpa.GetAccountHistory(accountID, windowKind, rangeName)
		if !ok {
			writeJSONError(w, r, http.StatusNotFound, `{"error":"not_found","message":"未找到指定账号历史数据"}`)
			return
		}

		etag, _ := computeHistorySemanticETag(hist)
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("X-History-To-Sec", strconv.FormatInt(hist.ToSec, 10))
		if etag != "" {
			w.Header().Set("ETag", etag)
			if requestMatchesETag(r, etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		raw, mErr := json.Marshal(hist)
		if mErr != nil {
			writeJSONError(w, r, http.StatusInternalServerError, `{"error":"marshal_error","message":"历史数据序列化失败"}`)
			return
		}
		writeDynamicJSON(w, r, raw)
	})

	mux.Handle("/", assets)

	return securityHeaders(mux), nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
