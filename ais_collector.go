package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type AISConfig struct {
	BaseURL    string
	EnvFile    string
	QuotasFile string
}

type QuotasFile struct {
	ResetTimezone string            `json:"reset_timezone"`
	ResetHour     int               `json:"reset_hour"`
	ResetMinute   int               `json:"reset_minute"`
	RuleNote      string            `json:"rule_note"`
	Models        []QuotaModelEntry `json:"models"`
}

type QuotaModelEntry struct {
	ID    string                    `json:"id"`
	Label string                    `json:"label"`
	Tiers map[string]QuotaTierLimit `json:"tiers"`
}

type QuotaTierLimit struct {
	Requests int64 `json:"requests"`
	Tokens   int64 `json:"tokens"`
}

type AISCollector struct {
	cfg    AISConfig
	client *http.Client

	envMu        sync.Mutex
	envMtime     time.Time
	authEnabled  bool
	authUsername string
	authPassword string

	quotaMu    sync.Mutex
	quotaMtime time.Time
	quotaSize  int64
	quotas     *QuotasFile

	sessMu        sync.Mutex
	sessionCookie *http.Cookie
	sessionExpiry time.Time
	lastLoginFail time.Time
	failCount     int
}

func NewAISCollector(cfg AISConfig) *AISCollector {
	return &AISCollector{
		cfg: cfg,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				Proxy:               nil, // Direct internal network connection only
				MaxIdleConns:        16,
				IdleConnTimeout:     60 * time.Second,
				TLSHandshakeTimeout: 5 * time.Second,
			},
		},
	}
}

// Raw API responses from AIStudio2API
type aisStatusResp struct {
	State          string `json:"state"`
	Running        bool   `json:"running"`
	Ready          bool   `json:"ready"`
	ActiveRequests int    `json:"active_requests"`
}

type aisAccountsResp struct {
	Accounts []aisRawAccount `json:"accounts"`
}

type aisRawAccount struct {
	ID             string          `json:"id"`
	Label          string          `json:"label"`
	Enabled        bool            `json:"enabled"`
	State          string          `json:"state"`
	BenefitTier    json.RawMessage `json:"benefit_tier"`
	ActiveRequests int             `json:"active_requests"`
}

type aisCooldownsResp struct {
	Cooldowns []aisRawCooldown `json:"cooldowns"`
}

type aisRawCooldown struct {
	AccountID    string    `json:"account_id"`
	AccountLabel string    `json:"account_label"`
	Channel      string    `json:"channel"`
	ModelID      string    `json:"model_id"`
	Scope        string    `json:"scope"`
	Until        time.Time `json:"until"`
}

type aisUsageLatency struct {
	AvgMS float64 `json:"avg_ms"`
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
}

type aisUsageStats struct {
	Requests        int64           `json:"requests"`
	Succeeded       int64           `json:"succeeded"`
	Failed          int64           `json:"failed"`
	Canceled        int64           `json:"canceled"`
	RateLimited     int64           `json:"rate_limited"`
	InputTokens     int64           `json:"input_tokens"`
	ReasoningTokens int64           `json:"reasoning_tokens"`
	ReplyTokens     int64           `json:"reply_tokens"`
	TotalTokens     int64           `json:"total_tokens"`
	FirstEvent      aisUsageLatency `json:"first_event"`
}

type aisUsageBucket struct {
	At string `json:"at"`
	aisUsageStats
}

type aisUsagePair struct {
	Account string `json:"account"`
	Model   string `json:"model"`
	aisUsageStats
}

type aisUsageResp struct {
	Totals  aisUsageStats    `json:"totals"`
	Buckets []aisUsageBucket `json:"buckets"`
	Pairs   []aisUsagePair   `json:"pairs"` // fallback if older version embeds pairs
}

type aisUsagePairsResp struct {
	Pairs []aisUsagePair `json:"pairs"`
}

var errAISUnauthorized = errors.New("ais unauthorized")
var errAISLoginBackoff = errors.New("ais login backoff")

