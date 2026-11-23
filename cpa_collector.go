package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type CPAConfig struct {
	AuthsDir        string
	PriorityDir     string
	LamplighterDir  string
	ContainerAuthDir string
	CPAMPBaseURL    string
	AdminKeyEnv     string
	AdminKeyFile    string
}

type CPACollector struct {
	cfg    CPAConfig
	client *http.Client

	keyMu         sync.Mutex
	lastKeyMtime  time.Time
	lastKeySize   int64
	lastKeyExists bool

	histFileMu    sync.Mutex
	histFileCache map[string]cachedLampFile

	histStoreMu sync.RWMutex
	histStore   *cpaHistoryStore
}

func NewCPACollector(cfg CPAConfig) *CPACollector {
	return &CPACollector{
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
		histFileCache: map[string]cachedLampFile{},
	}
}

// CheckSecretKeyChanged checks if the secret key file was created/updated since last check.
func (c *CPACollector) CheckSecretKeyChanged() bool {
	if strings.TrimSpace(c.cfg.AdminKeyEnv) != "" {
		return false
	}
	if c.cfg.AdminKeyFile == "" {
		return false
	}
	info, err := os.Stat(c.cfg.AdminKeyFile)
	c.keyMu.Lock()
	defer c.keyMu.Unlock()

	if err != nil {
		changed := c.lastKeyExists
		c.lastKeyExists = false
		c.lastKeyMtime = time.Time{}
		c.lastKeySize = 0
		return changed
	}
	changed := !c.lastKeyExists || !info.ModTime().Equal(c.lastKeyMtime) || info.Size() != c.lastKeySize
	c.lastKeyExists = true
	c.lastKeyMtime = info.ModTime()
	c.lastKeySize = info.Size()
	return changed
}

