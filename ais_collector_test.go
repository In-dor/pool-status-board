package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeModelIDAndAliases(t *testing.T) {
	qf := &QuotasFile{
		Models: []QuotaModelEntry{
			{
				ID:    "gemini-4-neon",
				Label: "Gemini 4 Neon",
			},
			{
				ID:       "gemini-nano-banana-2.1",
				Label:    "Nano Banana 2.1",
				Category: "image",
				Aliases:  []string{"nano-banana-2.1"},
			},
			{
				ID:       "gemini-3-pro-image",
				Label:    "Nano Banana Pro",
				Category: "image",
				Aliases:  []string{"gemini-3-pro-image-preview", "nano-banana-pro"},
			},
			{
				ID:       "gemini-3.1-flash-lite-image",
				Label:    "Nano Banana 2 Lite",
				Category: "image",
				Aliases:  []string{"gemini-2.0-flash-exp", "nano-banana", "nano-banana-2-lite"},
			},
		},
	}
	aliasMap := buildModelAliasMap(qf)

	cases := []struct {
		in   string
		want string
	}{
		{"gemini-nano-banana-2.1", "gemini-nano-banana-2.1"},
		{"  nano-banana-2.1  ", "gemini-nano-banana-2.1"},
		{"models/nano-banana-2.1", "gemini-nano-banana-2.1"},
		{"MODELS/Nano-Banana-Pro", "gemini-3-pro-image"},
		{"models/gemini-3-pro-image-preview(high)", "gemini-3-pro-image"},
		{"gemini-3-pro-image-preview-search", "gemini-3-pro-image"},
		{"models/gemini-3-pro-image-preview(high)-search", "gemini-3-pro-image"},
		{"gemini-4-neon-search(low)", "gemini-4-neon"},
		{"  GEMINI-2.0-FLASH-EXP  ", "gemini-3.1-flash-lite-image"},
		{"*", "*"},
		{"", ""},
		{"models/unregistered-model(high)-search", "unregistered-model"},
	}

	for _, tc := range cases {
		got := normalizeModelID(tc.in, aliasMap)
		if got != tc.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAISCollectorRequestOnlyAndDualModeAndAliases(t *testing.T) {
	now := time.Now().UTC()
	cdShort := now.Add(90 * time.Second).Truncate(time.Second)
	cdLong := now.Add(420 * time.Second).Truncate(time.Second)
	cdOtherShort := now.Add(60 * time.Second).Truncate(time.Second)
	cdOtherLong := now.Add(300 * time.Second).Truncate(time.Second)

	quotas := QuotasFile{
		ResetTimezone: "Asia/Shanghai",
		ResetHour:     16,
		ResetMinute:   0,
		RuleNote:      "每日北京时间 16:00 重置；文本模型按单账号次数与 Token 短板熔断，画图模型按单账号次数计额。",
		Models: []QuotaModelEntry{
			{
				ID:    "gemini-4-neon",
				Label: "Gemini 4 Neon",
				Tiers: map[string]QuotaTierLimit{
					"Free":  {Requests: 0, Tokens: 0},
					"Pro":   {Requests: 50, Tokens: 10000},
					"Ultra": {Requests: 150, Tokens: 20000},
				},
			},
			{
				ID:       "gemini-nano-banana-2.1",
				Label:    "Nano Banana 2.1",
				Category: "image",
				Aliases:  []string{"nano-banana-2.1"},
				Tiers: map[string]QuotaTierLimit{
					"Free":  {Requests: 0, Tokens: 0},
					"Pro":   {Requests: 50, Tokens: 0},
					"Ultra": {Requests: 150, Tokens: 0},
				},
			},
			{
				ID:       "gemini-3-pro-image",
				Label:    "Nano Banana Pro",
				Category: "image",
				Aliases:  []string{"gemini-3-pro-image-preview", "nano-banana-pro"},
				Tiers: map[string]QuotaTierLimit{
					"Free":  {Requests: 0, Tokens: 0},
					"Pro":   {Requests: 50, Tokens: 0},
					"Ultra": {Requests: 100, Tokens: 0},
				},
			},
			{
				ID:       "gemini-3.1-flash-lite-image",
				Label:    "Nano Banana 2 Lite",
				Category: "image",
				Aliases:  []string{"nano-banana", "nano-banana-2-lite"},
				Tiers: map[string]QuotaTierLimit{
					"Free":  {Requests: 100, Tokens: 0},
					"Pro":   {Requests: 100, Tokens: 0},
					"Ultra": {Requests: 150, Tokens: 0},
				},
			},
		},
	}

	tmpDir := t.TempDir()
	quotasPath := filepath.Join(tmpDir, "quotas.json")
	qRaw, err := json.Marshal(quotas)
	if err != nil {
		t.Fatalf("marshal quotas: %v", err)
	}
	if err := os.WriteFile(quotasPath, qRaw, 0o600); err != nil {
		t.Fatalf("write quotas: %v", err)
	}

	// Synthetic mock AIStudio2API server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/status":
			_ = json.NewEncoder(w).Encode(aisStatusResp{
				State: "ready", Running: true, Ready: true, ActiveRequests: 0,
			})
		case "/api/accounts":
			_ = json.NewEncoder(w).Encode(aisAccountsResp{
				Accounts: []aisRawAccount{
					{ID: "acc-ultra-1", Label: "u1", Enabled: true, State: "ready", BenefitTier: json.RawMessage(`"Ultra"`)},
					{ID: "acc-pro-1", Label: "p1", Enabled: true, State: "ready", BenefitTier: json.RawMessage(`"Pro"`)},
					{ID: "acc-pro-2", Label: "p2", Enabled: true, State: "ready", BenefitTier: json.RawMessage(`"Pro"`)},
					{ID: "acc-free-1", Label: "f1", Enabled: true, State: "ready", BenefitTier: json.RawMessage(`"Free"`)},
				},
			})
		case "/api/cooldowns":
			_ = json.NewEncoder(w).Encode(aisCooldownsResp{
				Cooldowns: []aisRawCooldown{
					// Order 1: long then short on acc-pro-1 for gemini-3-pro-image (via alias & canonical)
					{AccountID: "acc-pro-1", Channel: "playground", ModelID: "nano-banana-pro", Until: cdLong},
					{AccountID: "acc-pro-1", Channel: "playground", ModelID: "gemini-3-pro-image", Until: cdShort},
					// Order 2: short then long on acc-pro-2 for gemini-3-pro-image using remaining_seconds fallback
					{AccountID: "acc-pro-2", Channel: "playground", ModelID: "models/gemini-3-pro-image-preview", RemainingSeconds: 60},
					{AccountID: "acc-pro-2", Channel: "playground", ModelID: "gemini-3-pro-image", RemainingSeconds: 300},
					// Order 3: short then long with Until on acc-ultra-1 for gemini-3.1-flash-lite-image
					{AccountID: "acc-ultra-1", Channel: "playground", ModelID: "nano-banana", Until: cdOtherShort},
					{AccountID: "acc-ultra-1", Channel: "playground", ModelID: "nano-banana-2-lite", Until: cdOtherLong},
				},
			})
		case "/api/usage":
			_ = json.NewEncoder(w).Encode(aisUsageResp{
				Totals: aisUsageStats{Requests: 112, Succeeded: 112, TotalTokens: 90000000},
			})
		case "/api/usage/pairs":
			_ = json.NewEncoder(w).Encode(aisUsagePairsResp{
				Pairs: []aisUsagePair{
					// Text model: acc-pro-1 exhausts tokens (10000/10000) despite having 45 requests left
					{Account: "acc-pro-1", Model: "models/gemini-4-neon(high)", aisUsageStats: aisUsageStats{Requests: 5, TotalTokens: 10000}},
					// Image model (tokens: 0): acc-ultra-1 has BOTH canonical and alias calls -> must SUM (10 + 20 = 30, tokens 50000000 + 25000000 = 75000000)
					// Huge used_tokens must NEVER trigger exhausted_tokens!
					{Account: "acc-ultra-1", Model: "gemini-nano-banana-2.1", aisUsageStats: aisUsageStats{Requests: 10, TotalTokens: 50000000}},
					{Account: "acc-ultra-1", Model: "models/Nano-Banana-2.1(high)-search", aisUsageStats: aisUsageStats{Requests: 20, TotalTokens: 25000000}},
					// Image model: acc-pro-1 exhausts requests (50/50)
					{Account: "acc-pro-1", Model: "nano-banana-2.1", aisUsageStats: aisUsageStats{Requests: 50, TotalTokens: 120000}},
					// Image model: acc-pro-2 has 0 usage -> 100% full health
					// Image model: acc-free-1 is eligible for gemini-3.1-flash-lite-image with 25/100 requests used
					{Account: "acc-free-1", Model: "nano-banana", aisUsageStats: aisUsageStats{Requests: 25, TotalTokens: 42000}},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	col := NewAISCollector(AISConfig{
		BaseURL:    srv.URL,
		QuotasFile: quotasPath,
	})

	pool := col.Collect(context.Background())
	if !pool.Online {
		t.Fatalf("expected pool online, got status=%s msg=%s", pool.Status, pool.StatusMessage)
	}
	if len(pool.Models) != 4 {
		t.Fatalf("expected 4 models, got %d", len(pool.Models))
	}

	modelByID := map[string]AIStudioModelSummary{}
	for _, m := range pool.Models {
		if math.IsNaN(m.EffectivePercent) || math.IsInf(m.EffectivePercent, 0) {
			t.Fatalf("model %s has non-finite effective_percent: %v", m.ID, m.EffectivePercent)
		}
		modelByID[m.ID] = m
	}

	// Verify accounts by deterministic tier + original ID order
	accByTier := map[string][]AIStudioAccount{}
	for _, a := range pool.Accounts {
		for _, am := range a.Models {
			if math.IsNaN(am.EffectivePercent) || math.IsInf(am.EffectivePercent, 0) {
				t.Fatalf("account %s model %s has non-finite effective_percent: %v", a.CodeName, am.ModelID, am.EffectivePercent)
			}
		}
		accByTier[a.Tier] = append(accByTier[a.Tier], a)
	}
	findAccModel := func(acc AIStudioAccount, modelID string) AIStudioAccountModel {
		for _, m := range acc.Models {
			if m.ModelID == modelID {
				return m
			}
		}
		t.Fatalf("model %s not found in account %s", modelID, acc.CodeName)
		return AIStudioAccountModel{}
	}

	// 1. Check gemini-nano-banana-2.1 (tokens: 0, Ultra=150, Pro=50, Free=0)
	nb21 := modelByID["gemini-nano-banana-2.1"]
	if nb21.Category != "image" {
		t.Errorf("nb21.Category = %q, want image", nb21.Category)
	}
	if nb21.EligibleAccounts != 3 {
		t.Errorf("nb21.EligibleAccounts = %d, want 3 (1 Ultra + 2 Pro; Free ineligible)", nb21.EligibleAccounts)
	}
	if nb21.AvailableAccounts != 2 || nb21.ExhaustedAccounts != 1 {
		t.Errorf("nb21 avail=%d exhausted=%d, want 2 and 1", nb21.AvailableAccounts, nb21.ExhaustedAccounts)
	}
	if nb21.LimitRequests != 250 || nb21.LimitTokens != 0 {
		t.Errorf("nb21 limits req=%d tok=%d, want 250 and 0", nb21.LimitRequests, nb21.LimitTokens)
	}
	// Ultra used 30 (10+20 merged!), Pro-1 used 50 (exhausted), Pro-2 used 0 -> used=80, effRem=120+0+50=170
	if nb21.UsedRequests != 80 || nb21.UsedTokens != 75120000 {
		t.Errorf("nb21 used req=%d tok=%d, want 80 and 75120000", nb21.UsedRequests, nb21.UsedTokens)
	}
	if nb21.EffectiveRemReq != 170 || nb21.EffectiveRemTokens != 0 {
		t.Errorf("nb21 effRem req=%d tok=%d, want 170 and 0", nb21.EffectiveRemReq, nb21.EffectiveRemTokens)
	}
	// 170 / 250 = 68%
	if nb21.EffectivePercent != 68 {
		t.Errorf("nb21.EffectivePercent = %v, want 68", nb21.EffectivePercent)
	}

	// Check Ultra account on nb21: 30/150 used -> 120 remaining = 80%, NOT exhausted_tokens despite 75M tokens
	ultraAcc := accByTier["Ultra"][0]
	ultraNB21 := findAccModel(ultraAcc, "gemini-nano-banana-2.1")
	if ultraNB21.Status != "available" {
		t.Errorf("ultra nb21 status = %q, want available", ultraNB21.Status)
	}
	if ultraNB21.UsedRequests != 30 || ultraNB21.UsedTokens != 75000000 {
		t.Errorf("ultra nb21 used req=%d tok=%d, want 30 and 75000000", ultraNB21.UsedRequests, ultraNB21.UsedTokens)
	}
	if ultraNB21.LimitTokens != 0 || ultraNB21.EffectiveRemTokens != 0 {
		t.Errorf("ultra nb21 token fields = (%d,%d), want (0,0)", ultraNB21.LimitTokens, ultraNB21.EffectiveRemTokens)
	}
	if ultraNB21.EffectivePercent != 80 {
		t.Errorf("ultra nb21 effective_percent = %v, want 80", ultraNB21.EffectivePercent)
	}

	// Check Free account on nb21: ineligible
	freeAcc := accByTier["Free"][0]
	freeNB21 := findAccModel(freeAcc, "gemini-nano-banana-2.1")
	if freeNB21.Status != "ineligible" {
		t.Errorf("free nb21 status = %q, want ineligible", freeNB21.Status)
	}

	// Check Pro accounts on nb21: one exhausted_requests (0%), one available 100%
	var sawExhaustedReq, sawFull100 bool
	for _, pAcc := range accByTier["Pro"] {
		m := findAccModel(pAcc, "gemini-nano-banana-2.1")
		if m.Status == "exhausted_requests" && m.EffectivePercent == 0 && m.UsedRequests == 50 {
			sawExhaustedReq = true
		}
		if m.Status == "available" && m.EffectivePercent == 100 && m.UsedRequests == 0 && m.EffectiveRemReq == 50 {
			sawFull100 = true
		}
	}
	if !sawExhaustedReq || !sawFull100 {
		t.Errorf("expected one Pro exhausted_requests (saw=%v) and one Pro 100%% available (saw=%v)", sawExhaustedReq, sawFull100)
	}

	// 2. Check gemini-3-pro-image cooldown merging (max remaining_seconds retained regardless of order!)
	nbPro := modelByID["gemini-3-pro-image"]
	if nbPro.CoolingAccounts != 2 || nbPro.AvailableAccounts != 1 {
		t.Errorf("nbPro cooling=%d avail=%d, want 2 and 1", nbPro.CoolingAccounts, nbPro.AvailableAccounts)
	}
	// LimitRequests = 100 (Ultra) + 50 + 50 (Pro) = 200; only Ultra is available (100/200 = 50%)
	if nbPro.EffectivePercent != 50 {
		t.Errorf("nbPro.EffectivePercent = %v, want 50", nbPro.EffectivePercent)
	}
	// Verify both Pro accounts kept their LONGER cooldown (~420s and ~300s), NOT 90s or 60s
	var sawLongUntil, sawLongRemSec bool
	for _, pAcc := range accByTier["Pro"] {
		m := findAccModel(pAcc, "gemini-3-pro-image")
		if m.Status != "cooldown" || m.EffectivePercent != 0 {
			t.Fatalf("expected Pro account %s on nbPro in cooldown with 0%%, got status=%s pct=%v", pAcc.CodeName, m.Status, m.EffectivePercent)
		}
		if m.CooldownUntilMs == cdLong.UnixMilli() {
			sawLongUntil = true
		} else {
			remSec := float64(m.CooldownUntilMs-now.UnixMilli()) / 1000.0
			if remSec >= 290 && remSec <= 315 {
				sawLongRemSec = true
			}
		}
	}
	if !sawLongUntil {
		t.Errorf("expected acc-pro-1 to retain longer Until cooldown (%d), not shorter (%d)", cdLong.UnixMilli(), cdShort.UnixMilli())
	}
	if !sawLongRemSec {
		t.Errorf("expected acc-pro-2 to retain longer RemainingSeconds cooldown (~300s), not shorter (60s)")
	}

	// 3. Check gemini-3.1-flash-lite-image: Free eligible (75/100 = 75%), Ultra cooling (300s retained, NOT 60s)
	ultraLite := findAccModel(ultraAcc, "gemini-3.1-flash-lite-image")
	if ultraLite.Status != "cooldown" || ultraLite.CooldownUntilMs != cdOtherLong.UnixMilli() {
		t.Errorf("ultra lite cooldown = (%s, %d), want (cooldown, %d)", ultraLite.Status, ultraLite.CooldownUntilMs, cdOtherLong.UnixMilli())
	}
	freeLite := findAccModel(freeAcc, "gemini-3.1-flash-lite-image")
	if freeLite.Status != "available" || freeLite.EffectivePercent != 75 || freeLite.UsedTokens != 42000 {
		t.Errorf("free lite = (status=%s, pct=%v, usedTok=%d), want (available, 75, 42000)", freeLite.Status, freeLite.EffectivePercent, freeLite.UsedTokens)
	}

	// 4. Check text model gemini-4-neon still enforces dual-dimension token exhaustion
	var sawTokExhausted bool
	for _, pAcc := range accByTier["Pro"] {
		m := findAccModel(pAcc, "gemini-4-neon")
		if m.Status == "exhausted_tokens" && m.EffectivePercent == 0 {
			sawTokExhausted = true
		}
	}
	if !sawTokExhausted {
		t.Errorf("expected Pro account on text model gemini-4-neon to trigger exhausted_tokens")
	}
}

func TestRepoQuotasJSONValidAndConsistent(t *testing.T) {
	raw, err := os.ReadFile("quotas.json")
	if err != nil {
		t.Fatalf("read quotas.json: %v", err)
	}
	var qf QuotasFile
	if err := json.Unmarshal(raw, &qf); err != nil {
		t.Fatalf("unmarshal quotas.json: %v", err)
	}
	if len(qf.Models) != 14 {
		t.Fatalf("len(quotas.Models) = %d, want 14", len(qf.Models))
	}

	aliasMap := buildModelAliasMap(&qf)
	imageCount := 0
	for _, m := range qf.Models {
		if m.Category == "image" {
			imageCount++
			for tier, lim := range m.Tiers {
				if lim.Tokens != 0 {
					t.Errorf("image model %s tier %s has tokens=%d, want 0", m.ID, tier, lim.Tokens)
				}
			}
			for _, a := range m.Aliases {
				if got := normalizeModelID(a, aliasMap); got != m.ID {
					t.Errorf("alias %q resolved to %q, want %q", a, got, m.ID)
				}
			}
		}
	}
	if imageCount != 4 {
		t.Errorf("imageCount = %d, want 4", imageCount)
	}
}