func (c *AISCollector) loadEnvIfNeeded() {
	if c.cfg.EnvFile == "" {
		return
	}
	info, err := os.Stat(c.cfg.EnvFile)
	if err != nil {
		return
	}
	c.envMu.Lock()
	defer c.envMu.Unlock()
	if info.ModTime().Equal(c.envMtime) {
		return
	}
	f, err := os.Open(c.cfg.EnvFile)
	if err != nil {
		return
	}
	defer f.Close()

	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		kv[k] = v
	}

	en := strings.ToLower(strings.TrimSpace(kv["ADMIN_AUTH_ENABLED"]))
	newEnabled := en == "true" || en == "1" || en == "yes"
	newUser := strings.TrimSpace(kv["ADMIN_USERNAME"])
	newPass := kv["ADMIN_PASSWORD"]

	// If credentials changed, reset cached cookie
	if newEnabled != c.authEnabled || newUser != c.authUsername || newPass != c.authPassword {
		c.sessMu.Lock()
		c.sessionCookie = nil
		c.sessionExpiry = time.Time{}
		c.lastLoginFail = time.Time{}
		c.failCount = 0
		c.sessMu.Unlock()
	}

	c.authEnabled = newEnabled
	c.authUsername = newUser
	c.authPassword = newPass
	c.envMtime = info.ModTime()
}

func (c *AISCollector) loadQuotas() (*QuotasFile, error) {
	info, err := os.Stat(c.cfg.QuotasFile)
	if err != nil {
		c.quotaMu.Lock()
		cached := c.quotas
		c.quotaMu.Unlock()
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}
	c.quotaMu.Lock()
	defer c.quotaMu.Unlock()
	if c.quotas != nil && info.ModTime().Equal(c.quotaMtime) && info.Size() == c.quotaSize {
		return c.quotas, nil
	}
	raw, err := os.ReadFile(c.cfg.QuotasFile)
	if err != nil {
		if c.quotas != nil {
			return c.quotas, nil
		}
		return nil, err
	}
	var qf QuotasFile
	if err := json.Unmarshal(raw, &qf); err != nil {
		log.Printf("[ais] quotas.json reload failed, keeping previous config: %v", err)
		if c.quotas != nil {
			return c.quotas, nil
		}
		return nil, err
	}
	c.quotas = &qf
	c.quotaMtime = info.ModTime()
	c.quotaSize = info.Size()
	return c.quotas, nil
}

func (c *AISCollector) ensureLogin(ctx context.Context, force bool) error {
	c.envMu.Lock()
	enabled := c.authEnabled
	username := c.authUsername
	password := c.authPassword
	c.envMu.Unlock()

	if !enabled {
		return nil
	}

	c.sessMu.Lock()
	defer c.sessMu.Unlock()

	now := time.Now()
	if !force && c.sessionCookie != nil && now.Add(10*time.Minute).Before(c.sessionExpiry) {
		return nil
	}

	// Enforce login failure backoff (at least 60s, up to 300s) to never hit 429 rate limit
	if !c.lastLoginFail.IsZero() {
		backoff := 60 * time.Second
		if c.failCount >= 3 {
			backoff = 300 * time.Second
		} else if c.failCount == 2 {
			backoff = 120 * time.Second
		}
		if now.Sub(c.lastLoginFail) < backoff {
			return errAISLoginBackoff
		}
	}

	payload, err := json.Marshal(map[string]any{
		"username": username,
		"password": password,
		"remember": false,
	})
	if err != nil {
		return err
	}

	loginURL := strings.TrimRight(c.cfg.BaseURL, "/") + "/api/auth/login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loginURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately DO NOT set Origin/Referer headers to avoid same-origin mismatch

	resp, err := c.client.Do(req)
	if err != nil {
		c.lastLoginFail = now
		c.failCount++
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.lastLoginFail = now
		c.failCount++
		c.sessionCookie = nil
		c.sessionExpiry = time.Time{}
		return fmt.Errorf("ais login failed (http %d)", resp.StatusCode)
	}

	var chosen *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == "aistudio_admin" {
			chosen = ck
			break
		}
		if chosen == nil {
			chosen = ck
		}
	}
	if chosen == nil {
		c.lastLoginFail = now
		c.failCount++
		return errors.New("ais login did not return session cookie")
	}

	exp := now.Add(11 * time.Hour)
	if !chosen.Expires.IsZero() && chosen.Expires.After(now) {
		exp = chosen.Expires
	} else if chosen.MaxAge > 0 {
		exp = now.Add(time.Duration(chosen.MaxAge) * time.Second)
	}

	c.sessionCookie = chosen
	c.sessionExpiry = exp
	c.lastLoginFail = time.Time{}
	c.failCount = 0
	return nil
}

