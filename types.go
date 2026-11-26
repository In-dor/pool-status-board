package main

// Snapshot is the strictly sanitized public DTO returned by /api/snapshot.
// It must NEVER contain real emails, auth filenames, OAuth tokens, cookies,
// passwords, admin keys, proxy addresses, or project IDs.
type Snapshot struct {
	GeneratedAtMs          int64            `json:"generated_at_ms"`
	RefreshIntervalSeconds int              `json:"refresh_interval_seconds"`
	Antigravity            AntigravityPool  `json:"antigravity"`
	AIStudio               AIStudioPool     `json:"aistudio"`
}

// ---------- Antigravity (CLIProxyAPI + CPAMP) ----------

type AntigravityPool struct {
	Online        bool                   `json:"online"`
	Status        string                 `json:"status"` // online | degraded | offline
	StatusMessage string                 `json:"status_message,omitempty"`
	CPAMPStatus   string                 `json:"cpamp_status"` // ok | no_key | unauthorized | error
	CPAMPMessage  string                 `json:"cpamp_message,omitempty"`
	UpdatedAtMs   int64                  `json:"updated_at_ms"`
	Summary       AntigravityPoolSummary `json:"summary"`
	Accounts      []AntigravityAccount   `json:"accounts"`
}

type AntigravityPoolSummary struct {
	ActiveAccounts      int                        `json:"active_accounts"`
	TierCounts          map[string]int             `json:"tier_counts"`
	TotalRequests       int64                      `json:"total_requests"`
	TotalTokens         int64                      `json:"total_tokens"`
	TotalCost           float64                    `json:"total_cost"`
	WeightedSuccessRate *float64                   `json:"weighted_success_rate"`
	WindowAverages      []AntigravityWindowAverage `json:"window_averages"`
}

type AntigravityWindowAverage struct {
	Key                 string  `json:"key"`
	Label               string  `json:"label"`
	AvgRemainingPercent float64 `json:"avg_remaining_percent"`
	AccountCount        int     `json:"account_count"`
}

type AntigravityAccount struct {
	ID          string                    `json:"id"`
	CodeName    string                    `json:"code_name"`
	Tier        string                    `json:"tier"`   // Ultra | Pro | Free | Unknown
	Status      string                    `json:"status"` // available | limited | exhausted | unknown
	StatusLabel string                    `json:"status_label"`
	History     AntigravityAccountHistory `json:"history"`
	Windows     []AntigravityWindow       `json:"windows"`
	HasHistory  bool                      `json:"has_history"`
	Trend24h    []AntigravityTrendSeries  `json:"trend_24h,omitempty"`
}

type AntigravityAccountHistory struct {
	Available           bool                      `json:"available"`
	StatusNote          string                    `json:"status_note,omitempty"`
	TotalRequests       int64                     `json:"total_requests"`
	SuccessCalls        int64                     `json:"success_calls"`
	FailureCalls        int64                     `json:"failure_calls"`
	TotalTokens         int64                     `json:"total_tokens"`
	TotalCost           float64                   `json:"total_cost"`
	SuccessRate         *float64                  `json:"success_rate"` // 0..100
	LatestRequestAtMs   int64                     `json:"latest_request_at_ms,omitempty"`
	HasRecentBuckets    bool                      `json:"has_recent_buckets"`
	RecentWindowMinutes int                       `json:"recent_window_minutes"` // 200 (20 x 10m)
	RecentWindowSuccess int64                     `json:"recent_window_success"`
	RecentWindowFailure int64                     `json:"recent_window_failure"`
	RecentWindowRate    *float64                  `json:"recent_window_rate"` // 0..100
	RecentBuckets       []AntigravityRecentBucket `json:"recent_buckets"`
	RecentSuccessCount  int                       `json:"recent_success_count"`
	RecentFailureCount  int                       `json:"recent_failure_count"`
	RecentSuccessRate   *float64                  `json:"recent_success_rate"` // 0..100
	RecentBars          []AntigravityRecentBar    `json:"recent_bars,omitempty"`
}

type AntigravityRecentBucket struct {
	Time    string `json:"time,omitempty"`
	Success int64  `json:"success"`
	Failed  int64  `json:"failed"`
}

type AntigravityRecentBar struct {
	OK          bool  `json:"ok"`
	TimestampMs int64 `json:"timestamp_ms,omitempty"`
	StatusCode  int   `json:"status_code,omitempty"`
}

