package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolveEffectiveResetTimeline(t *testing.T) {
	const dur5h int64 = 5 * 3600
	base := int64(1700000000)

	t.Run("multi-step forward lookahead confirms anchored reset", func(t *testing.T) {
		reset1 := base + 3600
		reset2 := reset1 + dur5h
		pts := []lampHistoryPoint{
			{T: reset1 - 30, R: 97.4, X: reset1},
			// Two rapid 100% samples right after reset before consumption starts
			{T: reset1 + 30, R: 100, X: reset2},
			{T: reset1 + 70, R: 100, X: reset2},
			{T: reset1 + 110, R: 99.8, X: reset2},
		}
		eff := resolveEffectiveResetTimeline(pts, reset1+600, dur5h)
		for i, want := range []int64{reset1, reset2, reset2, reset2} {
			if eff[i] != want {
				t.Fatalf("eff[%d] = %d, want %d", i, eff[i], want)
			}
		}
	})

	t.Run("unopened sliding window stays zero even with rapid polls", func(t *testing.T) {
		pts := []lampHistoryPoint{
			{T: base, R: 100, X: base + dur5h},
			{T: base + 20, R: 100, X: base + 20 + dur5h},
			{T: base + 900, R: 100, X: base + 900 + dur5h},
		}
		eff := resolveEffectiveResetTimeline(pts, base+1000, dur5h)
		for i, got := range eff {
			if got != 0 {
				t.Fatalf("eff[%d] = %d, want 0 for sliding unopened window", i, got)
			}
		}
	})

	t.Run("tail fresh-reset protection prevents 1-2m flicker", func(t *testing.T) {
		reset1 := base + 3600
		reset2 := reset1 + dur5h
		pts := []lampHistoryPoint{
			{T: reset1 - 30, R: 96.5, X: reset1},
			{T: reset1 + 30, R: 100, X: reset2},
			{T: reset1 + 65, R: 100, X: reset2},
		}
		eff := resolveEffectiveResetTimeline(pts, reset1+90, dur5h)
		if eff[1] != reset2 || eff[2] != reset2 {
			t.Fatalf("tail fresh reset not confirmed: eff=%v, want [%d %d %d]", eff, reset1, reset2, reset2)
		}
	})

	t.Run("single-point transient unopened glitch is smoothed", func(t *testing.T) {
		const dur7d int64 = 7 * 86400
		reset1 := base + 2*86400
		pts := []lampHistoryPoint{
			{T: base, R: 99.2, X: reset1},
			{T: base + 400, R: 100, X: base + 400 + dur7d}, // transient unopened glitch
			{T: base + 900, R: 99.2, X: reset1},
		}
		eff := resolveEffectiveResetTimeline(pts, base+1800, dur7d)
		for i, got := range eff {
			if got != reset1 {
				t.Fatalf("eff[%d] = %d, want %d (smoothed glitch)", i, got, reset1)
			}
		}
	})
}