func (c *AISCollector) getJSON(ctx context.Context, path string, dst any) (int, error) {
	if err := c.ensureLogin(ctx, false); err != nil {
		return 0, err
	}
	code, err := c.doGetJSONOnce(ctx, path, dst)
	if errors.Is(err, errAISUnauthorized) {
		// Retry login at most once
		c.sessMu.Lock()
		c.sessionCookie = nil
		c.sessionExpiry = time.Time{}
		c.sessMu.Unlock()
		if lErr := c.ensureLogin(ctx, true); lErr != nil {
			return code, lErr
		}
		return c.doGetJSONOnce(ctx, path, dst)
	}
	return code, err
}

func (c *AISCollector) doGetJSONOnce(ctx context.Context, path string, dst any) (int, error) {
	reqURL := strings.TrimRight(c.cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, err
	}

	c.sessMu.Lock()
	ck := c.sessionCookie
	c.sessMu.Unlock()
	if ck != nil {
		req.AddCookie(ck)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return resp.StatusCode, errAISUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("ais http %d", resp.StatusCode)
	}
	if dst != nil {
		if err := json.Unmarshal(raw, dst); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func computeAISCycle(now time.Time, qf *QuotasFile) (time.Time, time.Time) {
	tzName := "Asia/Shanghai"
	resetHour, resetMin := 16, 0
	if qf != nil {
		if strings.TrimSpace(qf.ResetTimezone) != "" {
			tzName = strings.TrimSpace(qf.ResetTimezone)
		}
		if qf.ResetHour >= 0 && qf.ResetHour < 24 {
			resetHour = qf.ResetHour
		}
		if qf.ResetMinute >= 0 && qf.ResetMinute < 60 {
			resetMin = qf.ResetMinute
		}
	}
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	localNow := now.In(loc)
	todayReset := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), resetHour, resetMin, 0, 0, loc)
	var cycleStart time.Time
	if !localNow.Before(todayReset) {
		cycleStart = todayReset
	} else {
		cycleStart = todayReset.Add(-24 * time.Hour)
	}
	nextReset := cycleStart.Add(24 * time.Hour)
	return cycleStart, nextReset
}