type AntigravityWindow struct {
	Key              string   `json:"key"`       // claude_5h | claude_weekly | gemini_5h | gemini_weekly | argon_weekly
	Label            string   `json:"label"`     // Claude 5h | Claude 周额度 | Gemini 5h | Gemini 周额度 | Argon 周额度
	Group            string   `json:"group"`     // claude | gemini | argon
	DurationSeconds  int64    `json:"duration_seconds"`
	RemainingPercent float64  `json:"remaining_percent"`
	UsedPercent      float64  `json:"used_percent"`
	ResetAtMs        int64    `json:"reset_at_ms"`
	ObservedAtMs     int64    `json:"observed_at_ms"`
	Stale            bool     `json:"stale"`
	HasCurrentUsage  bool     `json:"has_current_usage"`
	CurrentRequests  int64    `json:"current_requests"`
	CurrentTokens    int64    `json:"current_tokens"`
	CurrentCost      float64  `json:"current_cost"`
	HasForecast               bool     `json:"has_forecast"`
	ForecastBasis             string   `json:"forecast_basis,omitempty"` // quota | previous
	ForecastTokens            int64    `json:"forecast_tokens"`
	ForecastCost              float64  `json:"forecast_cost"`
	PostSampleCostUSD         float64  `json:"post_sample_cost_usd,omitempty"`
	PostSampleTokens          int64    `json:"post_sample_tokens,omitempty"`
	PostSampleRequests        int64    `json:"post_sample_requests,omitempty"`
	EstimatedRemainingPercent *float64 `json:"estimated_remaining_percent,omitempty"`
}

type AntigravityTrendSeries struct {
	Key    string                 `json:"key"`
	Label  string                 `json:"label"`
	Points []AntigravityTrendPoint `json:"points"`
}

type AntigravityTrendPoint struct {
	TimestampMs      int64   `json:"t"`
	RemainingPercent float64 `json:"r"`
}

type AntigravityHistoryResponse struct {
	AccountID     string                     `json:"account_id"`
	Window        string                     `json:"window"` // 5h | 7d
	Range         string                     `json:"range"`  // 3h | 6h | 12h | 24h | 3d | 7d | 8d | 14d | 15d | 30d
	FromSec       int64                      `json:"from_sec"`
	ToSec         int64                      `json:"to_sec"`
	BucketSeconds int64                      `json:"bucket_seconds"`
	SampleStepSec int64                      `json:"sample_step_sec"`
	Series        []AntigravityHistorySeries `json:"series"`
}

type AntigravityHistorySeries struct {
	Key             string       `json:"key"`
	Group           string       `json:"group"`
	Label           string       `json:"label"`
	DurationSeconds int64        `json:"duration_seconds"`
	Remaining       float64      `json:"remaining"`
	ResetSec        int64        `json:"reset_sec"`     // effective reset sec; 0 when unopened full window
	RawResetSec     int64        `json:"raw_reset_sec"` // raw reset sec from state/history
	ObservedSec     int64        `json:"observed_sec"`
	Points             [][2]float64 `json:"points"`                         // [[t_sec, remaining], ...]
	Resets             [][2]int64   `json:"resets"`                         // [[t_sec, effective_reset_sec], ...]
	CycleResets        []int64      `json:"cycle_resets,omitempty"`         // sample timestamps (t_sec) where real cycle resets occur
	EstimatedPoint     []float64    `json:"estimated_point,omitempty"`      // [now_sec, est_remaining] post-sample estimate/hold; never mixed into Points
	PostSampleRequests *int64       `json:"post_sample_requests,omitempty"` // non-nil when CPAMP confirmed post-sample activity (0 = confirmed idle)
}

// ---------- AIStudio2API ----------

type AIStudioPool struct {
	Online        bool                   `json:"online"`
	Status        string                 `json:"status"` // online | degraded | offline
	StatusMessage string                 `json:"status_message,omitempty"`
	UpdatedAtMs   int64                  `json:"updated_at_ms"`
	ServiceState  string                 `json:"service_state"`
	ServiceReady  bool                   `json:"service_ready"`
	ActiveReqs    int                    `json:"active_requests"`
	CycleStartMs  int64                  `json:"cycle_start_ms"`
	NextResetMs   int64                  `json:"next_reset_ms"`
	ResetRuleNote string                 `json:"reset_rule_note"`
	TierCounts    map[string]int         `json:"tier_counts"`
	AccountCounts AIStudioAccountCounts  `json:"account_counts"`
	Playground    AIStudioChannelSummary `json:"playground"`
	HourlyBuckets []AIStudioHourlyBucket `json:"hourly_buckets"`
	Models        []AIStudioModelSummary `json:"models"`
	Accounts      []AIStudioAccount      `json:"accounts"`
}