func TestIsCycleReset(t *testing.T) {
	base := int64(1700000000)

	cases := []struct {
		name    string
		prev    lampHistoryPoint
		cur     lampHistoryPoint
		prevEff int64
		curEff  int64
		want    bool
	}{
		{
			name:    "condition A micro-consumption reset",
			prev:    lampHistoryPoint{T: base, R: 97.4, X: base + 30},
			cur:     lampHistoryPoint{T: base + 60, R: 100, X: base + 18030},
			prevEff: base + 30,
			curEff:  base + 18030,
			want:    true,
		},
		{
			name:    "condition A immediate burn delta r <= 0",
			prev:    lampHistoryPoint{T: base, R: 98.0, X: base + 30},
			cur:     lampHistoryPoint{T: base + 60, R: 96.5, X: base + 18030},
			prevEff: base + 30,
			curEff:  base + 18030,
			want:    true,
		},
		{
			name:    "condition B consumed cycle resets to unopened 100%",
			prev:    lampHistoryPoint{T: base, R: 99.2, X: base + 10000},
			cur:     lampHistoryPoint{T: base + 900, R: 100, X: base + 900 + 7*86400},
			prevEff: base + 10000,
			curEff:  0,
			want:    true,
		},
		{
			name:    "condition C missing x sharp jump fallback",
			prev:    lampHistoryPoint{T: base, R: 80.0, X: 0},
			cur:     lampHistoryPoint{T: base + 900, R: 100.0, X: 0},
			prevEff: 0,
			curEff:  0,
			want:    true,
		},
		{
			name:    "exclude first consumption opening unopened 100% window",
			prev:    lampHistoryPoint{T: base, R: 100, X: base + 7*86400},
			cur:     lampHistoryPoint{T: base + 900, R: 99.8, X: base + 900 + 7*86400 - 300},
			prevEff: 0,
			curEff:  base + 900 + 7*86400 - 300,
			want:    false,
		},
		{
			name:    "exclude full idle window releasing back to sliding window",
			prev:    lampHistoryPoint{T: base, R: 100, X: base + 600000},
			cur:     lampHistoryPoint{T: base + 900, R: 100, X: base + 900 + 7*86400},
			prevEff: base + 600000,
			curEff:  0,
			want:    false,
		},
		{
			name:    "same cycle small upward jitter is not a reset",
			prev:    lampHistoryPoint{T: base, R: 96.2, X: base + 10000},
			cur:     lampHistoryPoint{T: base + 900, R: 98.6, X: base + 10000},
			prevEff: base + 10000,
			curEff:  base + 10000,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isCycleReset(tc.prev, tc.cur, tc.prevEff, tc.curEff)
			if got != tc.want {
				t.Fatalf("isCycleReset() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDownsamplePreservesMicroResetAndFiltersGlitch(t *testing.T) {
	const dur7d int64 = 7 * 86400
	const bucket4h int64 = 14400
	// Align base to a 4h bucket boundary
	base := int64(1700000000/bucket4h) * bucket4h

	resetA := base + 6*86400
	resetB := base + 3*3600 + 6*60 + dur7d // micro-reset inside bucket 0 (+3h06m)
	resetC := base + 4*3600 + 12*60 + dur7d

	tPreLow := base + 2*3600 + 30*60
	tPreReset := base + 3*3600
	tMicroReset := base + 3*3600 + 6*60 + 30 // 100% peak right after reset
	tPostBurn := base + 3*3600 + 45*60       // lower point in SAME 4h bucket
	tSecondReset := base + 4*3600 + 12*60    // early reset with slight consumption

	pts := []lampHistoryPoint{
		{T: base + 900, R: 99.2, X: resetA},
		{T: base + 1200, R: 100, X: base + 1200 + dur7d}, // single-point glitch (should be smoothed!)
		{T: base + 1800, R: 99.2, X: resetA},
		{T: tPreLow, R: 98.5, X: resetA},
		{T: tPreReset, R: 98.6, X: resetA},
		{T: tMicroReset, R: 100.0, X: resetB},
		{T: base + 3*3600 + 15*60, R: 98.7, X: resetB},
		{T: tPostBurn, R: 96.9, X: resetB},
		{T: base + 4*3600 + 1, R: 95.8, X: resetB},
		{T: tSecondReset, R: 99.99, X: resetC},
		{T: base + 5*3600, R: 94.9, X: resetC},
	}

	toSec := base + 6*3600
	eff := resolveEffectiveResetTimeline(pts, toSec, dur7d)
	sampled := downsampleHistoryPoints(pts, eff, base, toSec, bucket4h)
	cycleResets := extractCycleResets(sampled)
	resets := extractEffectiveResets(pts, eff, base, toSec, dur7d)

	// Verify 100% micro-reset peak and pre-reset low were both preserved in downsampled output
	foundPreLow, foundMicroPeak, foundSecond := false, false, false
	for _, p := range sampled {
		if p.T == tPreLow && p.R == 98.5 {
			foundPreLow = true
		}
		if p.T == tMicroReset && p.R == 100.0 {
			foundMicroPeak = true
		}
		if p.T == tSecondReset && p.R == 99.99 {
			foundSecond = true
		}
	}
	if !foundPreLow || !foundMicroPeak || !foundSecond {
		t.Fatalf("missing preserved points: preLow=%v microPeak=%v second=%v sampled=%+v", foundPreLow, foundMicroPeak, foundSecond, sampled)
	}

	// Verify exact cycle resets (glitch NOT included)
	if len(cycleResets) != 2 || cycleResets[0] != tMicroReset || cycleResets[1] != tSecondReset {
		t.Fatalf("cycleResets = %v, want [%d %d]", cycleResets, tMicroReset, tSecondReset)
	}

	// Verify resets transitions contain no glitch (only baseline resetA -> resetB -> resetC)
	if len(resets) != 3 || resets[0][1] != resetA || resets[1][1] != resetB || resets[2][1] != resetC {
		t.Fatalf("resets = %v, want 3 clean transitions [%d, %d, %d]", resets, resetA, resetB, resetC)
	}
}

func TestGetAccountHistoryEndToEnd(t *testing.T) {
	const dur5h int64 = 5 * 3600
	now := time.Now().Unix()
	reset3 := now - 45
	reset2 := reset3 - dur5h
	reset1 := reset2 - dur5h
	reset4 := reset3 + dur5h

	pts := []lampHistoryPoint{
		{T: reset1 - 60, R: 94.0, X: reset1},
		{T: reset1 + 30, R: 100.0, X: reset2},
		{T: reset1 + 900, R: 96.0, X: reset2},
		{T: reset2 - 30, R: 95.3, X: reset2}, // micro-burn < 5%
		{T: reset2 + 30, R: 100.0, X: reset3},
		{T: reset2 + 900, R: 98.2, X: reset3},
		{T: reset3 - 30, R: 97.4, X: reset3}, // micro-burn < 5%
		{T: reset3 + 30, R: 100.0, X: reset4},
	}

	c := NewCPACollector(CPAConfig{})
	c.histStore = &cpaHistoryStore{
		UpdatedAtSec: now,
		Accounts: map[string]*cpaAccountHistoryStore{
			"ag-ultra-1": {
				PublicID: "ag-ultra-1",
				Tier:     "Ultra",
				Windows: map[string]*cpaWindowHistory{
					"gemini_5h": {
						Key:             "gemini_5h",
						Group:           "gemini",
						Label:           "Gemini",
						WindowKind:      "5h",
						DurationSeconds: dur5h,
						Remaining:       100.0,
						ResetSec:        0, // simulate un-anchored effectiveResetSec right after fresh tail reset
						RawResetSec:     reset4,
						ObservedSec:     reset3 + 30,
						Points:          pts,
					},
				},
			},
		},
	}

	resp, ok := c.GetAccountHistory("ag-ultra-1", "5h", "24h")
	if !ok || resp == nil || len(resp.Series) != 1 {
		t.Fatalf("GetAccountHistory failed: ok=%v resp=%+v", ok, resp)
	}
	s := resp.Series[0]
	if len(s.CycleResets) != 3 {
		t.Fatalf("CycleResets = %v, want 3 entries", s.CycleResets)
	}
	if s.CycleResets[0] != reset1+30 || s.CycleResets[1] != reset2+30 || s.CycleResets[2] != reset3+30 {
		t.Fatalf("unexpected CycleResets = %v", s.CycleResets)
	}
	if s.ResetSec != reset4 {
		t.Fatalf("tail-protected ResetSec = %d, want %d", s.ResetSec, reset4)
	}
}

func TestBuildAccountWindowsPriorityRules(t *testing.T) {
	authIdx := "0123456789abcdef"
	base := time.Date(2026, 11, 24, 7, 30, 0, 0, time.UTC)
	resetAt := base.Add(3*time.Hour + 33*time.Minute + 16*time.Second)
	nowMs := base.Add(5 * time.Minute).UnixMilli()

	t.Run("never overwrites lamplighter float with fresher priority integer", func(t *testing.T) {
		lamp := &rawLamplighterState{
			Groups: map[string]rawLampGroup{
				"antigravity:" + authIdx + ":antigravity:gemini-models": {
					LastSeen: base.Unix(),
					Windows: map[string]rawLampWindow{
						"gemini-5h": {
							Remaining: 72.5111,
							Reset:     resetAt.Format(time.RFC3339),
							LastSeen:  base.Unix(),
						},
					},
				},
			},
		}
		prio := &rawPriorityState{
			Entries: map[string]rawPriorityEntry{
				authIdx + "|model_group=gemini": {
					ObservedAt:           base.Add(4 * time.Minute).Format(time.RFC3339Nano), // > 2m newer than lamplighter!
					ShortWindowRemaining: 73,                                                 // rounded integer!
					ShortWindowResetAt:   resetAt.Format(time.RFC3339),
				},
			},
		}

		wins := buildAccountWindows(authIdx, "Ultra", lamp, prio, nowMs)
		if len(wins) != 1 {
			t.Fatalf("got %d windows, want 1", len(wins))
		}
		w := wins[0]
		if math.Abs(w.remainingPercent-72.5111) > 1e-6 {
			t.Fatalf("remainingPercent = %v, want 72.5111 (must not overwrite with integer 73)", w.remainingPercent)
		}
		if w.observedAtMs != base.UnixMilli() {
			t.Fatalf("observedAtMs = %d, want %d", w.observedAtMs, base.UnixMilli())
		}
		if w.fromPriority {
			t.Fatalf("fromPriority = true, want false")
		}
	})

	t.Run("does not trigger false reset on unopened 100% sliding window", func(t *testing.T) {
		unopenedReset := base.Add(5 * time.Hour)
		lamp := &rawLamplighterState{
			Groups: map[string]rawLampGroup{
				"antigravity:" + authIdx + ":antigravity:claude-and-gpt-models": {
					LastSeen: base.Unix(),
					Windows: map[string]rawLampWindow{
						"3p-5h": {
							Remaining: 100.0,
							Reset:     unopenedReset.Format(time.RFC3339),
							LastSeen:  base.Unix(),
						},
					},
				},
			},
		}
		prio := &rawPriorityState{
			Entries: map[string]rawPriorityEntry{
				authIdx + "|model_group=claude_gpt": {
					ObservedAt:           base.Add(5 * time.Minute).Format(time.RFC3339Nano),
					ShortWindowRemaining: 100,
					ShortWindowResetAt:   unopenedReset.Add(5 * time.Minute).Format(time.RFC3339), // sliding!
				},
			},
		}
		wins := buildAccountWindows(authIdx, "Ultra", lamp, prio, nowMs)
		if len(wins) != 1 || wins[0].fromPriority {
			t.Fatalf("unopened sliding window should stay on lamplighter, got %+v", wins)
		}
	})

	t.Run("adopts priority when lamplighter lacks window", func(t *testing.T) {
		prio := &rawPriorityState{
			Entries: map[string]rawPriorityEntry{
				authIdx + "|model_group=gemini": {
					ObservedAt:           base.Add(2 * time.Minute).Format(time.RFC3339),
					ShortWindowRemaining: 88,
					ShortWindowResetAt:   resetAt.Format(time.RFC3339),
				},
			},
		}
		wins := buildAccountWindows(authIdx, "Ultra", nil, prio, nowMs)
		if len(wins) == 0 || wins[0].key != "gemini_5h" || wins[0].remainingPercent != 88 || !wins[0].fromPriority {
			t.Fatalf("expected priority fallback when lamplighter missing, got %+v", wins)
		}
	})

	t.Run("adopts priority on real new cycle reset", func(t *testing.T) {
		nextReset := resetAt.Add(5 * time.Hour)
		lamp := &rawLamplighterState{
			Groups: map[string]rawLampGroup{
				"antigravity:" + authIdx + ":antigravity:gemini-models": {
					LastSeen: base.Unix(),
					Windows: map[string]rawLampWindow{
						"gemini-5h": {
							Remaining: 42.345,
							Reset:     resetAt.Format(time.RFC3339),
							LastSeen:  base.Unix(),
						},
					},
				},
			},
		}
		prio := &rawPriorityState{
			Entries: map[string]rawPriorityEntry{
				authIdx + "|model_group=gemini": {
					ObservedAt:           resetAt.Add(30 * time.Second).Format(time.RFC3339),
					ShortWindowRemaining: 100,
					ShortWindowResetAt:   nextReset.Format(time.RFC3339),
				},
			},
		}
		wins := buildAccountWindows(authIdx, "Ultra", lamp, prio, resetAt.Add(45*time.Second).UnixMilli())
		if len(wins) != 1 {
			t.Fatalf("got %d windows, want 1", len(wins))
		}
		w := wins[0]
		if w.remainingPercent != 100 || w.resetAtMs != nextReset.UnixMilli() || !w.fromPriority || !w.priorityResetOrExhausted {
			t.Fatalf("expected priority real cycle reset adoption, got %+v", w)
		}
	})

	t.Run("adopts priority on exhaustion circuit-breaker", func(t *testing.T) {
		lamp := &rawLamplighterState{
			Groups: map[string]rawLampGroup{
				"antigravity:" + authIdx + ":antigravity:gemini-models": {
					LastSeen: base.Unix(),
					Windows: map[string]rawLampWindow{
						"gemini-5h": {
							Remaining: 4.567,
							Reset:     resetAt.Format(time.RFC3339),
							LastSeen:  base.Unix(),
						},
					},
				},
			},
		}
		prio := &rawPriorityState{
			Entries: map[string]rawPriorityEntry{
				authIdx + "|model_group=gemini": {
					ObservedAt:           base.Add(3 * time.Minute).Format(time.RFC3339),
					ShortWindowRemaining: 0,
					ShortWindowResetAt:   resetAt.Format(time.RFC3339),
				},
			},
		}
		wins := buildAccountWindows(authIdx, "Ultra", lamp, prio, nowMs)
		if len(wins) != 1 {
			t.Fatalf("got %d windows, want 1", len(wins))
		}
		w := wins[0]
		if w.remainingPercent != 0 || !w.fromPriority || !w.priorityResetOrExhausted {
			t.Fatalf("expected priority exhaustion adoption, got %+v", w)
		}
	})
}

func TestRefreshHistoryStoreNoIntegerPollutionAndEstimatedPoint(t *testing.T) {
	tmpDir := t.TempDir()
	histDir := filepath.Join(tmpDir, "history")
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}
	authIdx := "abcdef0123456789"
	now := time.Now().Truncate(time.Second)
	t1 := now.Add(-20 * time.Minute).Unix()
	t2 := now.Add(-5 * time.Minute).Unix()
	resetSec := now.Add(4 * time.Hour).Unix()

	jsonl := "" +
		`{"k":"s","t":` + strconvInt(t1) + `,"g":"antigravity:` + authIdx + `:antigravity:gemini-models","w":"gemini-5h","r":82.529,"x":` + strconvInt(resetSec) + `}` + "\n" +
		`{"k":"s","t":` + strconvInt(t2) + `,"g":"antigravity:` + authIdx + `:antigravity:gemini-models","w":"gemini-5h","r":72.511,"x":` + strconvInt(resetSec) + `}` + "\n"
	if err := os.WriteFile(filepath.Join(histDir, "2026-11-24.jsonl"), []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewCPACollector(CPAConfig{LamplighterDir: tmpDir})
	estRem := 69.123
	acc := &internalCPAAccount{
		authIndex: authIdx,
		publicID:  "ag-ultra-1",
		tier:      "Ultra",
		windows: []internalCPAWindow{
			{
				key:                       "gemini_5h",
				group:                     "gemini",
				label:                     "Gemini 5h",
				durationSeconds:           5 * 3600,
				remainingPercent:          73, // simulate non-reset integer priority point
				resetAtMs:                 resetSec * 1000,
				observedAtMs:              (t2 + 120) * 1000,
				fromPriority:              true,
				priorityResetOrExhausted:  false,
				hasPostSampleCheck:        true,
				postSampleRequests:        12,
				postSampleCostUSD:         16.2,
				estimatedRemainingPercent: &estRem,
			},
		},
	}

	c.refreshHistoryStore([]*internalCPAAccount{acc}, now)
	resp, ok := c.GetAccountHistory("ag-ultra-1", "5h", "24h")
	if !ok || len(resp.Series) != 1 {
		t.Fatalf("GetAccountHistory failed: ok=%v resp=%+v", ok, resp)
	}
	s := resp.Series[0]
	// Points MUST remain strictly the 2 real .jsonl float points (no integer 73 appended, no estimated_point mixed in!)
	if len(s.Points) != 2 {
		t.Fatalf("len(Points) = %d, want 2 (must not append non-reset integer or estimated point): %v", len(s.Points), s.Points)
	}
	if s.Points[1][1] != 72.511 {
		t.Fatalf("last point remaining = %v, want 72.511", s.Points[1][1])
	}
	if len(s.EstimatedPoint) != 2 || s.EstimatedPoint[1] != 69.123 {
		t.Fatalf("EstimatedPoint = %v, want [%d, 69.123]", s.EstimatedPoint, now.Unix())
	}
	if s.PostSampleRequests == nil || *s.PostSampleRequests != 12 {
		t.Fatalf("PostSampleRequests = %v, want 12", s.PostSampleRequests)
	}
}

func strconvInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestApplyWindowUsageForecastAndPostSampleEstimate(t *testing.T) {
	winStart := int64(1700000000000)
	obsMs := winStart + 90*60*1000
	curTo := obsMs + 10*60*1000

	t.Run("normal active window computes quota forecast and post-sample estimated remaining", func(t *testing.T) {
		w := &internalCPAWindow{
			key:              "gemini_5h",
			durationSeconds:  5 * 3600,
			remainingPercent: 72.511,
			resetAtMs:        winStart + 5*3600*1000,
			observedAtMs:     obsMs,
		}
		cur := &cpampWindowUsageItem{Matched: true, TotalRequests: 925, TotalTokens: 315_000_000, TotalCost: 136.67}
		fb := &cpampWindowUsageItem{Matched: true, TotalRequests: 915, TotalTokens: 312_000_000, TotalCost: 131.97}
		prev := &cpampWindowUsageItem{Matched: true, TotalRequests: 2500, TotalTokens: 1_000_000_000, TotalCost: 470.00}

		applyWindowUsageForecast(w, cur, fb, prev, winStart, curTo)

		if !w.hasForecast || w.forecastBasis != "quota" {
			t.Fatalf("forecast = (%v, %s), want (true, quota)", w.hasForecast, w.forecastBasis)
		}
		wantForecastCost := round2(131.97 * (100.0 / (100.0 - 72.511)))
		if math.Abs(w.forecastCost-wantForecastCost) > 0.02 {
			t.Fatalf("forecastCost = %v, want %v", w.forecastCost, wantForecastCost)
		}
		if math.Abs(w.postSampleCostUSD-4.70) > 1e-6 || w.postSampleTokens != 3_000_000 || w.postSampleRequests != 10 {
			t.Fatalf("postSample = (cost=%v, tok=%d, req=%d), want (4.70, 3000000, 10)", w.postSampleCostUSD, w.postSampleTokens, w.postSampleRequests)
		}
		if w.estimatedRemainingPercent == nil {
			t.Fatalf("estimatedRemainingPercent is nil")
		}
		wantEst := round3(72.511 - (4.70/w.forecastCost)*100.0)
		if math.Abs(*w.estimatedRemainingPercent-wantEst) > 0.002 {
			t.Fatalf("estimatedRemainingPercent = %v, want %v", *w.estimatedRemainingPercent, wantEst)
		}
	})

	t.Run("tiny usedPct (< 0.5%) avoids noisy clamp and falls back to previous", func(t *testing.T) {
		w := &internalCPAWindow{
			key:              "gemini_5h",
			durationSeconds:  5 * 3600,
			remainingPercent: 99.8, // usedPct = 0.2% < 0.5%
			resetAtMs:        winStart + 5*3600*1000,
			observedAtMs:     obsMs,
		}
		cur := &cpampWindowUsageItem{Matched: true, TotalRequests: 300, TotalTokens: 150_000_000, TotalCost: 80.00}
		fb := &cpampWindowUsageItem{Matched: true, TotalRequests: 2, TotalTokens: 1_000_000, TotalCost: 0.50}
		prev := &cpampWindowUsageItem{Matched: true, TotalRequests: 2000, TotalTokens: 1_000_000_000, TotalCost: 480.00}

		applyWindowUsageForecast(w, cur, fb, prev, winStart, curTo)

		if !w.hasForecast || w.forecastBasis != "previous" || w.forecastCost != 480.00 {
			t.Fatalf("expected previous fallback $480, got has=%v basis=%s cost=%v", w.hasForecast, w.forecastBasis, w.forecastCost)
		}
		if w.estimatedRemainingPercent == nil {
			t.Fatalf("expected estimatedRemainingPercent from previous fullCost")
		}
		wantEst := round3(99.8 - (79.50/480.00)*100.0)
		if math.Abs(*w.estimatedRemainingPercent-wantEst) > 0.01 {
			t.Fatalf("estimatedRemainingPercent = %v, want %v", *w.estimatedRemainingPercent, wantEst)
		}
	})

	t.Run("post-sample burst exceeding raw estimate while remaining > 1.5% never clamps to current", func(t *testing.T) {
		w := &internalCPAWindow{
			key:              "gemini_5h",
			durationSeconds:  5 * 3600,
			remainingPercent: 99.0, // usedPct = 1.0% >= 0.5%
			resetAtMs:        winStart + 5*3600*1000,
			observedAtMs:     obsMs,
		}
		cur := &cpampWindowUsageItem{Matched: true, TotalRequests: 300, TotalTokens: 150_000_000, TotalCost: 80.00}
		fb := &cpampWindowUsageItem{Matched: true, TotalRequests: 2, TotalTokens: 1_000_000, TotalCost: 0.50} // rawEst = $50 < $80

		// Without previous: must NOT fabricate contradictory "$80 used / $80 full"
		applyWindowUsageForecast(w, cur, fb, nil, winStart, curTo)
		if w.hasForecast {
			t.Fatalf("expected no self-contradictory forecast when cur > est and rem > 1.5%% without prev, got basis=%s cost=%v", w.forecastBasis, w.forecastCost)
		}
	})

	t.Run("near exhaustion (<= 1.5%) allows lower-bound clamp", func(t *testing.T) {
		w := &internalCPAWindow{
			key:              "gemini_5h",
			durationSeconds:  5 * 3600,
			remainingPercent: 1.0,
			resetAtMs:        winStart + 5*3600*1000,
			observedAtMs:     obsMs,
		}
		cur := &cpampWindowUsageItem{Matched: true, TotalRequests: 2010, TotalTokens: 1_005_000_000, TotalCost: 485.00}
		fb := &cpampWindowUsageItem{Matched: true, TotalRequests: 2000, TotalTokens: 990_000_000, TotalCost: 475.00} // rawEst = 479.80 < 485

		applyWindowUsageForecast(w, cur, fb, nil, winStart, curTo)
		if !w.hasForecast || w.forecastBasis != "quota" || w.forecastCost != 485.00 || w.forecastTokens != 1_005_000_000 {
			t.Fatalf("expected near-exhaustion clamp to cur, got has=%v basis=%s cost=%v tok=%d", w.hasForecast, w.forecastBasis, w.forecastCost, w.forecastTokens)
		}
	})
}

func TestForecastBaseMemoryCache(t *testing.T) {
	var fbRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req cpampWindowUsageReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad req", 400)
			return
		}
		items := make([]cpampWindowUsageItem, 0, len(req.Windows))
		for _, tgt := range req.Windows {
			if filepath.Ext(tgt.RequestKey) == "" && len(tgt.RequestKey) > 0 {
				// Identify forecast_base targets
				if len(tgt.RequestKey) >= 13 && tgt.RequestKey[len(tgt.RequestKey)-13:] == "forecast_base" {
					fbRequests.Add(1)
					items = append(items, cpampWindowUsageItem{
						RequestKey:    tgt.RequestKey,
						Matched:       true,
						TotalRequests: 500,
						TotalTokens:   200_000_000,
						TotalCost:     100.0,
					})
					continue
				}
			}
			items = append(items, cpampWindowUsageItem{
				RequestKey:    tgt.RequestKey,
				Matched:       true,
				TotalRequests: 520,
				TotalTokens:   208_000_000,
				TotalCost:     104.0,
			})
		}
		_ = json.NewEncoder(w).Encode(cpampWindowUsageResp{Items: items})
	}))
	defer srv.Close()

	c := NewCPACollector(CPAConfig{CPAMPBaseURL: srv.URL})
	nowMs := time.Now().UnixMilli()
	resetMs := nowMs + 3*3600*1000
	obsMs := nowMs - 5*60*1000

	makeAcc := func() []*internalCPAAccount {
		return []*internalCPAAccount{
			{
				rowKey:    "ag-row-0",
				authIndex: "idx1",
				publicID:  "ag-ultra-1",
				windows: []internalCPAWindow{
					{
						key:              "gemini_5h",
						providerWindowID: "gemini-5h",
						modelScopeKey:    "gemini",
						durationSeconds:  5 * 3600,
						remainingPercent: 75.0,
						resetAtMs:        resetMs,
						observedAtMs:     obsMs,
					},
				},
			},
		}
	}

	// First call: queries forecast_base from CPAMP and caches it
	accs1 := makeAcc()
	if _, err := c.enrichWindowUsage(context.Background(), "test-key", accs1, nowMs); err != nil {
		t.Fatalf("first enrichWindowUsage failed: %v", err)
	}
	if fbRequests.Load() != 1 {
		t.Fatalf("fbRequests after 1st call = %d, want 1", fbRequests.Load())
	}
	if accs1[0].windows[0].forecastCost != 400.0 || accs1[0].windows[0].postSampleCostUSD != 4.0 {
		t.Fatalf("unexpected 1st forecast/postSample: %+v", accs1[0].windows[0])
	}

	// Second call 15s later with same (account, window, winStart, obs): MUST hit memory cache (0 new forecast_base calls)
	accs2 := makeAcc()
	if _, err := c.enrichWindowUsage(context.Background(), "test-key", accs2, nowMs+15_000); err != nil {
		t.Fatalf("second enrichWindowUsage failed: %v", err)
	}
	if fbRequests.Load() != 1 {
		t.Fatalf("fbRequests after 2nd call = %d, want 1 (should hit cache)", fbRequests.Load())
	}
	if accs2[0].windows[0].forecastCost != 400.0 || accs2[0].windows[0].postSampleCostUSD != 4.0 {
		t.Fatalf("unexpected 2nd cached forecast/postSample: %+v", accs2[0].windows[0])
	}
}