func (c *AISCollector) Collect(ctx context.Context) AIStudioPool {
	now := time.Now()
	nowMs := now.UnixMilli()

	c.loadEnvIfNeeded()
	qf, qErr := c.loadQuotas()

	cycleStart, nextReset := computeAISCycle(now, qf)

	pool := AIStudioPool{
		UpdatedAtMs:   nowMs,
		CycleStartMs:  cycleStart.UnixMilli(),
		NextResetMs:   nextReset.UnixMilli(),
		TierCounts:    map[string]int{},
		HourlyBuckets: []AIStudioHourlyBucket{},
		Models:        []AIStudioModelSummary{},
		Accounts:      []AIStudioAccount{},
	}
	if qf != nil && qf.RuleNote != "" {
		pool.ResetRuleNote = qf.RuleNote
	} else {
		pool.ResetRuleNote = "每日北京时间 16:00 重置；次数与 Token 任一用尽即视为该账号该模型耗尽。"
	}
	if qErr != nil {
		pool.Online = false
		pool.Status = "offline"
		pool.StatusMessage = "配额配置文件 quotas.json 不可读"
		return pool
	}

	// 1. Fetch status, accounts, cooldowns
	var statusResp aisStatusResp
	if code, err := c.getJSON(ctx, "/api/status", &statusResp); err != nil {
		pool.Online = false
		pool.Status = "offline"
		pool.StatusMessage = formatAISPublicError(code, err)
		log.Printf("[ais] /api/status failed (code=%d): %v", code, err)
		return pool
	}

	var accResp aisAccountsResp
	if code, err := c.getJSON(ctx, "/api/accounts", &accResp); err != nil {
		pool.Online = false
		pool.Status = "offline"
		pool.StatusMessage = formatAISPublicError(code, err)
		log.Printf("[ais] /api/accounts failed (code=%d): %v", code, err)
		return pool
	}

	var cdResp aisCooldownsResp
	if code, err := c.getJSON(ctx, "/api/cooldowns", &cdResp); err != nil {
		log.Printf("[ais] /api/cooldowns warning (code=%d): %v", code, err)
	}

	// 2. Fetch current cycle usage for playground + pairs
	queryTo := now
	if queryTo.Sub(cycleStart) < 2*time.Second {
		queryTo = cycleStart.Add(2 * time.Second)
	}
	fromStr := url.QueryEscape(cycleStart.UTC().Format(time.RFC3339))
	toStr := url.QueryEscape(queryTo.UTC().Format(time.RFC3339))
	tzStr := url.QueryEscape("Asia/Shanghai")

	var pgUsage aisUsageResp
	if code, err := c.getJSON(ctx, fmt.Sprintf("/api/usage?from=%s&to=%s&channel=playground&tz=%s&bucket=3600", fromStr, toStr, tzStr), &pgUsage); err != nil {
		log.Printf("[ais] playground usage warning (code=%d): %v", code, err)
	}

	var pairsResp aisUsagePairsResp
	pairsCode, pairsErr := c.getJSON(ctx, fmt.Sprintf("/api/usage/pairs?from=%s&to=%s&channel=playground", fromStr, toStr), &pairsResp)
	pairs := pairsResp.Pairs
	if pairsErr != nil {
		if pairsCode == http.StatusNotFound && len(pgUsage.Pairs) > 0 {
			pairs = pgUsage.Pairs
		} else {
			log.Printf("[ais] playground usage pairs warning (code=%d): %v", pairsCode, pairsErr)
		}
	}

	// 3. Filter enabled accounts & assign deterministic anonymized code names
	type internalAISAcc struct {
		raw      aisRawAccount
		tier     string
		publicID string
		codeName string
	}

	var enabled []*internalAISAcc
	for _, a := range accResp.Accounts {
		if !a.Enabled {
			continue
		}
		tier := normalizeAISTier(parseRawStringOrInt(a.BenefitTier))
		enabled = append(enabled, &internalAISAcc{
			raw:  a,
			tier: tier,
		})
	}

	sort.Slice(enabled, func(i, j int) bool {
		ri, rj := tierSortRank(enabled[i].tier), tierSortRank(enabled[j].tier)
		if ri != rj {
			return ri < rj
		}
		return stableHashHex(enabled[i].raw.ID) < stableHashHex(enabled[j].raw.ID)
	})

	tierSeq := map[string]int{}
	accByKey := make(map[string]*internalAISAcc, len(enabled)*2)
	for _, a := range enabled {
		tierSeq[a.tier]++
		n := tierSeq[a.tier]
		a.codeName = fmt.Sprintf("AIS-%s #%d", a.tier, n)
		a.publicID = fmt.Sprintf("ais-%s-%d", strings.ToLower(a.tier), n)
		pool.TierCounts[a.tier]++

		if id := strings.TrimSpace(a.raw.ID); id != "" {
			accByKey[id] = a
		}
		if lbl := strings.TrimSpace(a.raw.Label); lbl != "" {
			if _, exists := accByKey[lbl]; !exists {
				accByKey[lbl] = a
			}
		}
	}

	// 4. Index Playground usage pairs by (account publicID, modelID)
	type usageKey struct {
		accID   string
		modelID string
	}
	pairUsage := map[usageKey]aisUsageStats{}
	accTotalUsedReq := map[string]int64{}
	accTotalUsedTok := map[string]int64{}

	for _, p := range pairs {
		acc := accByKey[strings.TrimSpace(p.Account)]
		if acc == nil {
			continue
		}
		modelID := normalizeModelID(p.Model)
		k := usageKey{accID: acc.publicID, modelID: modelID}
		prev := pairUsage[k]
		prev.Requests += p.Requests
		prev.TotalTokens += p.TotalTokens
		pairUsage[k] = prev

		accTotalUsedReq[acc.publicID] += p.Requests
		accTotalUsedTok[acc.publicID] += p.TotalTokens
	}

	// 5. Index active cooldowns (global "*" and playground channel only)
	globalCD := map[string]int64{}
	pgModelCD := map[usageKey]int64{}
	pgCooldownCount := 0

	for _, cd := range cdResp.Cooldowns {
		if !cd.Until.After(now) {
			continue
		}
		untilMs := cd.Until.UnixMilli()
		ch := strings.ToLower(strings.TrimSpace(cd.Channel))
		if ch != "" && ch != "*" && ch != "playground" {
			continue
		}
		pgCooldownCount++

		acc := accByKey[strings.TrimSpace(cd.AccountID)]
		if acc == nil {
			acc = accByKey[strings.TrimSpace(cd.AccountLabel)]
		}
		if acc == nil {
			continue
		}

		scope := strings.TrimSpace(cd.Scope)
		modelID := normalizeModelID(cd.ModelID)

		if scope == "*" || modelID == "*" || (modelID == "" && scope == "") {
			if untilMs > globalCD[acc.publicID] {
				globalCD[acc.publicID] = untilMs
			}
		}
		if modelID != "" && modelID != "*" {
			k := usageKey{accID: acc.publicID, modelID: modelID}
			if untilMs > pgModelCD[k] {
				pgModelCD[k] = untilMs
			}
		}
	}

	// 6. Per-account & per-model bottleneck accounting
	modelAcc := make([]AIStudioModelSummary, len(qf.Models))
	for i, m := range qf.Models {
		var tiers []string
		for _, tName := range []string{"Ultra", "Pro", "Free"} {
			lim := m.Tiers[tName]
			if lim.Requests > 0 && lim.Tokens > 0 {
				tiers = append(tiers, tName)
			}
		}
		modelAcc[i] = AIStudioModelSummary{
			ID:            m.ID,
			Label:         m.Label,
			EligibleTiers: tiers,
		}
	}

	pool.AccountCounts.Enabled = len(enabled)
	for _, acc := range enabled {
		st := strings.ToLower(strings.TrimSpace(acc.raw.State))
		switch st {
		case "ready":
			pool.AccountCounts.Ready++
		case "busy":
			pool.AccountCounts.Busy++
		case "cooldown":
			pool.AccountCounts.Cooldown++
		case "auth_required":
			pool.AccountCounts.AuthRequired++
		default:
			pool.AccountCounts.Unavailable++
		}

		accUnhealthy := st == "auth_required" || st == "unavailable" || st == "restricted" || st == "disabled"
		gCDMs := globalCD[acc.publicID]

		pubAcc := AIStudioAccount{
			ID:               acc.publicID,
			CodeName:         acc.codeName,
			Tier:             acc.tier,
			State:            st,
			StateLabel:       aisAccountStateLabel(st),
			ActiveRequests:   acc.raw.ActiveRequests,
			GlobalCooldownMs: gCDMs,
			UsedRequests:     accTotalUsedReq[acc.publicID],
			UsedTokens:       accTotalUsedTok[acc.publicID],
			Models:           make([]AIStudioAccountModel, 0, len(qf.Models)),
		}

		for mi, m := range qf.Models {
			lim := m.Tiers[acc.tier]
			if lim.Requests <= 0 || lim.Tokens <= 0 {
				pubAcc.Models = append(pubAcc.Models, AIStudioAccountModel{
					ModelID:     m.ID,
					Status:      "ineligible",
					StatusLabel: "档位不支持",
				})
				continue
			}

			pubAcc.EligibleModelCnt++
			ms := &modelAcc[mi]
			ms.EligibleAccounts++
			ms.LimitRequests += lim.Requests
			ms.LimitTokens += lim.Tokens

			u := pairUsage[usageKey{accID: acc.publicID, modelID: m.ID}]
			usedReq := u.Requests
			usedTok := u.TotalTokens
			ms.UsedRequests += usedReq
			ms.UsedTokens += usedTok

			rawRemReq := lim.Requests - usedReq
			if rawRemReq < 0 {
				rawRemReq = 0
			}
			rawRemTok := lim.Tokens - usedTok
			if rawRemTok < 0 {
				rawRemTok = 0
			}

			cdUntil := gCDMs
			if mCD := pgModelCD[usageKey{accID: acc.publicID, modelID: m.ID}]; mCD > cdUntil {
				cdUntil = mCD
			}

			am := AIStudioAccountModel{
				ModelID:        m.ID,
				LimitRequests:  lim.Requests,
				UsedRequests:   usedReq,
				RawRemRequests: rawRemReq,
				LimitTokens:    lim.Tokens,
				UsedTokens:     usedTok,
				RawRemTokens:   rawRemTok,
				CooldownUntilMs: cdUntil,
			}

			// Core single-account bottleneck & circuit-breaker rule
			switch {
			case accUnhealthy:
				switch st {
				case "auth_required":
					am.Status = "auth_required"
					am.StatusLabel = "需重登"
				case "restricted":
					am.Status = "restricted"
					am.StatusLabel = "服务受限"
				default:
					am.Status = "unavailable"
					am.StatusLabel = "不可用"
				}
				am.EffectiveRemReq = 0
				am.EffectiveRemTokens = 0
				am.EffectivePercent = 0
				ms.UnavailableAccounts++
			case cdUntil > nowMs:
				am.Status = "cooldown"
				am.StatusLabel = "冷却中"
				am.EffectiveRemReq = 0
				am.EffectiveRemTokens = 0
				am.EffectivePercent = 0
				pubAcc.CoolingModelCnt++
				ms.CoolingAccounts++
				if ms.EarliestRecoveryMs == 0 || cdUntil < ms.EarliestRecoveryMs {
					ms.EarliestRecoveryMs = cdUntil
				}
			case rawRemReq == 0:
				am.Status = "exhausted_requests"
				am.StatusLabel = "次数耗尽"
				am.EffectiveRemReq = 0
				am.EffectiveRemTokens = 0
				am.EffectivePercent = 0
				ms.ExhaustedAccounts++
			case rawRemTok == 0:
				am.Status = "exhausted_tokens"
				am.StatusLabel = "Token耗尽"
				am.EffectiveRemReq = 0
				am.EffectiveRemTokens = 0
				am.EffectivePercent = 0
				ms.ExhaustedAccounts++
			default:
				am.Status = "available"
				am.StatusLabel = "可用"
				am.EffectiveRemReq = rawRemReq
				am.EffectiveRemTokens = rawRemTok
				reqPct := float64(rawRemReq) / float64(lim.Requests) * 100.0
				tokPct := float64(rawRemTok) / float64(lim.Tokens) * 100.0
				am.EffectivePercent = round2(math.Min(reqPct, tokPct))
				pubAcc.AvailableModelCnt++
				ms.AvailableAccounts++
				ms.EffectiveRemReq += rawRemReq
				ms.EffectiveRemTokens += rawRemTok
			}

			pubAcc.Models = append(pubAcc.Models, am)
		}

		pool.Accounts = append(pool.Accounts, pubAcc)
	}

	// Finalize per-model effective bottleneck percent
	for i := range modelAcc {
		ms := &modelAcc[i]
		if ms.LimitRequests > 0 && ms.LimitTokens > 0 && ms.AvailableAccounts > 0 {
			reqPct := float64(ms.EffectiveRemReq) / float64(ms.LimitRequests) * 100.0
			tokPct := float64(ms.EffectiveRemTokens) / float64(ms.LimitTokens) * 100.0
			ms.EffectivePercent = round2(clampPercent(math.Min(reqPct, tokPct)))
		} else {
			ms.EffectivePercent = 0
		}
	}

	pool.Models = modelAcc
	pool.Online = true
	pool.ServiceState = statusResp.State
	pool.ServiceReady = statusResp.Ready
	pool.ActiveReqs = statusResp.ActiveRequests
	if statusResp.Ready {
		pool.Status = "online"
	} else {
		pool.Status = "degraded"
		pool.StatusMessage = "服务未处于就绪态"
	}

	pool.Playground = toChannelSummary(pgUsage.Totals, pgCooldownCount)
	pool.HourlyBuckets = toHourlyBuckets(pgUsage.Buckets)
	return pool
}