type AIStudioHourlyBucket struct {
	TimestampMs     int64   `json:"timestamp_ms"`
	Label           string  `json:"label"`
	Requests        int64   `json:"requests"`
	Succeeded       int64   `json:"succeeded"`
	Failed          int64   `json:"failed"`
	OtherFailed     int64   `json:"other_failed"`
	Canceled        int64   `json:"canceled"`
	RateLimited     int64   `json:"rate_limited"`
	InputTokens     int64   `json:"input_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	ReplyTokens     int64   `json:"reply_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	FirstEventP50Ms float64 `json:"first_event_p50_ms"`
	FirstEventAvgMs float64 `json:"first_event_avg_ms"`
}

type AIStudioAccountCounts struct {
	Enabled      int `json:"enabled"`
	Ready        int `json:"ready"`
	Busy         int `json:"busy"`
	Cooldown     int `json:"cooldown"`
	AuthRequired int `json:"auth_required"`
	Unavailable  int `json:"unavailable"`
}

type AIStudioChannelSummary struct {
	Requests        int64   `json:"requests"`
	Succeeded       int64   `json:"succeeded"`
	Failed          int64   `json:"failed"`
	Canceled        int64   `json:"canceled"`
	RateLimited     int64   `json:"rate_limited"`
	SuccessRate     *float64 `json:"success_rate"` // 0..100
	InputTokens     int64   `json:"input_tokens"`
	ReplyTokens     int64   `json:"reply_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	FirstEventAvgMs float64 `json:"first_event_avg_ms"`
	FirstEventP50Ms float64 `json:"first_event_p50_ms"`
	FirstEventP95Ms float64 `json:"first_event_p95_ms"`
	ActiveCooldowns int     `json:"active_cooldowns"`
}

type AIStudioModelSummary struct {
	ID                  string   `json:"id"`
	Label               string   `json:"label"`
	Category            string   `json:"category,omitempty"`
	EligibleTiers       []string `json:"eligible_tiers"`
	EligibleAccounts    int      `json:"eligible_accounts"`
	AvailableAccounts   int      `json:"available_accounts"`
	CoolingAccounts     int      `json:"cooling_accounts"`
	ExhaustedAccounts   int      `json:"exhausted_accounts"`
	UnavailableAccounts int      `json:"unavailable_accounts"`
	LimitRequests       int64    `json:"limit_requests"`
	UsedRequests        int64    `json:"used_requests"`
	EffectiveRemReq     int64    `json:"effective_remaining_requests"`
	LimitTokens         int64    `json:"limit_tokens"`
	UsedTokens          int64    `json:"used_tokens"`
	EffectiveRemTokens  int64    `json:"effective_remaining_tokens"`
	EffectivePercent    float64  `json:"effective_percent"` // 0..100 bottleneck
	EarliestRecoveryMs  int64    `json:"earliest_recovery_ms,omitempty"`
}

type AIStudioAccount struct {
	ID                 string                  `json:"id"`
	CodeName           string                  `json:"code_name"`
	Tier               string                  `json:"tier"` // Ultra | Pro | Free
	State              string                  `json:"state"`
	StateLabel         string                  `json:"state_label"`
	ActiveRequests     int                     `json:"active_requests"`
	GlobalCooldownMs   int64                   `json:"global_cooldown_ms,omitempty"`
	UsedRequests       int64                   `json:"used_requests"`
	UsedTokens         int64                   `json:"used_tokens"`
	AvailableModelCnt  int                     `json:"available_models"`
	EligibleModelCnt   int                     `json:"eligible_models"`
	CoolingModelCnt    int                     `json:"cooling_models"`
	Models             []AIStudioAccountModel  `json:"models"`
}

type AIStudioAccountModel struct {
	ModelID            string  `json:"model_id"`
	Status             string  `json:"status"` // available | cooldown | exhausted_requests | exhausted_tokens | auth_required | unavailable | ineligible
	StatusLabel        string  `json:"status_label"`
	LimitRequests      int64   `json:"limit_requests,omitempty"`
	UsedRequests       int64   `json:"used_requests,omitempty"`
	EffectiveRemReq    int64   `json:"effective_remaining_requests,omitempty"`
	LimitTokens        int64   `json:"limit_tokens,omitempty"`
	UsedTokens         int64   `json:"used_tokens,omitempty"`
	EffectiveRemTokens int64   `json:"effective_remaining_tokens,omitempty"`
	EffectivePercent   float64 `json:"effective_percent,omitempty"`
	CooldownUntilMs    int64   `json:"cooldown_until_ms,omitempty"`
}

// ---------- Projected Snapshot Views (?view=summary|compact|tui) ----------

type snapshotSummaryView struct {
	GeneratedAtMs          int64                   `json:"generated_at_ms"`
	RefreshIntervalSeconds int                     `json:"refresh_interval_seconds"`
	Antigravity            AntigravityPool         `json:"antigravity"`
	AIStudio               aiStudioPoolSummaryView `json:"aistudio"`
}

type aiStudioPoolSummaryView struct {
	Online        bool                   `json:"online"`
	Status        string                 `json:"status"`
	StatusMessage string                 `json:"status_message,omitempty"`
	UpdatedAtMs   int64                  `json:"updated_at_ms"`
	ServiceState  string                 `json:"service_state"`
	ServiceReady  bool                   `json:"service_ready"`
	ActiveReqs    int                    `json:"active_requests"`
	CycleStartMs  int64                  `json:"cycle_start_ms"`
	NextResetMs   int64                  `json:"next_reset_ms"`
	ResetRuleNote string                 `json:"reset_rule_note"`
	TierCounts    map[string]int         `json:"tier_counts"`
	AccountCounts AIStudioAccountCounts  `json:"account_counts"`
	Playground    AIStudioChannelSummary `json:"playground"`
	HourlyBuckets []AIStudioHourlyBucket `json:"hourly_buckets"`
	Models        []AIStudioModelSummary `json:"models"`
}

type snapshotCompactView struct {
	GeneratedAtMs          int64                   `json:"generated_at_ms"`
	RefreshIntervalSeconds int                     `json:"refresh_interval_seconds"`
	Antigravity            AntigravityPool         `json:"antigravity"`
	AIStudio               aiStudioPoolCompactView `json:"aistudio"`
}

type aiStudioPoolCompactView struct {
	Online        bool                   `json:"online"`
	Status        string                 `json:"status"`
	StatusMessage string                 `json:"status_message,omitempty"`
	UpdatedAtMs   int64                  `json:"updated_at_ms"`
	ServiceState  string                 `json:"service_state"`
	ServiceReady  bool                   `json:"service_ready"`
	ActiveReqs    int                    `json:"active_requests"`
	CycleStartMs  int64                  `json:"cycle_start_ms"`
	NextResetMs   int64                  `json:"next_reset_ms"`
	ResetRuleNote string                 `json:"reset_rule_note"`
	TierCounts    map[string]int         `json:"tier_counts"`
	AccountCounts AIStudioAccountCounts  `json:"account_counts"`
	Playground    AIStudioChannelSummary `json:"playground"`
	Models        []AIStudioModelSummary `json:"models"`
}

type snapshotTUIView struct {
	GeneratedAtMs          int64                  `json:"generated_at_ms"`
	RefreshIntervalSeconds int                    `json:"refresh_interval_seconds"`
	Antigravity            antigravityPoolTUIView `json:"antigravity"`
	AIStudio               aiStudioPoolTUIView    `json:"aistudio"`
}

type antigravityPoolTUIView struct {
	Online        bool                      `json:"online"`
	Status        string                    `json:"status"`
	StatusMessage string                    `json:"status_message,omitempty"`
	Summary       antigravitySummaryTUIView `json:"summary"`
	Accounts      []antigravityAccountTUI   `json:"accounts"`
}

type antigravitySummaryTUIView struct {
	ActiveAccounts int `json:"active_accounts"`
}

type antigravityAccountTUI struct {
	ID       string              `json:"id"`
	CodeName string              `json:"code_name"`
	Tier     string              `json:"tier"`
	Status   string              `json:"status"`
	Windows  []AntigravityWindow `json:"windows"`
}

type aiStudioPoolTUIView struct {
	Online        bool                   `json:"online"`
	Status        string                 `json:"status"`
	StatusMessage string                 `json:"status_message,omitempty"`
	NextResetMs   int64                  `json:"next_reset_ms"`
	AccountCounts AIStudioAccountCounts  `json:"account_counts"`
	TierCounts    map[string]int         `json:"tier_counts"`
	Playground    AIStudioChannelSummary `json:"playground"`
	Models        []AIStudioModelSummary `json:"models"`
}

func buildSnapshotSummaryView(snap Snapshot) snapshotSummaryView {
	hourly := snap.AIStudio.HourlyBuckets
	if hourly == nil {
		hourly = []AIStudioHourlyBucket{}
	}
	models := snap.AIStudio.Models
	if models == nil {
		models = []AIStudioModelSummary{}
	}
	return snapshotSummaryView{
		GeneratedAtMs:          snap.GeneratedAtMs,
		RefreshIntervalSeconds: snap.RefreshIntervalSeconds,
		Antigravity:            snap.Antigravity,
		AIStudio: aiStudioPoolSummaryView{
			Online:        snap.AIStudio.Online,
			Status:        snap.AIStudio.Status,
			StatusMessage: snap.AIStudio.StatusMessage,
			UpdatedAtMs:   snap.AIStudio.UpdatedAtMs,
			ServiceState:  snap.AIStudio.ServiceState,
			ServiceReady:  snap.AIStudio.ServiceReady,
			ActiveReqs:    snap.AIStudio.ActiveReqs,
			CycleStartMs:  snap.AIStudio.CycleStartMs,
			NextResetMs:   snap.AIStudio.NextResetMs,
			ResetRuleNote: snap.AIStudio.ResetRuleNote,
			TierCounts:    snap.AIStudio.TierCounts,
			AccountCounts: snap.AIStudio.AccountCounts,
			Playground:    snap.AIStudio.Playground,
			HourlyBuckets: hourly,
			Models:        models,
		},
	}
}

func buildSnapshotCompactView(snap Snapshot) snapshotCompactView {
	models := snap.AIStudio.Models
	if models == nil {
		models = []AIStudioModelSummary{}
	}
	return snapshotCompactView{
		GeneratedAtMs:          snap.GeneratedAtMs,
		RefreshIntervalSeconds: snap.RefreshIntervalSeconds,
		Antigravity:            snap.Antigravity,
		AIStudio: aiStudioPoolCompactView{
			Online:        snap.AIStudio.Online,
			Status:        snap.AIStudio.Status,
			StatusMessage: snap.AIStudio.StatusMessage,
			UpdatedAtMs:   snap.AIStudio.UpdatedAtMs,
			ServiceState:  snap.AIStudio.ServiceState,
			ServiceReady:  snap.AIStudio.ServiceReady,
			ActiveReqs:    snap.AIStudio.ActiveReqs,
			CycleStartMs:  snap.AIStudio.CycleStartMs,
			NextResetMs:   snap.AIStudio.NextResetMs,
			ResetRuleNote: snap.AIStudio.ResetRuleNote,
			TierCounts:    snap.AIStudio.TierCounts,
			AccountCounts: snap.AIStudio.AccountCounts,
			Playground:    snap.AIStudio.Playground,
			Models:        models,
		},
	}
}

func buildSnapshotTUIView(snap Snapshot) snapshotTUIView {
	accounts := make([]antigravityAccountTUI, 0, len(snap.Antigravity.Accounts))
	for _, acc := range snap.Antigravity.Accounts {
		wins := acc.Windows
		if wins == nil {
			wins = []AntigravityWindow{}
		}
		accounts = append(accounts, antigravityAccountTUI{
			ID:       acc.ID,
			CodeName: acc.CodeName,
			Tier:     acc.Tier,
			Status:   acc.Status,
			Windows:  wins,
		})
	}
	models := snap.AIStudio.Models
	if models == nil {
		models = []AIStudioModelSummary{}
	}
	tierCounts := snap.AIStudio.TierCounts
	if tierCounts == nil {
		tierCounts = map[string]int{}
	}
	return snapshotTUIView{
		GeneratedAtMs:          snap.GeneratedAtMs,
		RefreshIntervalSeconds: snap.RefreshIntervalSeconds,
		Antigravity: antigravityPoolTUIView{
			Online:        snap.Antigravity.Online,
			Status:        snap.Antigravity.Status,
			StatusMessage: snap.Antigravity.StatusMessage,
			Summary: antigravitySummaryTUIView{
				ActiveAccounts: snap.Antigravity.Summary.ActiveAccounts,
			},
			Accounts: accounts,
		},
		AIStudio: aiStudioPoolTUIView{
			Online:        snap.AIStudio.Online,
			Status:        snap.AIStudio.Status,
			StatusMessage: snap.AIStudio.StatusMessage,
			NextResetMs:   snap.AIStudio.NextResetMs,
			AccountCounts: snap.AIStudio.AccountCounts,
			TierCounts:    tierCounts,
			Playground:    snap.AIStudio.Playground,
			Models:        models,
		},
	}
}
