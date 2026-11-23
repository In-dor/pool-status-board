package main

import (
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