func (c *CPACollector) readAdminKey() string {
	if envKey := strings.TrimSpace(c.cfg.AdminKeyEnv); envKey != "" {
		return envKey
	}
	if c.cfg.AdminKeyFile == "" {
		return ""
	}
	raw, err := os.ReadFile(c.cfg.AdminKeyFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// Internal account representation; NEVER serialized to public API.
type internalCPAAccount struct {
	rowKey              string
	fileName            string
	provider            string
	email               string
	label               string
	projectID           string
	authIndex           string
	disabled            bool
	tier                string
	codeName            string
	publicID            string
	windows             []internalCPAWindow
	hasHistory          bool
	hasRecentBuckets    bool
	recentBuckets       []AntigravityRecentBucket
	recentWindowSuccess int64
	recentWindowFailure int64
	recentWindowRate    *float64
	history             AntigravityAccountHistory
}

const (
	cpampRecentBucketCount   = 20
	cpampRecentWindowMinutes = 200
	lampMaxHistoryDays       = 37 // cover 36d + TZ margin
	lampResetDriftSec        = 60
	lampResetJumpPercent     = 5.0
	lampDefaultStepSec       = 900 // 15 minutes
)

type lampParsedSample struct {
	AuthIndex string
	Window    string
	T         int64
	R         float64
	X         int64
}

type cachedLampFile struct {
	Size    int64
	ModTime time.Time
	Samples []lampParsedSample
}

type lampHistoryPoint struct {
	T int64
	R float64
	X int64
}

type cpaWindowHistory struct {
	Key             string
	Group           string
	Label           string
	WindowKind      string // "5h" | "7d"
	DurationSeconds int64
	Remaining       float64
	ResetSec        int64 // effective reset sec (0 if unopened full or expired)
	RawResetSec     int64
	ObservedSec     int64
	Points          []lampHistoryPoint
}

type cpaAccountHistoryStore struct {
	PublicID string
	Tier     string
	Windows  map[string]*cpaWindowHistory
}

type cpaHistoryStore struct {
	UpdatedAtSec int64
	Accounts     map[string]*cpaAccountHistoryStore
}

type internalCPAWindow struct {
	key              string
	label            string
	group            string
	providerWindowID string
	durationSeconds  int64
	modelScopeKey    string // "claude_gpt" | "gemini" | ""
	remainingPercent float64
	resetAtMs        int64
	observedAtMs     int64
	stale            bool
	hasCurrentUsage  bool
	currentRequests  int64
	currentTokens    int64
	currentCost      float64
	hasForecast      bool
	forecastBasis    string
	forecastTokens   int64
	forecastCost     float64
}

// Local file schemas
type rawAuthFile struct {
	Type      string `json:"type"`
	Provider  string `json:"provider"`
	Disabled  bool   `json:"disabled"`
	Email     string `json:"email"`
	ProjectID string `json:"project_id"`
}

type rawLamplighterState struct {
	Groups map[string]rawLampGroup `json:"groups"`
	Plans  map[string]rawLampPlan  `json:"plans"`
}

type rawLampGroup struct {
	ActiveWindows []string                 `json:"active_windows"`
	Label         string                   `json:"label"`
	LastSeen      int64                    `json:"last_seen"`
	Windows       map[string]rawLampWindow `json:"windows"`
}

type rawLampWindow struct {
	Severity  string  `json:"severity"`
	Remaining float64 `json:"remaining"`
	Reset     string  `json:"reset"`
	LastSeen  int64   `json:"last_seen"`
	Long      bool    `json:"long"`
}

type rawLampPlan struct {
	Plan    string `json:"plan"`
	Checked int64  `json:"checked"`
}

type rawLampHistoryLine struct {
	Kind      string  `json:"k"`
	Timestamp int64   `json:"t"`
	Group     string  `json:"g"`
	Window    string  `json:"w"`
	Remaining float64 `json:"r"`
	Reset     int64   `json:"x"`
	Status    string  `json:"s"`
}

type rawPriorityState struct {
	Entries   map[string]rawPriorityEntry `json:"entries"`
	PlanTiers map[string]rawPriorityTier  `json:"plan_tiers"`
}

type rawPriorityEntry struct {
	Provider             string  `json:"provider"`
	ModelGroup           string  `json:"model_group"`
	AuthIndex            string  `json:"auth_index"`
	ObservedAt           string  `json:"observed_at"`
	ShortWindowResetAt   string  `json:"short_window_reset_at"`
	ShortWindowRemaining float64 `json:"short_window_remaining"`
	LongWindowResetAt    string  `json:"long_window_reset_at"`
	LongWindowRemaining  float64 `json:"long_window_remaining"`
	ArgonWindowResetAt   string  `json:"argon_window_reset_at"`
	ArgonWindowRemaining float64 `json:"argon_window_remaining"`
	PlanType             string  `json:"plan_type"`
}

type rawPriorityTier struct {
	PlanType   string `json:"plan_type"`
	ObservedAt string `json:"observed_at"`
}

// CPAMP API schemas
type cpampAuthFilesResp struct {
	Files []cpampAuthFile `json:"files"`
}

type cpampRecentBucket struct {
	Time    string `json:"time"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

type cpampAuthFile struct {
	Name           string              `json:"name"`
	ID             string              `json:"id"`
	Provider       string              `json:"provider"`
	Type           string              `json:"type"`
	Email          string              `json:"email"`
	Account        string              `json:"account"`
	Label          string              `json:"label"`
	ProjectID      string              `json:"project_id"`
	AuthIndex      json.RawMessage     `json:"auth_index"`
	Disabled       bool                `json:"disabled"`
	Status         string              `json:"status"`
	RecentRequests []cpampRecentBucket `json:"recent_requests"`
}

type cpampHistoryReq struct {
	Accounts []cpampAccountTarget `json:"accounts"`
	CatchUp  bool                 `json:"catch_up"`
}

type cpampAccountTarget struct {
	RowKey                string `json:"row_key"`
	AccountSnapshot       string `json:"account_snapshot,omitempty"`
	AuthLabelSnapshot     string `json:"auth_label_snapshot,omitempty"`
	AuthFileSnapshot      string `json:"auth_file_snapshot,omitempty"`
	AuthProviderSnapshot  string `json:"auth_provider_snapshot,omitempty"`
	AuthProjectIDSnapshot string `json:"auth_project_id_snapshot,omitempty"`
	AuthIndex             string `json:"auth_index,omitempty"`
	Source                string `json:"source,omitempty"`
}

type cpampHistoryResp struct {
	Items         []cpampHistoryItem `json:"items"`
	GeneratedAtMs int64              `json:"generated_at_ms"`
}

type cpampHistoryItem struct {
	RowKey         string               `json:"row_key"`
	Matched        bool                 `json:"matched"`
	TotalRequests  int64                `json:"total_requests"`
	SuccessCalls   int64                `json:"success_calls"`
	FailureCalls   int64                `json:"failure_calls"`
	TotalTokens    int64                `json:"total_tokens"`
	TotalCost      float64              `json:"total_cost"`
	SuccessRate    *float64             `json:"success_rate"`
	SyncStatus     string               `json:"sync_status"`
	LatestRequest  *cpampRecentRequest  `json:"latest_request"`
	RecentRequests []cpampRecentRequest `json:"recent_requests"`
}

type cpampRecentRequest struct {
	Failed      *bool  `json:"failed"`
	Success     *bool  `json:"success"`
	OK          *bool  `json:"ok"`
	StatusCode  int    `json:"status_code"`
	Status      string `json:"status"`
	TimestampMs int64  `json:"timestamp_ms"`
	Timestamp   int64  `json:"timestamp"`
}

type cpampWindowUsageReq struct {
	Windows []cpampWindowTarget `json:"windows"`
}

type cpampModelScope struct {
	Kind     string `json:"kind"`
	Key      string `json:"key,omitempty"`
	Complete bool   `json:"complete"`
}

type cpampWindowTarget struct {
	RequestKey            string           `json:"request_key"`
	RowKey                string           `json:"row_key"`
	WindowKey             string           `json:"window_key"`
	ProviderWindowID      string           `json:"provider_window_id"`
	Period                string           `json:"period"` // current | previous | previous_equal_range
	FromMs                int64            `json:"from_ms"`
	ToMs                  int64            `json:"to_ms"`
	ModelScope            *cpampModelScope `json:"model_scope,omitempty"`
	AccountSnapshot       string           `json:"account_snapshot,omitempty"`
	AuthLabelSnapshot     string           `json:"auth_label_snapshot,omitempty"`
	AuthFileSnapshot      string           `json:"auth_file_snapshot,omitempty"`
	AuthProviderSnapshot  string           `json:"auth_provider_snapshot,omitempty"`
	AuthProjectIDSnapshot string           `json:"auth_project_id_snapshot,omitempty"`
	AuthIndex             string           `json:"auth_index,omitempty"`
	Source                string           `json:"source,omitempty"`
}

type cpampWindowUsageResp struct {
	Items []cpampWindowUsageItem `json:"items"`
}

type cpampWindowUsageItem struct {
	RequestKey       string  `json:"request_key"`
	RowKey           string  `json:"row_key"`
	WindowKey        string  `json:"window_key"`
	ProviderWindowID string  `json:"provider_window_id"`
	Period           string  `json:"period"`
	FromMs           int64   `json:"from_ms"`
	ToMs             int64   `json:"to_ms"`
	Matched          bool    `json:"matched"`
	TotalRequests    int64   `json:"total_requests"`
	TotalTokens      int64   `json:"total_tokens"`
	TotalCost        float64 `json:"total_cost"`
	LastSeenMs       int64   `json:"last_seen_ms"`
	ScopeMatchStatus string  `json:"scope_match_status"`
}

func (c *CPACollector) Collect(ctx context.Context) AntigravityPool {
	now := time.Now()
	nowMs := now.UnixMilli()

	pool := AntigravityPool{
		UpdatedAtMs: nowMs,
		Summary: AntigravityPoolSummary{
			TierCounts: map[string]int{},
		},
		Accounts: []AntigravityAccount{},
	}

	// 1. Read local auth files (enabled only)
	accounts, err := c.loadEnabledAuths()
	if err != nil {
		pool.Online = false
		pool.Status = "offline"
		pool.StatusMessage = "无法读取 Antigravity 凭证目录"
		pool.CPAMPStatus = "error"
		return pool
	}

	// 2. Read local lamplighter & priority state files
	lampState := c.loadLamplighterState()
	prioState := c.loadPriorityState()

	// 3. Optional CPAMP enrichment (graceful degradation if key missing or error)
	adminKey := c.readAdminKey()
	cpampAuthByFile := map[string]cpampAuthFile{}
	if adminKey == "" {
		pool.CPAMPStatus = "no_key"
		pool.CPAMPMessage = "待接入密钥"
	} else {
		authFiles, statusCode, fetchErr := c.fetchCPAMPAuthFiles(ctx, adminKey)
		if fetchErr != nil {
			if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
				pool.CPAMPStatus = "unauthorized"
				pool.CPAMPMessage = "密钥校验失败"
			} else {
				pool.CPAMPStatus = "error"
				pool.CPAMPMessage = "CPAMP 服务暂不可达"
				log.Printf("[cpa] auth-files fetch failed (status=%d)", statusCode)
			}
		} else {
			pool.CPAMPStatus = "ok"
			for _, f := range authFiles {
				if name := strings.TrimSpace(f.Name); name != "" {
					cpampAuthByFile[name] = f
				}
			}
		}
	}

	// Reconcile auth_index / disabled / label from CPAMP auth-files if available
	filtered := make([]*internalCPAAccount, 0, len(accounts))
	for _, acc := range accounts {
		if cf, ok := cpampAuthByFile[acc.fileName]; ok {
			if cf.Disabled {
				continue
			}
			if idx := parseRawStringOrInt(cf.AuthIndex); idx != "" {
				acc.authIndex = idx
			}
			if cf.Label != "" {
				acc.label = cf.Label
			}
			if acc.email == "" {
				if cf.Email != "" {
					acc.email = cf.Email
				} else if cf.Account != "" {
					acc.email = cf.Account
				}
			}
			if acc.projectID == "" && cf.ProjectID != "" {
				acc.projectID = cf.ProjectID
			}
			// Extract CPAMP native 20x10m recent request buckets directly from auth-files (zero extra request)
			buckets, totOk, totFail, rate := normalizeCPAMPRecentBuckets(cf.RecentRequests)
			acc.hasRecentBuckets = true
			acc.recentBuckets = buckets
			acc.recentWindowSuccess = totOk
			acc.recentWindowFailure = totFail
			acc.recentWindowRate = rate
		}
		if acc.label == "" {
			acc.label = acc.email
		}
		// Resolve plan tier from priority state or lamplighter plans
		acc.tier = resolveCPATier(acc.authIndex, prioState, lampState)
		filtered = append(filtered, acc)
	}

	// Stable sort: tier (Ultra -> Pro -> Free -> Unknown), then sha256(authIndex)
	sort.Slice(filtered, func(i, j int) bool {
		ri, rj := tierSortRank(filtered[i].tier), tierSortRank(filtered[j].tier)
		if ri != rj {
			return ri < rj
		}
		return stableHashHex(filtered[i].authIndex) < stableHashHex(filtered[j].authIndex)
	})

	// Assign stable anonymous code names
	tierSeq := map[string]int{}
	for idx, acc := range filtered {
		tierSeq[acc.tier]++
		n := tierSeq[acc.tier]
		acc.codeName = fmt.Sprintf("AG-%s #%d", acc.tier, n)
		acc.publicID = fmt.Sprintf("ag-%s-%d", strings.ToLower(acc.tier), n)
		acc.rowKey = fmt.Sprintf("ag-row-%d", idx)
	}

	// 4. Populate quota windows from lamplighter (+ priority fallback)
	for _, acc := range filtered {
		acc.windows = buildAccountWindows(acc.authIndex, acc.tier, lampState, prioState, nowMs)
	}

	// 5. Refresh 36-day lamplighter history store in memory (archived daily files cached by size+mtime)
	c.refreshHistoryStore(filtered, now)

	// 6. Query CPAMP account-history & account-window-usage if key is active
	if adminKey != "" && pool.CPAMPStatus == "ok" && len(filtered) > 0 {
		if statusCode, hErr := c.enrichHistory(ctx, adminKey, filtered); hErr != nil {
			if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
				pool.CPAMPStatus = "unauthorized"
				pool.CPAMPMessage = "密钥校验失败"
			} else {
				pool.CPAMPStatus = "error"
				pool.CPAMPMessage = "历史统计拉取异常"
				log.Printf("[cpa] account-history failed (status=%d)", statusCode)
			}
		}
		if pool.CPAMPStatus == "ok" {
			if statusCode, wErr := c.enrichWindowUsage(ctx, adminKey, filtered, nowMs); wErr != nil {
				log.Printf("[cpa] account-window-usage warning (status=%d)", statusCode)
			}
		}
	}

	// 7. Build strictly sanitized public DTOs and summary
	winSum := map[string]float64{}
	winCnt := map[string]int{}
	winLabel := map[string]string{
		"claude_5h":     "Claude 5h",
		"claude_weekly": "Claude 周额度",
		"gemini_5h":     "Gemini 5h",
		"gemini_weekly": "Gemini 周额度",
		"argon_weekly":  "Argon 周额度",
	}

	var totalReq, totalSuccessCalls, totalTok int64
	var totalCost float64
	hasAnyHistory := false

	for _, acc := range filtered {
		pool.Summary.TierCounts[acc.tier]++

		if acc.hasRecentBuckets {
			acc.history.HasRecentBuckets = true
			acc.history.RecentWindowMinutes = cpampRecentWindowMinutes
			acc.history.RecentWindowSuccess = acc.recentWindowSuccess
			acc.history.RecentWindowFailure = acc.recentWindowFailure
			acc.history.RecentWindowRate = acc.recentWindowRate
			acc.history.RecentBuckets = acc.recentBuckets
		} else if acc.history.RecentBuckets == nil {
			acc.history.RecentBuckets = []AntigravityRecentBucket{}
		}
		if acc.history.RecentBars == nil {
			acc.history.RecentBars = []AntigravityRecentBar{}
		}

		if acc.history.Available {
			hasAnyHistory = true
			totalReq += acc.history.TotalRequests
			totalSuccessCalls += acc.history.SuccessCalls
			totalTok += acc.history.TotalTokens
			totalCost += acc.history.TotalCost
		} else if pool.CPAMPStatus != "ok" {
			acc.history.StatusNote = pool.CPAMPMessage
		}

		pubWindows := make([]AntigravityWindow, 0, len(acc.windows))
		allExhausted := len(acc.windows) > 0
		anyExhausted := false
		anyLow := false

		for _, w := range acc.windows {
			usedPct := clampPercent(100.0 - w.remainingPercent)
			pubWindows = append(pubWindows, AntigravityWindow{
				Key:              w.key,
				Label:            w.label,
				Group:            w.group,
				DurationSeconds:  w.durationSeconds,
				RemainingPercent: round3(w.remainingPercent),
				UsedPercent:      round3(usedPct),
				ResetAtMs:        w.resetAtMs,
				ObservedAtMs:     w.observedAtMs,
				Stale:            w.stale,
				HasCurrentUsage:  w.hasCurrentUsage,
				CurrentRequests:  w.currentRequests,
				CurrentTokens:    w.currentTokens,
				CurrentCost:      round2(w.currentCost),
				HasForecast:      w.hasForecast,
				ForecastBasis:    w.forecastBasis,
				ForecastTokens:   w.forecastTokens,
				ForecastCost:     round2(w.forecastCost),
			})
			winSum[w.key] += w.remainingPercent
			winCnt[w.key]++

			if w.remainingPercent > 0.01 {
				allExhausted = false
			} else {
				anyExhausted = true
			}
			if w.remainingPercent < 20 {
				anyLow = true
			}
		}

		accStatus, accLabel := "available", "可用"
		if len(acc.windows) == 0 {
			accStatus, accLabel = "unknown", "待采样"
		} else if allExhausted {
			accStatus, accLabel = "exhausted", "已耗尽"
		} else if anyExhausted {
			accStatus, accLabel = "limited", "部分受限"
		} else if anyLow {
			accStatus, accLabel = "limited", "余量偏低"
		}

		pool.Accounts = append(pool.Accounts, AntigravityAccount{
			ID:          acc.publicID,
			CodeName:    acc.codeName,
			Tier:        acc.tier,
			Status:      accStatus,
			StatusLabel: accLabel,
			History:     acc.history,
			Windows:     pubWindows,
			HasHistory:  acc.hasHistory,
		})
	}

	pool.Summary.ActiveAccounts = len(filtered)
	pool.Summary.TotalRequests = totalReq
	pool.Summary.TotalTokens = totalTok
	pool.Summary.TotalCost = round2(totalCost)
	if hasAnyHistory && totalReq > 0 {
		pool.Summary.WeightedSuccessRate = floatPtr(round2(float64(totalSuccessCalls) / float64(totalReq) * 100.0))
	}

	windowOrder := []string{"claude_5h", "claude_weekly", "gemini_5h", "gemini_weekly", "argon_weekly"}
	for _, wk := range windowOrder {
		if cnt := winCnt[wk]; cnt > 0 {
			pool.Summary.WindowAverages = append(pool.Summary.WindowAverages, AntigravityWindowAverage{
				Key:                 wk,
				Label:               winLabel[wk],
				AvgRemainingPercent: round2(winSum[wk] / float64(cnt)),
				AccountCount:        cnt,
			})
		}
	}

	pool.Online = true
	if pool.CPAMPStatus == "ok" {
		pool.Status = "online"
	} else {
		pool.Status = "degraded"
		pool.StatusMessage = pool.CPAMPMessage
	}
	return pool
}

func (c *CPACollector) loadEnabledAuths() ([]*internalCPAAccount, error) {
	entries, err := os.ReadDir(c.cfg.AuthsDir)
	if err != nil {
		return nil, err
	}
	var result []*internalCPAAccount
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(strings.ToLower(ent.Name()), ".json") {
			continue
		}
		rawBytes, rErr := os.ReadFile(filepath.Join(c.cfg.AuthsDir, ent.Name()))
		if rErr != nil {
			continue
		}
		var af rawAuthFile
		if jErr := json.Unmarshal(rawBytes, &af); jErr != nil {
			continue
		}
		// Strictly exclude disabled accounts
		if af.Disabled {
			continue
		}
		prov := strings.ToLower(strings.TrimSpace(af.Type))
		if prov == "" {
			prov = strings.ToLower(strings.TrimSpace(af.Provider))
		}
		if prov == "" {
			prov = "antigravity"
		}
		if prov != "antigravity" {
			continue
		}
		authIdx := computeCPAAuthIndex(prov, c.cfg.ContainerAuthDir, ent.Name())
		result = append(result, &internalCPAAccount{
			fileName:  ent.Name(),
			provider:  prov,
			email:     strings.TrimSpace(af.Email),
			projectID: strings.TrimSpace(af.ProjectID),
			authIndex: authIdx,
			disabled:  false,
		})
	}
	return result, nil
}

func computeCPAAuthIndex(provider, containerAuthDir, fileName string) string {
	dir := strings.TrimRight(strings.TrimSpace(containerAuthDir), "/")
	if dir == "" {
		dir = "/root/.cli-proxy-api"
	}
	key := fmt.Sprintf("%s:%s/%s", provider, dir, fileName)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

func (c *CPACollector) loadLamplighterState() *rawLamplighterState {
	raw, err := os.ReadFile(filepath.Join(c.cfg.LamplighterDir, "state.json"))
	if err != nil {
		return nil
	}
	var st rawLamplighterState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

func (c *CPACollector) loadPriorityState() *rawPriorityState {
	raw, err := os.ReadFile(filepath.Join(c.cfg.PriorityDir, "state.json"))
	if err != nil {
		return nil
	}
	var st rawPriorityState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil
	}
	return &st
}

func resolveCPATier(authIndex string, prio *rawPriorityState, lamp *rawLamplighterState) string {
	if prio != nil {
		if pt, ok := prio.PlanTiers[authIndex]; ok && pt.PlanType != "" {
			if t := normalizeAntigravityTier(pt.PlanType); t != "Unknown" {
				return t
			}
		}
		for _, suffix := range []string{"|model_group=gemini", "|model_group=claude_gpt"} {
			if ent, ok := prio.Entries[authIndex+suffix]; ok && ent.PlanType != "" {
				if t := normalizeAntigravityTier(ent.PlanType); t != "Unknown" {
					return t
				}
			}
		}
	}
	if lamp != nil {
		if lp, ok := lamp.Plans[authIndex]; ok && lp.Plan != "" {
			if t := normalizeAntigravityTier(lp.Plan); t != "Unknown" {
				return t
			}
		}
	}
	return "Unknown"
}

func parseRFC3339Ms(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

func buildAccountWindows(authIndex, tier string, lamp *rawLamplighterState, prio *rawPriorityState, nowMs int64) []internalCPAWindow {
	type winDef struct {
		key             string
		label           string
		group           string
		lampGroupSuffix string
		lampWinKey      string
		prioGroup       string
		prioField       string // short | long | argon
		durationSeconds int64
		modelScopeKey   string
		proOnly         bool
	}

	defs := []winDef{
		{
			key: "claude_5h", label: "Claude 5h", group: "claude",
			lampGroupSuffix: "claude-and-gpt-models", lampWinKey: "3p-5h",
			prioGroup: "claude_gpt", prioField: "short",
			durationSeconds: 5 * 3600, modelScopeKey: "claude_gpt",
		},
		{
			key: "claude_weekly", label: "Claude 周额度", group: "claude",
			lampGroupSuffix: "claude-and-gpt-models", lampWinKey: "3p-weekly",
			prioGroup: "claude_gpt", prioField: "long",
			durationSeconds: 7 * 86400, modelScopeKey: "claude_gpt",
		},
		{
			key: "gemini_5h", label: "Gemini 5h", group: "gemini",
			lampGroupSuffix: "gemini-models", lampWinKey: "gemini-5h",
			prioGroup: "gemini", prioField: "short",
			durationSeconds: 5 * 3600, modelScopeKey: "gemini",
		},
		{
			key: "gemini_weekly", label: "Gemini 周额度", group: "gemini",
			lampGroupSuffix: "gemini-models", lampWinKey: "gemini-weekly",
			prioGroup: "gemini", prioField: "long",
			durationSeconds: 7 * 86400, modelScopeKey: "gemini",
		},
		{
			key: "argon_weekly", label: "Argon 周额度", group: "argon",
			lampGroupSuffix: "gemini-4-argon", lampWinKey: "gemini-argon-wtu-weekly",
			prioGroup: "argon", prioField: "argon",
			durationSeconds: 7 * 86400, modelScopeKey: "",
			proOnly: true,
		},
	}

	isUltra := strings.EqualFold(tier, "Ultra")
	isPro := strings.EqualFold(tier, "Pro")

	var out []internalCPAWindow
	for _, d := range defs {
		// Ultra strictly has NO independent Argon window; only Pro accounts may show Argon if present.
		if d.proOnly {
			if isUltra || !isPro {
				continue
			}
		}

		var found bool
		var rem float64
		var resetMs, obsMs int64

		// Primary source: lamplighter/state.json (high precision float remaining)
		if lamp != nil {
			groupKey := fmt.Sprintf("antigravity:%s:antigravity:%s", authIndex, d.lampGroupSuffix)
			if grp, ok := lamp.Groups[groupKey]; ok {
				if lw, ok2 := grp.Windows[d.lampWinKey]; ok2 {
					found = true
					rem = clampPercent(lw.Remaining)
					resetMs = parseRFC3339Ms(lw.Reset)
					if lw.LastSeen > 0 {
						obsMs = lw.LastSeen * 1000
					} else if grp.LastSeen > 0 {
						obsMs = grp.LastSeen * 1000
					}
				}
			}
		}

		// Secondary complementary source: antigravity-priority/state.json
		if prio != nil {
			pKey := fmt.Sprintf("%s|model_group=%s", authIndex, d.prioGroup)
			if pe, ok := prio.Entries[pKey]; ok {
				pObs := parseRFC3339Ms(pe.ObservedAt)
				var pRem float64
				var pReset int64
				var pValid bool
				switch d.prioField {
				case "short":
					pReset = parseRFC3339Ms(pe.ShortWindowResetAt)
					pRem = clampPercent(pe.ShortWindowRemaining)
					pValid = pReset > 0 || pObs > 0
				case "long":
					pReset = parseRFC3339Ms(pe.LongWindowResetAt)
					pRem = clampPercent(pe.LongWindowRemaining)
					pValid = pReset > 0 || pObs > 0
				case "argon":
					pReset = parseRFC3339Ms(pe.ArgonWindowResetAt)
					pRem = clampPercent(pe.ArgonWindowRemaining)
					pValid = pReset > 0
				}
				if pValid {
					// Use priority only if lamplighter didn't have it, or priority is strictly much fresher (> 2 min)
					if !found || (pObs > obsMs+120_000 && pReset > 0) {
						found = true
						rem = pRem
						resetMs = pReset
						obsMs = pObs
					} else if resetMs == 0 && pReset > 0 {
						resetMs = pReset
					}
				}
			}
		}

		if !found {
			if d.proOnly {
				continue
			}
			continue
		}

		stale := resetMs > 0 && resetMs <= nowMs
		out = append(out, internalCPAWindow{
			key:              d.key,
			label:            d.label,
			group:            d.group,
			providerWindowID: d.lampWinKey,
			durationSeconds:  d.durationSeconds,
			modelScopeKey:    d.modelScopeKey,
			remainingPercent: rem,
			resetAtMs:        resetMs,
			observedAtMs:     obsMs,
			stale:            stale,
		})
	}
	return out
}

func sanitizeBucketTimeLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		if (r >= '0' && r <= '9') || r == ':' || r == '-' || r == ' ' || r == '/' || r == '–' {
			b.WriteRune(r)
		}
		if b.Len() >= 24 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func normalizeCPAMPRecentBuckets(raw []cpampRecentBucket) ([]AntigravityRecentBucket, int64, int64, *float64) {
	src := raw
	if len(src) > cpampRecentBucketCount {
		src = src[len(src)-cpampRecentBucketCount:]
	}
	emptyPad := cpampRecentBucketCount - len(src)
	out := make([]AntigravityRecentBucket, 0, cpampRecentBucketCount)
	for i := 0; i < emptyPad; i++ {
		out = append(out, AntigravityRecentBucket{})
	}
	var totOk, totFail int64
	for _, item := range src {
		okCnt := item.Success
		if okCnt < 0 {
			okCnt = 0
		}
		failCnt := item.Failed
		if failCnt < 0 {
			failCnt = 0
		}
		totOk += okCnt
		totFail += failCnt
		out = append(out, AntigravityRecentBucket{
			Time:    sanitizeBucketTimeLabel(item.Time),
			Success: okCnt,
			Failed:  failCnt,
		})
	}
	var rate *float64
	if total := totOk + totFail; total > 0 {
		rate = floatPtr(round2(clampPercent(float64(totOk) / float64(total) * 100.0)))
	}
	return out, totOk, totFail, rate
}

func effectiveResetSec(tSec int64, rem float64, rawResetSec int64, durationSec int64) int64 {
	if rawResetSec <= 0 || durationSec <= 0 {
		return 0
	}
	// Unopened full quota window: upstream slides reset = t + windowDuration on every poll.
	if rem >= 99.999 && (rawResetSec-tSec) >= durationSec-120 {
		return 0
	}
	return rawResetSec
}

func (c *CPACollector) loadLamplighterHistoryFiles(histDir string) ([]lampParsedSample, error) {
	entries, err := os.ReadDir(histDir)
	if err != nil {
		return nil, err
	}
	var jsonlFiles []string
	for _, ent := range entries {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".jsonl") {
			jsonlFiles = append(jsonlFiles, ent.Name())
		}
	}
	sort.Strings(jsonlFiles)
	if len(jsonlFiles) > lampMaxHistoryDays {
		jsonlFiles = jsonlFiles[len(jsonlFiles)-lampMaxHistoryDays:]
	}

	keepSet := make(map[string]struct{}, len(jsonlFiles))
	for _, name := range jsonlFiles {
		keepSet[name] = struct{}{}
	}

	c.histFileMu.Lock()
	defer c.histFileMu.Unlock()

	if c.histFileCache == nil {
		c.histFileCache = map[string]cachedLampFile{}
	}
	for k := range c.histFileCache {
		if _, ok := keepSet[k]; !ok {
			delete(c.histFileCache, k)
		}
	}

	var all []lampParsedSample
	for _, name := range jsonlFiles {
		fullPath := filepath.Join(histDir, name)
		info, sErr := os.Stat(fullPath)
		if sErr != nil {
			continue
		}
		if cached, ok := c.histFileCache[name]; ok && cached.Size == info.Size() && cached.ModTime.Equal(info.ModTime()) {
			all = append(all, cached.Samples...)
			continue
		}

		f, oErr := os.Open(fullPath)
		if oErr != nil {
			continue
		}
		var parsed []lampParsedSample
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var rec rawLampHistoryLine
			if jErr := json.Unmarshal(line, &rec); jErr != nil {
				continue
			}
			if rec.Kind != "s" || rec.Timestamp <= 0 || rec.Group == "" || rec.Window == "" {
				continue
			}
			// Group format: antigravity:<auth_index>:antigravity:<group_slug>
			parts := strings.Split(rec.Group, ":")
			if len(parts) < 2 || parts[1] == "" {
				continue
			}
			parsed = append(parsed, lampParsedSample{
				AuthIndex: parts[1],
				Window:    rec.Window,
				T:         rec.Timestamp,
				R:         round3(clampPercent(rec.Remaining)),
				X:         rec.Reset,
			})
		}
		_ = f.Close()

		c.histFileCache[name] = cachedLampFile{
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Samples: parsed,
		}
		all = append(all, parsed...)
	}
	return all, nil
}

func (c *CPACollector) refreshHistoryStore(accounts []*internalCPAAccount, now time.Time) {
	nowSec := now.Unix()
	store := &cpaHistoryStore{
		UpdatedAtSec: nowSec,
		Accounts:     make(map[string]*cpaAccountHistoryStore, len(accounts)),
	}
	if len(accounts) == 0 {
		c.histStoreMu.Lock()
		c.histStore = store
		c.histStoreMu.Unlock()
		return
	}

	type winMetaDef struct {
		key             string
		group           string
		label           string
		windowKind      string
		durationSeconds int64
	}
	byRawWindow := map[string]winMetaDef{
		"gemini-5h":               {key: "gemini_5h", group: "gemini", label: "Gemini", windowKind: "5h", durationSeconds: 5 * 3600},
		"3p-5h":                   {key: "claude_5h", group: "claude", label: "Claude / GPT", windowKind: "5h", durationSeconds: 5 * 3600},
		"gemini-weekly":           {key: "gemini_weekly", group: "gemini", label: "Gemini", windowKind: "7d", durationSeconds: 7 * 86400},
		"3p-weekly":               {key: "claude_weekly", group: "claude", label: "Claude / GPT", windowKind: "7d", durationSeconds: 7 * 86400},
		"gemini-argon-wtu-weekly": {key: "argon_weekly", group: "argon", label: "Gemini 4 Argon", windowKind: "7d", durationSeconds: 7 * 86400},
	}
	byKey := map[string]winMetaDef{}
	for _, m := range byRawWindow {
		byKey[m.key] = m
	}

	accByIdx := make(map[string]*internalCPAAccount, len(accounts))
	for _, acc := range accounts {
		accByIdx[acc.authIndex] = acc
		store.Accounts[acc.publicID] = &cpaAccountHistoryStore{
			PublicID: acc.publicID,
			Tier:     acc.tier,
			Windows:  map[string]*cpaWindowHistory{},
		}
	}

	histDir := filepath.Join(c.cfg.LamplighterDir, "history")
	samples, _ := c.loadLamplighterHistoryFiles(histDir)
	cutoffSec := nowSec - int64(lampMaxHistoryDays)*86400

	collected := make(map[string]map[string][]lampHistoryPoint, len(accounts))
	for _, smp := range samples {
		if smp.T < cutoffSec {
			continue
		}
		acc, ok := accByIdx[smp.AuthIndex]
		if !ok {
			continue
		}
		if smp.Window == "gemini-argon-wtu-weekly" {
			if strings.EqualFold(acc.tier, "Ultra") || !strings.EqualFold(acc.tier, "Pro") {
				continue
			}
		}
		meta, ok := byRawWindow[smp.Window]
		if !ok {
			continue
		}
		if collected[acc.publicID] == nil {
			collected[acc.publicID] = map[string][]lampHistoryPoint{}
		}
		collected[acc.publicID][meta.key] = append(collected[acc.publicID][meta.key], lampHistoryPoint{
			T: smp.T,
			R: smp.R,
			X: smp.X,
		})
	}

	for _, acc := range accounts {
		accStore := store.Accounts[acc.publicID]
		liveByKey := make(map[string]internalCPAWindow, len(acc.windows))
		for _, w := range acc.windows {
			liveByKey[w.key] = w
		}

		// Union of live windows + history windows
		winKeys := map[string]struct{}{}
		for k := range liveByKey {
			winKeys[k] = struct{}{}
		}
		for k := range collected[acc.publicID] {
			winKeys[k] = struct{}{}
		}

		for wk := range winKeys {
			meta, ok := byKey[wk]
			if !ok {
				continue
			}
			pts := collected[acc.publicID][wk]
			if len(pts) > 1 {
				sort.Slice(pts, func(i, j int) bool { return pts[i].T < pts[j].T })
				dedup := make([]lampHistoryPoint, 0, len(pts))
				for _, p := range pts {
					if len(dedup) > 0 && dedup[len(dedup)-1].T == p.T {
						dedup[len(dedup)-1] = p
					} else {
						dedup = append(dedup, p)
					}
				}
				pts = dedup
			}

			wh := &cpaWindowHistory{
				Key:             meta.key,
				Group:           meta.group,
				Label:           meta.label,
				WindowKind:      meta.windowKind,
				DurationSeconds: meta.durationSeconds,
				Points:          pts,
			}

			if lw, hasLive := liveByKey[wk]; hasLive {
				wh.Remaining = round3(clampPercent(lw.remainingPercent))
				wh.RawResetSec = lw.resetAtMs / 1000
				wh.ObservedSec = lw.observedAtMs / 1000
				if wh.ObservedSec == 0 {
					wh.ObservedSec = nowSec
				}
				if !lw.stale && wh.RawResetSec > nowSec {
					wh.ResetSec = effectiveResetSec(wh.ObservedSec, wh.Remaining, wh.RawResetSec, meta.durationSeconds)
				}
				// Append live state sample if newer than last history point
				if wh.ObservedSec > 0 && (len(wh.Points) == 0 || wh.ObservedSec > wh.Points[len(wh.Points)-1].T+30) {
					wh.Points = append(wh.Points, lampHistoryPoint{
						T: wh.ObservedSec,
						R: wh.Remaining,
						X: wh.RawResetSec,
					})
				}
			} else if len(pts) > 0 {
				last := pts[len(pts)-1]
				wh.Remaining = last.R
				wh.ObservedSec = last.T
				wh.RawResetSec = last.X
				if last.X > nowSec {
					wh.ResetSec = effectiveResetSec(last.T, last.R, last.X, meta.durationSeconds)
				}
			}

			if len(wh.Points) > 0 || wh.ObservedSec > 0 {
				accStore.Windows[wk] = wh
				acc.hasHistory = true
			}
		}
	}

	c.histStoreMu.Lock()
	c.histStore = store
	c.histStoreMu.Unlock()
}

func resolveHistoryRange(windowKind, rangeName string) (normWindow, normRange string, spanSec, bucketSec int64) {
	if windowKind == "7d" {
		normWindow = "7d"
		switch strings.ToLower(strings.TrimSpace(rangeName)) {
		case "24h", "1d":
			return "7d", "24h", 24 * 3600, 0
		case "3d":
			return "7d", "3d", 3 * 86400, 1800
		case "4d":
			return "7d", "4d", 4 * 86400, 1800
		case "7d":
			// 8d span ensures future [now, reset_sec] fits cleanly alongside history
			return "7d", "7d", 8 * 86400, 3600
		case "8d", "":
			return "7d", "8d", 8 * 86400, 3600
		case "14d":
			return "7d", "14d", 15 * 86400, 7200
		case "15d":
			return "7d", "15d", 15 * 86400, 7200
		case "30d", "1mo":
			return "7d", "30d", 30 * 86400, 14400
		default:
			return "7d", "8d", 8 * 86400, 3600
		}
	}
	normWindow = "5h"
	switch strings.ToLower(strings.TrimSpace(rangeName)) {
	case "1h":
		return "5h", "1h", 3600, 0
	case "3h":
		return "5h", "3h", 3 * 3600, 0
	case "6h":
		return "5h", "6h", 6 * 3600, 0
	case "12h":
		return "5h", "12h", 12 * 3600, 0
	case "24h", "":
		return "5h", "24h", 24 * 3600, 0
	default:
		return "5h", "24h", 24 * 3600, 0
	}
}

func (c *CPACollector) GetAccountHistory(accountID, windowKind, rangeName string) (*AntigravityHistoryResponse, bool) {
	c.histStoreMu.RLock()
	store := c.histStore
	c.histStoreMu.RUnlock()
	if store == nil {
		return nil, false
	}
	accStore, ok := store.Accounts[strings.TrimSpace(accountID)]
	if !ok {
		return nil, false
	}

	normWin, normRange, spanSec, bucketSec := resolveHistoryRange(windowKind, rangeName)
	toSec := time.Now().Unix()
	if store.UpdatedAtSec > toSec {
		toSec = store.UpdatedAtSec
	}
	fromSec := toSec - spanSec

	var order []string
	if normWin == "7d" {
		order = []string{"gemini_weekly", "claude_weekly", "argon_weekly"}
	} else {
		order = []string{"gemini_5h", "claude_5h"}
	}

	resp := &AntigravityHistoryResponse{
		AccountID:     accStore.PublicID,
		Window:        normWin,
		Range:         normRange,
		FromSec:       fromSec,
		ToSec:         toSec,
		BucketSeconds: bucketSec,
		SampleStepSec: lampDefaultStepSec,
		Series:        make([]AntigravityHistorySeries, 0, len(order)),
	}

	for _, wk := range order {
		wh, exists := accStore.Windows[wk]
		if !exists {
			continue
		}
		eff := resolveEffectiveResetTimeline(wh.Points, toSec, wh.DurationSeconds)
		sampled := downsampleHistoryPoints(wh.Points, eff, fromSec, toSec, bucketSec)

		pts := make([][2]float64, len(sampled))
		for i, p := range sampled {
			pts[i] = [2]float64{float64(p.T), p.R}
		}
		cycleResets := extractCycleResets(sampled)
		resets := extractEffectiveResets(wh.Points, eff, fromSec, toSec, wh.DurationSeconds)

		// Re-check effective reset against current time; honor tail-protected timeline if consistent
		resetSec := wh.ResetSec
		if resetSec == 0 && len(wh.Points) > 0 {
			for i := len(wh.Points) - 1; i >= 0; i-- {
				if wh.Points[i].T <= toSec {
					if eff[i] > toSec && (wh.RawResetSec == 0 || absInt64(eff[i]-wh.RawResetSec) <= lampResetDriftSec) {
						resetSec = eff[i]
					}
					break
				}
			}
		}
		if resetSec > 0 && resetSec <= toSec {
			resetSec = 0
		}

		resp.Series = append(resp.Series, AntigravityHistorySeries{
			Key:             wh.Key,
			Group:           wh.Group,
			Label:           wh.Label,
			DurationSeconds: wh.DurationSeconds,
			Remaining:       wh.Remaining,
			ResetSec:        resetSec,
			RawResetSec:     wh.RawResetSec,
			ObservedSec:     wh.ObservedSec,
			Points:          pts,
			Resets:          resets,
			CycleResets:     cycleResets,
		})
	}
	return resp, true
}

type resolvedHistoryPoint struct {
	lampHistoryPoint
	Eff int64
}

func resolveEffectiveResetTimeline(pts []lampHistoryPoint, toSec, durationSec int64) []int64 {
	eff := make([]int64, len(pts))
	if len(pts) == 0 || durationSec <= 0 {
		return eff
	}

	var prevEff int64
	for i, p := range pts {
		if p.T > toSec {
			break
		}
		ex := effectiveResetSec(p.T, p.R, p.X, durationSec)

		// Multi-step forward lookahead:
		// If a 100% sample sits right after a reset (within 120s of full duration), scan all
		// consecutive samples keeping the same anchored reset timestamp (within drift). If any
		// sample starts consuming (< 99.999) or stays anchored > 2*drift without sliding forward,
		// confirm p.X as an anchored effective reset timestamp.
		if ex == 0 && p.X > 0 {
			for j := i + 1; j < len(pts) && pts[j].T <= toSec; j++ {
				nxt := pts[j]
				if nxt.X <= 0 || absInt64(nxt.X-p.X) > lampResetDriftSec {
					break
				}
				if nxt.R < 99.999 || (nxt.T-p.T) > lampResetDriftSec*2 {
					ex = p.X
					break
				}
			}
		}

		// Tail fresh-reset protection:
		// Near the tail (within 2*drift of toSec), if a prior anchored cycle exists and p.X
		// stepped forward by one cycle duration (or continues that newly confirmed tail cycle),
		// confirm p.X directly to prevent 1~2 minute flicker to 0.
		if ex == 0 && p.X > 0 && prevEff > 0 && toSec-p.T <= lampResetDriftSec*2 {
			if absInt64(p.X-prevEff-durationSec) <= lampResetDriftSec || absInt64(p.X-prevEff) <= lampResetDriftSec {
				ex = p.X
			}
		}

		eff[i] = ex
		prevEff = ex
	}

	// Single-point transient unopened glitch filter:
	// Smooth isolated eff[i] == 0 sandwiched by identical anchored cycles.
	for i := 1; i+1 < len(pts); i++ {
		if pts[i].T > toSec || pts[i+1].T > toSec {
			break
		}
		if eff[i] == 0 && eff[i-1] > 0 && eff[i+1] > 0 && absInt64(eff[i-1]-eff[i+1]) <= lampResetDriftSec {
			eff[i] = eff[i-1]
		}
	}

	return eff
}

func isCycleReset(prev, cur lampHistoryPoint, prevEff, curEff int64) bool {
	// Explicit non-reset exclusions:
	// 1. Opening an unopened 100% sliding window on first consumption
	if prevEff == 0 && curEff > 0 && prev.R >= 99.999 {
		return false
	}
	// 2. Full idle window releasing back to unopened sliding window
	if prevEff > 0 && curEff == 0 && prev.R >= 99.999 {
		return false
	}
	// Condition A: anchored cycle advanced to a later reset timestamp
	if prevEff > 0 && curEff > 0 && curEff > prevEff+lampResetDriftSec {
		return true
	}
	// Condition B: consumed anchored cycle reset into unopened 100% state
	if prevEff > 0 && curEff == 0 && prev.R < 99.999 && cur.R >= 99.999 {
		return true
	}
	// Condition C: fallback when reset timestamp is missing or unchanged on a sharp jump
	if cur.R-prev.R >= lampResetJumpPercent {
		return true
	}
	return false
}

func extractCycleResets(sampled []resolvedHistoryPoint) []int64 {
	if len(sampled) < 2 {
		return nil
	}
	var out []int64
	for i := 1; i < len(sampled); i++ {
		if isCycleReset(sampled[i-1].lampHistoryPoint, sampled[i].lampHistoryPoint, sampled[i-1].Eff, sampled[i].Eff) {
			out = append(out, sampled[i].T)
		}
	}
	return out
}

func extractEffectiveResets(pts []lampHistoryPoint, eff []int64, fromSec, toSec, durationSec int64) [][2]int64 {
	if len(pts) == 0 || durationSec <= 0 {
		return [][2]int64{}
	}
	if len(eff) != len(pts) {
		eff = resolveEffectiveResetTimeline(pts, toSec, durationSec)
	}
	scanFrom := fromSec - durationSec
	var transitions [][2]int64
	var lastX int64
	hasLast := false

	for i, p := range pts {
		if p.T > toSec {
			break
		}
		if p.T < scanFrom {
			continue
		}
		ex := eff[i]
		changed := false
		if !hasLast {
			changed = true
		} else if (ex == 0) != (lastX == 0) {
			changed = true
		} else if ex > 0 && absInt64(ex-lastX) > lampResetDriftSec {
			changed = true
		}
		if changed {
			transitions = append(transitions, [2]int64{p.T, ex})
			lastX = ex
			hasLast = true
		}
	}

	if len(transitions) == 0 {
		return [][2]int64{}
	}
	// Keep the last transition at or before fromSec (for active cycle baseline) plus all after fromSec
	startIdx := 0
	for i, tr := range transitions {
		if tr[0] <= fromSec {
			startIdx = i
		} else {
			break
		}
	}
	out := make([][2]int64, len(transitions)-startIdx)
	copy(out, transitions[startIdx:])
	return out
}

func downsampleHistoryPoints(pts []lampHistoryPoint, eff []int64, fromSec, toSec, bucketSec int64) []resolvedHistoryPoint {
	if len(pts) == 0 {
		return []resolvedHistoryPoint{}
	}
	maxLeadGap := bucketSec
	if maxLeadGap < lampDefaultStepSec {
		maxLeadGap = lampDefaultStepSec
	}
	maxLeadGap *= 4

	var lead *resolvedHistoryPoint
	var inRange []resolvedHistoryPoint
	for i := range pts {
		p := pts[i]
		if p.T > toSec {
			break
		}
		var ex int64
		if i < len(eff) {
			ex = eff[i]
		}
		rp := resolvedHistoryPoint{lampHistoryPoint: p, Eff: ex}
		if p.T < fromSec {
			cp := rp
			lead = &cp
			continue
		}
		inRange = append(inRange, rp)
	}

	var slice []resolvedHistoryPoint
	if lead != nil && fromSec-lead.T <= maxLeadGap {
		slice = make([]resolvedHistoryPoint, 0, len(inRange)+1)
		slice = append(slice, *lead)
		slice = append(slice, inRange...)
	} else {
		slice = inRange
	}
	if len(slice) == 0 {
		return []resolvedHistoryPoint{}
	}

	if bucketSec <= 0 {
		return slice
	}

	// Reset-preserving time-bucket downsampling:
	// Within each bucket keep the lowest remaining point, BUT flush immediately
	// before & after any cycle reset (resolved via timeline + isCycleReset) so micro-reset
	// pre-reset lows and post-reset 100% peaks are always preserved.
	out := make([]resolvedHistoryPoint, 0, len(slice)/2+4)
	curBucket := slice[0].T / bucketSec
	bucketMin := slice[0]
	prev := slice[0]

	appendPoint := func(p resolvedHistoryPoint) {
		if len(out) > 0 && out[len(out)-1].T == p.T {
			out[len(out)-1] = p
			return
		}
		out = append(out, p)
	}

	for i := 1; i < len(slice); i++ {
		p := slice[i]
		if isCycleReset(prev.lampHistoryPoint, p.lampHistoryPoint, prev.Eff, p.Eff) {
			appendPoint(bucketMin)
			if prev.T != bucketMin.T {
				appendPoint(prev)
			}
			appendPoint(p)
			curBucket = p.T / bucketSec
			bucketMin = p
			prev = p
			continue
		}

		b := p.T / bucketSec
		if b != curBucket {
			appendPoint(bucketMin)
			curBucket = b
			bucketMin = p
		} else if p.R <= bucketMin.R {
			bucketMin = p
		}
		prev = p
	}
	appendPoint(bucketMin)
	// Always preserve the very last raw sample accurately
	last := slice[len(slice)-1]
	if last.T != bucketMin.T {
		appendPoint(last)
	}
	return out
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (c *CPACollector) doCPAMPJSON(ctx context.Context, method, path, adminKey string, reqBody any, dst any) (int, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, err
		}
		bodyReader = bytes.NewReader(b)
	}
	url := strings.TrimRight(c.cfg.CPAMPBaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+adminKey)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("cpamp http %d", resp.StatusCode)
	}
	if dst != nil {
		if err := json.Unmarshal(raw, dst); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (c *CPACollector) fetchCPAMPAuthFiles(ctx context.Context, adminKey string) ([]cpampAuthFile, int, error) {
	var resp cpampAuthFilesResp
	code, err := c.doCPAMPJSON(ctx, http.MethodGet, "/v0/management/auth-files", adminKey, nil, &resp)
	if err != nil {
		return nil, code, err
	}
	return resp.Files, code, nil
}

func (c *CPACollector) enrichHistory(ctx context.Context, adminKey string, accounts []*internalCPAAccount) (int, error) {
	req := cpampHistoryReq{
		Accounts: make([]cpampAccountTarget, 0, len(accounts)),
		CatchUp:  false,
	}
	byRow := make(map[string]*internalCPAAccount, len(accounts))
	for _, acc := range accounts {
		byRow[acc.rowKey] = acc
		req.Accounts = append(req.Accounts, cpampAccountTarget{
			RowKey:                acc.rowKey,
			AccountSnapshot:       acc.email,
			AuthLabelSnapshot:     acc.label,
			AuthFileSnapshot:      acc.fileName,
			AuthProviderSnapshot:  acc.provider,
			AuthProjectIDSnapshot: acc.projectID,
			AuthIndex:             acc.authIndex,
			Source:                acc.fileName,
		})
	}

	var resp cpampHistoryResp
	code, err := c.doCPAMPJSON(ctx, http.MethodPost, "/v0/management/monitoring/account-history", adminKey, req, &resp)
	if err != nil {
		return code, err
	}

	for _, item := range resp.Items {
		acc, ok := byRow[item.RowKey]
		if !ok {
			continue
		}
		h := AntigravityAccountHistory{
			Available:     true,
			TotalRequests: item.TotalRequests,
			SuccessCalls:  item.SuccessCalls,
			FailureCalls:  item.FailureCalls,
			TotalTokens:   item.TotalTokens,
			TotalCost:     round2(item.TotalCost),
			RecentBars:    []AntigravityRecentBar{},
		}
		if item.SuccessRate != nil {
			sr := *item.SuccessRate
			if sr >= 0 && sr <= 1.0 && (item.TotalRequests > 0 || sr > 0) {
				// CPAMP returns success_rate as 0..1 fraction
				sr = sr * 100.0
			}
			h.SuccessRate = floatPtr(round2(clampPercent(sr)))
		} else if item.TotalRequests > 0 {
			h.SuccessRate = floatPtr(round2(float64(item.SuccessCalls) / float64(item.TotalRequests) * 100.0))
		}

		// Parse recent_requests into sanitized green/red bars (oldest -> newest, up to 24 bars)
		bars := make([]AntigravityRecentBar, 0, len(item.RecentRequests))
		for _, rr := range item.RecentRequests {
			okReq := isRecentRequestOK(rr)
			ts := rr.TimestampMs
			if ts == 0 && rr.Timestamp > 0 {
				if rr.Timestamp < 1e12 {
					ts = rr.Timestamp * 1000
				} else {
					ts = rr.Timestamp
				}
			}
			bars = append(bars, AntigravityRecentBar{
				OK:          okReq,
				TimestampMs: ts,
				StatusCode:  rr.StatusCode,
			})
		}
		// Sort oldest -> newest if timestamps exist
		if len(bars) > 1 && bars[0].TimestampMs > 0 && bars[len(bars)-1].TimestampMs > 0 {
			sort.SliceStable(bars, func(i, j int) bool {
				return bars[i].TimestampMs < bars[j].TimestampMs
			})
		}
		if len(bars) > 24 {
			bars = bars[len(bars)-24:]
		}
		var okCnt, failCnt int
		for _, b := range bars {
			if b.OK {
				okCnt++
			} else {
				failCnt++
			}
		}
		if item.LatestRequest != nil {
			ts := item.LatestRequest.TimestampMs
			if ts == 0 && item.LatestRequest.Timestamp > 0 {
				if item.LatestRequest.Timestamp < 1e12 {
					ts = item.LatestRequest.Timestamp * 1000
				} else {
					ts = item.LatestRequest.Timestamp
				}
			}
			h.LatestRequestAtMs = ts
		}
		if len(bars) > 0 && h.LatestRequestAtMs == 0 {
			h.LatestRequestAtMs = bars[len(bars)-1].TimestampMs
		}
		h.RecentBars = bars
		h.RecentSuccessCount = okCnt
		h.RecentFailureCount = failCnt
		if len(bars) > 0 {
			h.RecentSuccessRate = floatPtr(round2(float64(okCnt) / float64(len(bars)) * 100.0))
		}
		acc.history = h
	}
	return code, nil
}

func isRecentRequestOK(rr cpampRecentRequest) bool {
	if rr.Failed != nil {
		return !*rr.Failed
	}
	if rr.Success != nil {
		return *rr.Success
	}
	if rr.OK != nil {
		return *rr.OK
	}
	if rr.StatusCode > 0 {
		return rr.StatusCode >= 200 && rr.StatusCode < 400
	}
	s := strings.ToLower(strings.TrimSpace(rr.Status))
	if s != "" {
		return s == "ok" || s == "success" || s == "succeeded" || s == "completed"
	}
	return true
}

func (c *CPACollector) enrichWindowUsage(ctx context.Context, adminKey string, accounts []*internalCPAAccount, nowMs int64) (int, error) {
	var mainTargets []cpampWindowTarget
	var forecastBaseTargets []cpampWindowTarget

	for _, acc := range accounts {
		for i := range acc.windows {
			w := &acc.windows[i]
			if w.modelScopeKey == "" || w.resetAtMs <= 0 || w.durationSeconds <= 0 {
				continue
			}
			durMs := w.durationSeconds * 1000
			winStartMs := w.resetAtMs - durMs
			if winStartMs <= 0 {
				continue
			}
			scope := &cpampModelScope{
				Kind:     "family",
				Key:      w.modelScopeKey,
				Complete: true,
			}

			curToMs := nowMs
			if w.resetAtMs < curToMs {
				curToMs = w.resetAtMs
			}
			if winStartMs < curToMs {
				mainTargets = append(mainTargets, cpampWindowTarget{
					RequestKey:            fmt.Sprintf("%s|%s|current", acc.rowKey, w.key),
					RowKey:                acc.rowKey,
					WindowKey:             w.key,
					ProviderWindowID:      w.providerWindowID,
					Period:                "current",
					FromMs:                winStartMs,
					ToMs:                  curToMs,
					ModelScope:            scope,
					AccountSnapshot:       acc.email,
					AuthLabelSnapshot:     acc.label,
					AuthFileSnapshot:      acc.fileName,
					AuthProviderSnapshot:  acc.provider,
					AuthProjectIDSnapshot: acc.projectID,
					AuthIndex:             acc.authIndex,
					Source:                acc.fileName,
				})
				// Separate forecast_base window [winStartMs, observedAtMs] so post-sample requests do not inflate forecast
				if w.observedAtMs > winStartMs && w.observedAtMs < curToMs {
					forecastBaseTargets = append(forecastBaseTargets, cpampWindowTarget{
						RequestKey:            fmt.Sprintf("%s|%s|forecast_base", acc.rowKey, w.key),
						RowKey:                acc.rowKey,
						WindowKey:             w.key,
						ProviderWindowID:      w.providerWindowID,
						Period:                "current",
						FromMs:                winStartMs,
						ToMs:                  w.observedAtMs,
						ModelScope:            scope,
						AccountSnapshot:       acc.email,
						AuthLabelSnapshot:     acc.label,
						AuthFileSnapshot:      acc.fileName,
						AuthProviderSnapshot:  acc.provider,
						AuthProjectIDSnapshot: acc.projectID,
						AuthIndex:             acc.authIndex,
						Source:                acc.fileName,
					})
				}
			}

			prevStartMs := winStartMs - durMs
			if prevStartMs > 0 {
				mainTargets = append(mainTargets, cpampWindowTarget{
					RequestKey:            fmt.Sprintf("%s|%s|previous", acc.rowKey, w.key),
					RowKey:                acc.rowKey,
					WindowKey:             w.key,
					ProviderWindowID:      w.providerWindowID,
					Period:                "previous",
					FromMs:                prevStartMs,
					ToMs:                  winStartMs,
					ModelScope:            scope,
					AccountSnapshot:       acc.email,
					AuthLabelSnapshot:     acc.label,
					AuthFileSnapshot:      acc.fileName,
					AuthProviderSnapshot:  acc.provider,
					AuthProjectIDSnapshot: acc.projectID,
					AuthIndex:             acc.authIndex,
					Source:                acc.fileName,
				})
			}
		}
	}

	if len(mainTargets) == 0 {
		return http.StatusOK, nil
	}

	var mainResp cpampWindowUsageResp
	code, err := c.doCPAMPJSON(ctx, http.MethodPost, "/v0/management/monitoring/account-window-usage", adminKey, cpampWindowUsageReq{Windows: mainTargets}, &mainResp)
	if err != nil {
		return code, err
	}

	results := make(map[string]cpampWindowUsageItem, len(mainResp.Items)+len(forecastBaseTargets))
	for _, it := range mainResp.Items {
		if it.RequestKey != "" {
			results[it.RequestKey] = it
		}
	}

	if len(forecastBaseTargets) > 0 {
		var fbResp cpampWindowUsageResp
		if _, fbErr := c.doCPAMPJSON(ctx, http.MethodPost, "/v0/management/monitoring/account-window-usage", adminKey, cpampWindowUsageReq{Windows: forecastBaseTargets}, &fbResp); fbErr == nil {
			for _, it := range fbResp.Items {
				if it.RequestKey != "" {
					results[it.RequestKey] = it
				}
			}
		}
	}

	// Apply window usage & CPAMP estimateWindowUsage forecast formula
	for _, acc := range accounts {
		for i := range acc.windows {
			w := &acc.windows[i]
			cur, hasCur := results[fmt.Sprintf("%s|%s|current", acc.rowKey, w.key)]
			fb, hasFB := results[fmt.Sprintf("%s|%s|forecast_base", acc.rowKey, w.key)]
			prev, hasPrev := results[fmt.Sprintf("%s|%s|previous", acc.rowKey, w.key)]

			curValid := hasCur && cur.Matched && (cur.ScopeMatchStatus == "" || cur.ScopeMatchStatus == "complete")
			if curValid {
				w.currentRequests = cur.TotalRequests
				w.currentTokens = cur.TotalTokens
				w.currentCost = cur.TotalCost
				w.hasCurrentUsage = cur.TotalRequests > 0 || cur.TotalTokens > 0 || cur.TotalCost > 0
			}

			// Determine forecast base (prefer observedAtMs slice to avoid inflating forecast)
			var base *cpampWindowUsageItem
			if hasFB && fb.Matched && (fb.ScopeMatchStatus == "" || fb.ScopeMatchStatus == "complete") {
				fbCopy := fb
				base = &fbCopy
			} else if curValid {
				if w.observedAtMs == 0 || cur.LastSeenMs == 0 || cur.LastSeenMs <= w.observedAtMs {
					curCopy := cur
					base = &curCopy
				}
			}

			usedPct := 100.0 - w.remainingPercent
			if !w.stale && usedPct > 0 && usedPct <= 100 && base != nil && (base.TotalRequests > 0 || base.TotalTokens > 0 || base.TotalCost > 0) {
				multiplier := 100.0 / usedPct
				estTokens := int64(math.Round(float64(base.TotalTokens) * multiplier))
				if curValid && cur.TotalTokens > estTokens {
					estTokens = cur.TotalTokens
				}
				estCost := round2(base.TotalCost * multiplier)
				if curValid && cur.TotalCost > estCost {
					estCost = round2(cur.TotalCost)
				}
				w.hasForecast = true
				w.forecastBasis = "quota"
				w.forecastTokens = estTokens
				w.forecastCost = estCost
			} else if !w.stale && hasPrev && prev.Matched && (prev.ScopeMatchStatus == "" || prev.ScopeMatchStatus == "complete") && (prev.TotalCost >= 0.01 || prev.TotalTokens >= 1000) {
				estTokens := prev.TotalTokens
				estCost := round2(prev.TotalCost)
				if !curValid || (estTokens >= cur.TotalTokens && estCost+1e-9 >= cur.TotalCost) {
					w.hasForecast = true
					w.forecastBasis = "previous"
					w.forecastTokens = estTokens
					w.forecastCost = estCost
				}
			}
		}
	}

	return code, nil
}

func parseRawStringOrInt(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.FormatInt(n, 10)
	}
	return ""
}