func toHourlyBuckets(raw []aisUsageBucket) []AIStudioHourlyBucket {
	if len(raw) == 0 {
		return []AIStudioHourlyBucket{}
	}
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	out := make([]AIStudioHourlyBucket, 0, len(raw))
	for _, b := range raw {
		var tsMs int64
		label := ""
		if t, pErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(b.At)); pErr == nil {
			tsMs = t.UnixMilli()
			label = t.In(loc).Format("15:04")
		} else if t, pErr := time.Parse(time.RFC3339, strings.TrimSpace(b.At)); pErr == nil {
			tsMs = t.UnixMilli()
			label = t.In(loc).Format("15:04")
		}
		otherFailed := b.Failed - b.RateLimited
		if otherFailed < 0 {
			otherFailed = 0
		}
		out = append(out, AIStudioHourlyBucket{
			TimestampMs:     tsMs,
			Label:           label,
			Requests:        b.Requests,
			Succeeded:       b.Succeeded,
			Failed:          b.Failed,
			OtherFailed:     otherFailed,
			Canceled:        b.Canceled,
			RateLimited:     b.RateLimited,
			InputTokens:     b.InputTokens,
			ReasoningTokens: b.ReasoningTokens,
			ReplyTokens:     b.ReplyTokens,
			TotalTokens:     b.TotalTokens,
			FirstEventP50Ms: round2(b.FirstEvent.P50MS),
			FirstEventAvgMs: round2(b.FirstEvent.AvgMS),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimestampMs < out[j].TimestampMs })
	return out
}