func TestRefreshCoordinatorDebounceSingleflightAndRerun(t *testing.T) {
	var fakeNowMu sync.Mutex
	fakeNow := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	getNow := func() time.Time {
		fakeNowMu.Lock()
		defer fakeNowMu.Unlock()
		return fakeNow
	}
	advance := func(d time.Duration) {
		fakeNowMu.Lock()
		fakeNow = fakeNow.Add(d)
		fakeNowMu.Unlock()
	}

	var runs atomic.Int32
	blockCh := make(chan struct{})
	startedCh := make(chan struct{}, 4)

	rc := NewRefreshCoordinator(4*time.Second, func(ctx context.Context) {
		n := runs.Add(1)
		startedCh <- struct{}{}
		if n == 2 {
			<-blockCh
		}
	})
	rc.nowFn = getNow

	ctx := context.Background()

	// 1. Initial blocking run
	rc.RunBlocking(ctx)
	<-startedCh
	if runs.Load() != 1 {
		t.Fatalf("runs = %d, want 1", runs.Load())
	}

	// 2. ?fresh=1 within < 4s must be debounced
	advance(2 * time.Second)
	if refreshed := rc.RefreshIfStale(ctx, ctx); refreshed {
		t.Fatalf("expected RefreshIfStale within 2s to debounce (return false)")
	}
	if runs.Load() != 1 {
		t.Fatalf("runs after debounce = %d, want 1", runs.Load())
	}

	// 3. ?fresh=1 after >= 4s triggers run #2; concurrent caller coalesces via singleflight;
	//    a mid-flight file-change trigger queues run #3!
	advance(3 * time.Second) // total 5s >= 4s
	var wg sync.WaitGroup
	wg.Add(2)
	var r1, r2 bool
	go func() {
		defer wg.Done()
		r1 = rc.RefreshIfStale(ctx, ctx)
	}()
	<-startedCh // run #2 is now in flight
	go func() {
		defer wg.Done()
		r2 = rc.RefreshIfStale(ctx, ctx)
	}()
	// Give second caller a moment to attach to doneCh, then fire mid-flight lamplighter rerun trigger
	time.Sleep(25 * time.Millisecond)
	rc.TriggerBackground(ctx, true)
	close(blockCh)
	wg.Wait()

	if !r1 || !r2 {
		t.Fatalf("expected both coalesced fresh callers to return true, got r1=%v r2=%v", r1, r2)
	}
	// Wait for queued rerun #3
	select {
	case <-startedCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for queued mid-flight rerun")
	}
	if runs.Load() != 3 {
		t.Fatalf("runs = %d, want 3 (1 initial + 1 coalesced fresh + 1 queued file-change rerun)", runs.Load())
	}
}

func TestCheckLamplighterStateChanged(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	if err := os.WriteFile(stateFile, []byte(`{"groups":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCPACollector(CPAConfig{LamplighterDir: dir})
	// Prime
	if !c.CheckLamplighterStateChanged() {
		t.Fatalf("expected first stat check to report initial presence")
	}
	if c.CheckLamplighterStateChanged() {
		t.Fatalf("expected unchanged file to return false")
	}
	// Write updated state with different size
	if err := os.WriteFile(stateFile, []byte(`{"groups":{"a":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !c.CheckLamplighterStateChanged() {
		t.Fatalf("expected modified file to return true")
	}
	if c.CheckLamplighterStateChanged() {
		t.Fatalf("expected steady file after modification to return false")
	}
}