func toChannelSummary(s aisUsageStats, cooldownCnt int) AIStudioChannelSummary {
	cs := AIStudioChannelSummary{
		Requests:        s.Requests,
		Succeeded:       s.Succeeded,
		Failed:          s.Failed,
		Canceled:        s.Canceled,
		RateLimited:     s.RateLimited,
		InputTokens:     s.InputTokens,
		ReplyTokens:     s.ReplyTokens,
		TotalTokens:     s.TotalTokens,
		FirstEventAvgMs: round2(s.FirstEvent.AvgMS),
		FirstEventP50Ms: round2(s.FirstEvent.P50MS),
		FirstEventP95Ms: round2(s.FirstEvent.P95MS),
		ActiveCooldowns: cooldownCnt,
	}
	if s.Requests > 0 {
		cs.SuccessRate = floatPtr(round2(clampPercent(float64(s.Succeeded) / float64(s.Requests) * 100.0)))
	}
	return cs
}

func normalizeModelID(m string) string {
	m = strings.TrimSpace(m)
	m = strings.TrimPrefix(m, "models/")
	return m
}

func aisAccountStateLabel(st string) string {
	switch st {
	case "ready":
		return "就绪"
	case "busy":
		return "繁忙"
	case "cooldown":
		return "冷却中"
	case "auth_required":
		return "需重登"
	case "restricted":
		return "受限"
	case "unavailable":
		return "不可用"
	case "disabled":
		return "已停用"
	default:
		if st == "" {
			return "未知"
		}
		return st
	}
}

func formatAISPublicError(code int, err error) string {
	if errors.Is(err, errAISLoginBackoff) {
		return "管理会话登录退避中"
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return "管理鉴权失败"
	}
	if code == http.StatusTooManyRequests {
		return "管理接口触发限流"
	}
	if code >= 500 {
		return fmt.Sprintf("服务返回异常 (HTTP %d)", code)
	}
	return "AIStudio2API 暂不可达"
}
