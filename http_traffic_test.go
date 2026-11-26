package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func gunzipBytes(t *testing.T, compressed []byte) []byte {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("gzip.NewReader failed: %v", err)
	}
	defer gr.Close()
	out, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("gunzip read failed: %v", err)
	}
	return out
}

func makeSyntheticSnapshot(anchorMs, wallTsMs int64, geminiRem float64) Snapshot {
	buckets := make([]AntigravityRecentBucket, 20)
	for i := range buckets {
		buckets[i] = AntigravityRecentBucket{
			Time:    fmt.Sprintf("10:%02d-10:%02d", (i*10)%60, ((i+1)*10)%60),
			Success: int64(i % 5),
			Failed:  0,
		}
	}
	hourly := make([]AIStudioHourlyBucket, 24)
	for i := range hourly {
		hourly[i] = AIStudioHourlyBucket{
			TimestampMs:     anchorMs - int64(24-i)*3600_000,
			Label:           fmt.Sprintf("%02d:00", i),
			Requests:        int64(i * 2),
			Succeeded:       int64(i * 2),
			TotalTokens:     int64(i * 15000),
			InputTokens:     int64(i * 10000),
			ReplyTokens:     int64(i * 5000),
			FirstEventP50Ms: 420.5,
			FirstEventAvgMs: 480.2,
		}
	}
	models := make([]AIStudioModelSummary, 14)
	for i := range models {
		id := fmt.Sprintf("gemini-test-model-%02d", i+1)
		cat := ""
		limTok := int64(10_000_000)
		effRemTok := int64(9_500_000)
		if i >= 10 {
			cat = "image"
			limTok = 0
			effRemTok = 0
		}
		models[i] = AIStudioModelSummary{
			ID:                 id,
			Label:              fmt.Sprintf("Test Model %02d", i+1),
			Category:           cat,
			EligibleTiers:      []string{"Ultra", "Pro"},
			EligibleAccounts:   12,
			AvailableAccounts:  12,
			LimitRequests:      600,
			UsedRequests:       12,
			EffectiveRemReq:    588,
			LimitTokens:        limTok,
			UsedTokens:         500_000,
			EffectiveRemTokens: effRemTok,
			EffectivePercent:   95.0,
		}
	}
	accounts := make([]AIStudioAccount, 12)
	for i := range accounts {
		accModels := make([]AIStudioAccountModel, 14)
		for j := range accModels {
			if j == 0 && i == 0 {
				accModels[j] = AIStudioAccountModel{
					ModelID:     models[j].ID,
					Status:      "ineligible",
					StatusLabel: "档位不支持",
				}
				continue
			}
			limTok := int64(1_000_000)
			effRemTok := int64(1_000_000)
			if j >= 10 {
				limTok = 0
				effRemTok = 0
			}
			accModels[j] = AIStudioAccountModel{
				ModelID:            models[j].ID,
				Status:             "available",
				StatusLabel:        "可用",
				LimitRequests:      50,
				UsedRequests:       0,
				EffectiveRemReq:    50,
				LimitTokens:        limTok,
				UsedTokens:         0,
				EffectiveRemTokens: effRemTok,
				EffectivePercent:   100,
			}
		}
		accounts[i] = AIStudioAccount{
			ID:                fmt.Sprintf("ais-pro-%d", i+1),
			CodeName:          fmt.Sprintf("AIS-Pro #%d", i+1),
			Tier:              "Pro",
			State:             "ready",
			StateLabel:        "就绪",
			AvailableModelCnt: 14,
			EligibleModelCnt:  14,
			Models:            accModels,
		}
	}

	estRem := geminiRem - 1.25
	return Snapshot{
		GeneratedAtMs:          wallTsMs,
		RefreshIntervalSeconds: 15,
		Antigravity: AntigravityPool{
			Online:      true,
			Status:      "online",
			CPAMPStatus: "ok",
			UpdatedAtMs: wallTsMs,
			Summary: AntigravityPoolSummary{
				ActiveAccounts:          1,
				TierCounts:              map[string]int{"Ultra": 1},
				TotalRequests:           1200,
				TotalTokens:             450_000_000,
				TotalCost:               320.5,
				WeightedSuccessRate:     floatPtr(99.5),
				WeightedCacheHitRate:    floatPtr(89.8),
				WeightedCacheHitRate24h: floatPtr(93.2),
				TotalCacheSavedCost:     20132.05,
				WindowAverages: []AntigravityWindowAverage{
					{Key: "gemini_5h", Label: "Gemini 5h", AvgRemainingPercent: geminiRem, AccountCount: 1},
				},
			},
			Accounts: []AntigravityAccount{
				{
					ID:          "ag-ultra-1",
					CodeName:    "AG-Ultra #1",
					Tier:        "Ultra",
					Status:      "available",
					StatusLabel: "可用",
					HasHistory:  true,
					History: AntigravityAccountHistory{
						Available:           true,
						TotalRequests:       1200,
						SuccessCalls:        1194,
						FailureCalls:        6,
						TotalTokens:         450_000_000,
						TotalCost:           320.5,
						SuccessRate:         floatPtr(99.5),
						InputTokens:         10_000_000_000,
						CachedTokens:        8_980_000_000,
						CacheHitRate:        floatPtr(89.8),
						CacheSavedCost:      20132.05,
						InputTokens24h:      1_000_000_000,
						CachedTokens24h:     932_000_000,
						CacheHitRate24h:     floatPtr(93.2),
						CacheSavedCost24h:   3046.21,
						LatestRequestAtMs:   anchorMs - 15_000,
						HasRecentBuckets:    true,
						RecentWindowMinutes: 200,
						RecentWindowSuccess: 40,
						RecentWindowFailure: 0,
						RecentWindowRate:    floatPtr(100),
						RecentBuckets:       buckets,
					},
					Windows: []AntigravityWindow{
						{
							Key:                       "gemini_5h",
							Label:                     "Gemini 5h",
							Group:                     "gemini",
							DurationSeconds:           5 * 3600,
							RemainingPercent:          geminiRem,
							UsedPercent:               round3(100 - geminiRem),
							ResetAtMs:                 anchorMs + 3*3600_000,
							ObservedAtMs:              anchorMs - 120_000,
							HasCurrentUsage:           true,
							CurrentRequests:           120,
							CurrentTokens:             45_000_000,
							CurrentCost:               22.5,
							HasForecast:               true,
							ForecastBasis:             "quota",
							ForecastTokens:            300_000_000,
							ForecastCost:              150.0,
							PostSampleRequests:        3,
							PostSampleCostUSD:         1.87,
							EstimatedRemainingPercent: &estRem,
						},
					},
				},
			},
		},
		AIStudio: AIStudioPool{
			Online:        true,
			Status:        "online",
			UpdatedAtMs:   wallTsMs,
			ServiceState:  "ready",
			ServiceReady:  true,
			CycleStartMs:  anchorMs - 8*3600_000,
			NextResetMs:   anchorMs + 16*3600_000,
			ResetRuleNote: "每日北京时间 16:00 重置",
			TierCounts:    map[string]int{"Pro": 12},
			AccountCounts: AIStudioAccountCounts{Enabled: 12, Ready: 12},
			Playground: AIStudioChannelSummary{
				Requests:    168,
				Succeeded:   166,
				Failed:      2,
				SuccessRate: floatPtr(98.81),
				InputTokens: 1_200_000,
				ReplyTokens: 600_000,
				TotalTokens: 1_800_000,
			},
			HourlyBuckets: hourly,
			Models:        models,
			Accounts:      accounts,
		},
	}
}

func TestStaticAssetsGzipETagAnd304(t *testing.T) {
	subWeb, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	handler, err := newAppHandler(AppConfig{RefreshIntervalSeconds: 15}, &BoardState{}, nil, nil, context.Background(), subWeb)
	if err != nil {
		t.Fatalf("newAppHandler: %v", err)
	}

	paths := []struct {
		urlPath  string
		fileName string
	}{
		{"/", "index.html"},
		{"/index.html", "index.html"},
		{"/app.js?v=20261126-3", "app.js"},
		{"/styles.css?v=20261126-3", "styles.css"},
		{"/favicon.svg?v=20261126-3", "favicon.svg"},
	}

	for _, tc := range paths {
		t.Run(tc.urlPath, func(t *testing.T) {
			origBytes, err := fs.ReadFile(subWeb, tc.fileName)
			if err != nil {
				t.Fatalf("read embedded %s: %v", tc.fileName, err)
			}

			// 1. Gzip request -> 200 OK + gzip + strong ETag + no-cache + Vary
			req := httptest.NewRequest(http.MethodGet, tc.urlPath, nil)
			req.Header.Set("Accept-Encoding", "br, gzip, deflate")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", tc.urlPath, rec.Code)
			}
			if ce := rec.Header().Get("Content-Encoding"); ce != "gzip" {
				t.Fatalf("GET %s Content-Encoding = %q, want gzip", tc.urlPath, ce)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
				t.Fatalf("GET %s Cache-Control = %q, want no-cache", tc.urlPath, cc)
			}
			if vary := rec.Header().Get("Vary"); vary != "Accept-Encoding" {
				t.Fatalf("GET %s Vary = %q, want Accept-Encoding", tc.urlPath, vary)
			}
			etagGz := rec.Header().Get("ETag")
			if !strings.HasPrefix(etagGz, `"psb-`) || !strings.HasSuffix(etagGz, `-gz"`) {
				t.Fatalf("GET %s unexpected gzip ETag = %q", tc.urlPath, etagGz)
			}
			if rec.Body.Len() >= len(origBytes) {
				t.Fatalf("GET %s gzipped %d >= raw %d", tc.urlPath, rec.Body.Len(), len(origBytes))
			}
			if decoded := gunzipBytes(t, rec.Body.Bytes()); !bytes.Equal(decoded, origBytes) {
				t.Fatalf("GET %s gunzipped content mismatch", tc.urlPath)
			}

			// 2. If-None-Match exact -> 304 Not Modified (0 B, no Content-Encoding)
			req304 := httptest.NewRequest(http.MethodGet, tc.urlPath, nil)
			req304.Header.Set("Accept-Encoding", "gzip")
			req304.Header.Set("If-None-Match", etagGz)
			rec304 := httptest.NewRecorder()
			handler.ServeHTTP(rec304, req304)

			if rec304.Code != http.StatusNotModified {
				t.Fatalf("GET %s with If-None-Match status = %d, want 304", tc.urlPath, rec304.Code)
			}
			if rec304.Body.Len() != 0 {
				t.Fatalf("304 body len = %d, want 0", rec304.Body.Len())
			}
			if ce := rec304.Header().Get("Content-Encoding"); ce != "" {
				t.Fatalf("304 must not set Content-Encoding, got %q", ce)
			}
			if gotETag := rec304.Header().Get("ETag"); gotETag != etagGz {
				t.Fatalf("304 ETag = %q, want %q", gotETag, etagGz)
			}

			// 3. Multi-value If-None-Match with W/ prefix -> 304
			reqMulti := httptest.NewRequest(http.MethodGet, tc.urlPath, nil)
			reqMulti.Header.Set("Accept-Encoding", "gzip")
			reqMulti.Header.Set("If-None-Match", `"other-etag", W/`+etagGz)
			recMulti := httptest.NewRecorder()
			handler.ServeHTTP(recMulti, reqMulti)
			if recMulti.Code != http.StatusNotModified {
				t.Fatalf("multi If-None-Match status = %d, want 304", recMulti.Code)
			}

			// 4. HEAD request -> 200 with 0 B body and Content-Length matching gzipped bytes
			reqHead := httptest.NewRequest(http.MethodHead, tc.urlPath, nil)
			reqHead.Header.Set("Accept-Encoding", "gzip")
			recHead := httptest.NewRecorder()
			handler.ServeHTTP(recHead, reqHead)
			if recHead.Code != http.StatusOK || recHead.Body.Len() != 0 {
				t.Fatalf("HEAD %s status=%d bodyLen=%d, want 200 and 0", tc.urlPath, recHead.Code, recHead.Body.Len())
			}
			if cl := recHead.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
				t.Fatalf("HEAD %s Content-Length = %q, want %d", tc.urlPath, cl, rec.Body.Len())
			}

			// 5. Identity request (gzip;q=0) -> 200 raw without Content-Encoding + distinct ETag
			reqRaw := httptest.NewRequest(http.MethodGet, tc.urlPath, nil)
			reqRaw.Header.Set("Accept-Encoding", "gzip;q=0")
			recRaw := httptest.NewRecorder()
			handler.ServeHTTP(recRaw, reqRaw)
			if recRaw.Code != http.StatusOK || recRaw.Header().Get("Content-Encoding") != "" {
				t.Fatalf("raw GET %s status=%d ce=%q", tc.urlPath, recRaw.Code, recRaw.Header().Get("Content-Encoding"))
			}
			if !bytes.Equal(recRaw.Body.Bytes(), origBytes) {
				t.Fatalf("raw GET %s bytes mismatch", tc.urlPath)
			}
			etagRaw := recRaw.Header().Get("ETag")
			if etagRaw == etagGz || !strings.HasPrefix(etagRaw, `"psb-`) {
				t.Fatalf("raw ETag = %q should differ from gzip ETag %q", etagRaw, etagGz)
			}
		})
	}
}

func TestSnapshotViewsGzipAndPayloadHierarchy(t *testing.T) {
	subWeb, _ := fs.Sub(webFS, "web")
	state := &BoardState{}
	tsMs := int64(1795680000000)
	state.Set(makeSyntheticSnapshot(tsMs, tsMs, 85.5))

	handler, err := newAppHandler(AppConfig{RefreshIntervalSeconds: 15}, state, nil, nil, context.Background(), subWeb)
	if err != nil {
		t.Fatalf("newAppHandler: %v", err)
	}

	rawSizes := map[string]int{}
	gzSizes := map[string]int{}
	decodedJSON := map[string]map[string]any{}

	for _, view := range []string{"full", "summary", "compact", "tui"} {
		url := "/api/snapshot?view=" + view
		// Uncompressed
		recRaw := httptest.NewRecorder()
		handler.ServeHTTP(recRaw, httptest.NewRequest(http.MethodGet, url, nil))
		if recRaw.Code != http.StatusOK {
			t.Fatalf("view=%s raw status=%d, want 200", view, recRaw.Code)
		}
		if recRaw.Header().Get("Content-Encoding") != "" {
			t.Fatalf("view=%s unexpected Content-Encoding without Accept-Encoding", view)
		}
		rawSizes[view] = recRaw.Body.Len()

		// Gzipped
		reqGz := httptest.NewRequest(http.MethodGet, url, nil)
		reqGz.Header.Set("Accept-Encoding", "gzip")
		recGz := httptest.NewRecorder()
		handler.ServeHTTP(recGz, reqGz)
		if recGz.Code != http.StatusOK {
			t.Fatalf("view=%s gz status=%d, want 200", view, recGz.Code)
		}
		if recGz.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("view=%s missing gzip Content-Encoding", view)
		}
		if recGz.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("view=%s Cache-Control=%q, want private, no-store", view, recGz.Header().Get("Cache-Control"))
		}
		if recGz.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("view=%s Vary=%q, want Accept-Encoding", view, recGz.Header().Get("Vary"))
		}
		if recGz.Header().Get("X-Generated-At-Ms") != strconv.FormatInt(tsMs, 10) {
			t.Fatalf("view=%s X-Generated-At-Ms=%q, want %d", view, recGz.Header().Get("X-Generated-At-Ms"), tsMs)
		}
		etag := recGz.Header().Get("ETag")
		wantPrefix := fmt.Sprintf(`W/"snap-%s-`, view)
		if !strings.HasPrefix(etag, wantPrefix) || !strings.HasSuffix(etag, `"`) {
			t.Fatalf("view=%s ETag=%q, want prefix %q", view, etag, wantPrefix)
		}
		gzSizes[view] = recGz.Body.Len()
		unpacked := gunzipBytes(t, recGz.Body.Bytes())
		if !bytes.Equal(unpacked, recRaw.Body.Bytes()) {
			t.Fatalf("view=%s gunzipped bytes differ from raw", view)
		}
		var m map[string]any
		if err := json.Unmarshal(unpacked, &m); err != nil {
			t.Fatalf("view=%s unmarshal: %v", view, err)
		}
		decodedJSON[view] = m
	}

	// Default & unknown view must fall back to full
	for _, fallbackURL := range []string{"/api/snapshot", "/api/snapshot?view=unknown_xyz"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fallbackURL, nil))
		if rec.Code != http.StatusOK || rec.Body.Len() != rawSizes["full"] {
			t.Fatalf("%s fallback mismatch: status=%d len=%d want=%d", fallbackURL, rec.Code, rec.Body.Len(), rawSizes["full"])
		}
		if !strings.HasPrefix(rec.Header().Get("ETag"), `W/"snap-full-`) {
			t.Fatalf("%s fallback ETag=%q, want snap-full prefix", fallbackURL, rec.Header().Get("ETag"))
		}
	}

	// Verify strictly monotonic size reduction: tui < compact < summary < full
	if !(rawSizes["tui"] < rawSizes["compact"] && rawSizes["compact"] < rawSizes["summary"] && rawSizes["summary"] < rawSizes["full"]) {
		t.Fatalf("expected strict raw size hierarchy tui < compact < summary < full, got %+v", rawSizes)
	}
	if !(gzSizes["tui"] < gzSizes["compact"] && gzSizes["compact"] < gzSizes["summary"] && gzSizes["summary"] < gzSizes["full"]) {
		t.Fatalf("expected strict gzip size hierarchy tui < compact < summary < full, got %+v", gzSizes)
	}

	// Verify field contracts per view
	// 1. full: has aistudio.accounts & hourly_buckets; no raw_remaining_*; no recent_bars when has_recent_buckets=true
	fullRaw := string(state.GetJSON())
	if strings.Contains(fullRaw, "raw_remaining_requests") || strings.Contains(fullRaw, "raw_remaining_tokens") {
		t.Fatalf("full snapshot still contains deprecated raw_remaining_* fields")
	}
	if strings.Contains(fullRaw, `"recent_bars"`) {
		t.Fatalf("full snapshot should omit recent_bars when has_recent_buckets is true")
	}
	fullAIS := decodedJSON["full"]["aistudio"].(map[string]any)
	if _, ok := fullAIS["accounts"]; !ok {
		t.Fatalf("full view missing aistudio.accounts")
	}
	if _, ok := fullAIS["hourly_buckets"]; !ok {
		t.Fatalf("full view missing aistudio.hourly_buckets")
	}

	// 2. summary: omits aistudio.accounts, keeps hourly_buckets & models & antigravity
	sumAIS := decodedJSON["summary"]["aistudio"].(map[string]any)
	if _, exists := sumAIS["accounts"]; exists {
		t.Fatalf("summary view must omit aistudio.accounts")
	}
	if _, exists := sumAIS["hourly_buckets"]; !exists {
		t.Fatalf("summary view must retain aistudio.hourly_buckets")
	}
	if _, exists := sumAIS["models"]; !exists {
		t.Fatalf("summary view must retain aistudio.models")
	}

	// 3. compact: omits aistudio.accounts and aistudio.hourly_buckets, keeps antigravity.accounts[0].history.recent_buckets & cache metrics & aistudio.models
	cmpAIS := decodedJSON["compact"]["aistudio"].(map[string]any)
	if _, exists := cmpAIS["accounts"]; exists {
		t.Fatalf("compact view must omit aistudio.accounts")
	}
	if _, exists := cmpAIS["hourly_buckets"]; exists {
		t.Fatalf("compact view must omit aistudio.hourly_buckets")
	}
	for _, viewName := range []string{"full", "summary", "compact"} {
		agObj := decodedJSON[viewName]["antigravity"].(map[string]any)
		agSum := agObj["summary"].(map[string]any)
		for _, k := range []string{"weighted_success_rate", "weighted_cache_hit_rate", "weighted_cache_hit_rate_24h", "total_cache_saved_cost"} {
			if _, exists := agSum[k]; !exists {
				t.Fatalf("%s view antigravity.summary missing %q", viewName, k)
			}
		}
		agAcc := agObj["accounts"].([]any)[0].(map[string]any)
		agHist := agAcc["history"].(map[string]any)
		for _, k := range []string{
			"success_rate", "input_tokens", "cached_tokens", "cache_hit_rate", "cache_saved_cost",
			"input_tokens_24h", "cached_tokens_24h", "cache_hit_rate_24h", "cache_saved_cost_24h",
		} {
			if _, exists := agHist[k]; !exists {
				t.Fatalf("%s view antigravity.accounts[0].history missing %q", viewName, k)
			}
		}
	}
	cmpAG := decodedJSON["compact"]["antigravity"].(map[string]any)
	cmpAGAcc := cmpAG["accounts"].([]any)[0].(map[string]any)
	cmpHist := cmpAGAcc["history"].(map[string]any)
	if _, exists := cmpHist["recent_buckets"]; !exists {
		t.Fatalf("compact view must retain antigravity account recent_buckets")
	}

	// 4. tui: omits antigravity account history, aistudio accounts & hourly_buckets; retains lightweight cache metrics, windows, models & playground
	tuiAG := decodedJSON["tui"]["antigravity"].(map[string]any)
	tuiAGSum := tuiAG["summary"].(map[string]any)
	for _, k := range []string{"active_accounts", "weighted_cache_hit_rate", "weighted_cache_hit_rate_24h"} {
		if _, exists := tuiAGSum[k]; !exists {
			t.Fatalf("tui antigravity.summary missing %q", k)
		}
	}
	tuiAGAcc := tuiAG["accounts"].([]any)[0].(map[string]any)
	if _, exists := tuiAGAcc["history"]; exists {
		t.Fatalf("tui view must omit antigravity account history")
	}
	for _, k := range []string{"id", "code_name", "tier", "status", "cache_hit_rate", "cache_hit_rate_24h", "cache_saved_cost", "windows"} {
		if _, exists := tuiAGAcc[k]; !exists {
			t.Fatalf("tui antigravity account missing %q", k)
		}
	}
	tuiAIS := decodedJSON["tui"]["aistudio"].(map[string]any)
	for _, k := range []string{"online", "status", "next_reset_ms", "account_counts", "tier_counts", "playground", "models"} {
		if _, exists := tuiAIS[k]; !exists {
			t.Fatalf("tui aistudio missing %q", k)
		}
	}
	if _, exists := tuiAIS["accounts"]; exists {
		t.Fatalf("tui view must omit aistudio.accounts")
	}
	if _, exists := tuiAIS["hourly_buckets"]; exists {
		t.Fatalf("tui view must omit aistudio.hourly_buckets")
	}
}

func TestSnapshotSemanticETag304AndFreshFlow(t *testing.T) {
	subWeb, _ := fs.Sub(webFS, "web")
	state := &BoardState{}
	t1 := int64(1795680000000)
	t2 := t1 + 15_000
	t3 := t2 + 15_000

	state.Set(makeSyntheticSnapshot(t1, t1, 80.0))

	var refreshCount atomic.Int32
	var nextRem atomic.Int64
	nextRem.Store(800) // 80.0 * 10
	var nextTs atomic.Int64
	nextTs.Store(t2)

	coord := NewRefreshCoordinator(0, func(ctx context.Context) {
		refreshCount.Add(1)
		state.Set(makeSyntheticSnapshot(t1, nextTs.Load(), float64(nextRem.Load())/10.0))
	})

	cfg := AppConfig{RefreshIntervalSeconds: 15, ViewPasscode: "test-pass-only"}
	handler, err := newAppHandler(cfg, state, coord, nil, context.Background(), subWeb)
	if err != nil {
		t.Fatalf("newAppHandler: %v", err)
	}

	// 1. Unauthorized error must NEVER set Content-Encoding: gzip
	reqUnauth := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	reqUnauth.Header.Set("Accept-Encoding", "gzip")
	recUnauth := httptest.NewRecorder()
	handler.ServeHTTP(recUnauth, reqUnauth)
	if recUnauth.Code != http.StatusUnauthorized || recUnauth.Header().Get("Content-Encoding") != "" {
		t.Fatalf("401 status=%d ce=%q", recUnauth.Code, recUnauth.Header().Get("Content-Encoding"))
	}

	// 2. Capture ETags for all 4 views at T1
	etagsT1 := map[string]string{}
	for _, view := range []string{"full", "summary", "compact", "tui"} {
		req := httptest.NewRequest(http.MethodGet, "/api/snapshot?view="+view, nil)
		req.Header.Set("X-View-Passcode", cfg.ViewPasscode)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("T1 view=%s status=%d", view, rec.Code)
		}
		etagsT1[view] = rec.Header().Get("ETag")
	}

	// 3. Advance ONLY wall-clock timestamps (T1 -> T2) via ?fresh=1:
	//    RefreshCoordinator MUST execute first, and since quota data is identical,
	//    semantic ETag stays unchanged -> 304 Not Modified with X-Generated-At-Ms = T2!
	reqFreshSame := httptest.NewRequest(http.MethodGet, "/api/snapshot?view=compact&fresh=1", nil)
	reqFreshSame.Header.Set("X-View-Passcode", cfg.ViewPasscode)
	reqFreshSame.Header.Set("Accept-Encoding", "gzip")
	reqFreshSame.Header.Set("If-None-Match", etagsT1["compact"])
	recFreshSame := httptest.NewRecorder()
	handler.ServeHTTP(recFreshSame, reqFreshSame)

	if refreshCount.Load() != 1 {
		t.Fatalf("expected ?fresh=1 to trigger coordinator refresh before ETag check, count=%d", refreshCount.Load())
	}
	if recFreshSame.Code != http.StatusNotModified {
		t.Fatalf("fresh same-data status=%d, want 304", recFreshSame.Code)
	}
	if recFreshSame.Body.Len() != 0 || recFreshSame.Header().Get("Content-Encoding") != "" {
		t.Fatalf("304 must have 0B body and no Content-Encoding, got len=%d ce=%q", recFreshSame.Body.Len(), recFreshSame.Header().Get("Content-Encoding"))
	}
	if recFreshSame.Header().Get("ETag") != etagsT1["compact"] {
		t.Fatalf("ETag changed when only timestamps changed: got %q want %q", recFreshSame.Header().Get("ETag"), etagsT1["compact"])
	}
	if recFreshSame.Header().Get("X-Generated-At-Ms") != strconv.FormatInt(t2, 10) {
		t.Fatalf("304 X-Generated-At-Ms=%q, want %d", recFreshSame.Header().Get("X-Generated-At-Ms"), t2)
	}

	// All other views must also return 304 with T2 timestamp
	for _, view := range []string{"full", "summary", "tui"} {
		req := httptest.NewRequest(http.MethodGet, "/api/snapshot?view="+view, nil)
		req.Header.Set("X-View-Passcode", cfg.ViewPasscode)
		req.Header.Set("If-None-Match", etagsT1[view])
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified || rec.Header().Get("X-Generated-At-Ms") != strconv.FormatInt(t2, 10) {
			t.Fatalf("T2 view=%s status=%d gen=%q, want 304 and %d", view, rec.Code, rec.Header().Get("X-Generated-At-Ms"), t2)
		}
	}

	// 4. Quota data changes (80.0 -> 77.5) via ?fresh=1 -> MUST return 200 OK with new ETag and T3
	nextRem.Store(775)
	nextTs.Store(t3)
	reqFreshChanged := httptest.NewRequest(http.MethodGet, "/api/snapshot?view=compact&fresh=1", nil)
	reqFreshChanged.Header.Set("X-View-Passcode", cfg.ViewPasscode)
	reqFreshChanged.Header.Set("Accept-Encoding", "gzip")
	reqFreshChanged.Header.Set("If-None-Match", etagsT1["compact"])
	recFreshChanged := httptest.NewRecorder()
	handler.ServeHTTP(recFreshChanged, reqFreshChanged)

	if recFreshChanged.Code != http.StatusOK {
		t.Fatalf("changed quota status=%d, want 200", recFreshChanged.Code)
	}
	if recFreshChanged.Header().Get("ETag") == etagsT1["compact"] {
		t.Fatalf("ETag must change when quota data changes")
	}
	if recFreshChanged.Header().Get("X-Generated-At-Ms") != strconv.FormatInt(t3, 10) {
		t.Fatalf("changed X-Generated-At-Ms=%q, want %d", recFreshChanged.Header().Get("X-Generated-At-Ms"), t3)
	}
	if recFreshChanged.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("200 OK with Accept-Encoding: gzip must set Content-Encoding: gzip")
	}
}

func TestHistoryGzipSemanticETagAnd304(t *testing.T) {
	subWeb, _ := fs.Sub(webFS, "web")
	const dur5h int64 = 5 * 3600
	nowSec := time.Now().Unix()
	resetSec := nowSec + 3*3600

	pts := make([]lampHistoryPoint, 0, 48)
	for i := 47; i >= 0; i-- {
		pts = append(pts, lampHistoryPoint{
			T: nowSec - int64(i)*900 - 120,
			R: round3(100.0 - float64(47-i)*0.45),
			X: resetSec,
		})
	}
	lastR := pts[len(pts)-1].R
	est1 := round3(lastR - 0.8)

	cpa := NewCPACollector(CPAConfig{})
	cpa.histStore = &cpaHistoryStore{
		UpdatedAtSec: nowSec,
		Accounts: map[string]*cpaAccountHistoryStore{
			"ag-ultra-1": {
				PublicID: "ag-ultra-1",
				Tier:     "Ultra",
				Windows: map[string]*cpaWindowHistory{
					"gemini_5h": {
						Key:                       "gemini_5h",
						Group:                     "gemini",
						Label:                     "Gemini",
						WindowKind:                "5h",
						DurationSeconds:           dur5h,
						Remaining:                 lastR,
						ResetSec:                  resetSec,
						RawResetSec:               resetSec,
						ObservedSec:               pts[len(pts)-1].T,
						HasPostSampleCheck:        true,
						PostSampleRequests:        2,
						PostSampleCostUSD:         1.5,
						EstimatedRemainingPercent: &est1,
						Points:                    pts,
					},
				},
			},
		},
	}

	handler, err := newAppHandler(AppConfig{RefreshIntervalSeconds: 15}, &BoardState{}, nil, cpa, context.Background(), subWeb)
	if err != nil {
		t.Fatalf("newAppHandler: %v", err)
	}

	// 1. Error responses (400/404) must never be gzipped
	for _, bad := range []struct {
		url  string
		want int
	}{
		{"/api/antigravity/history", http.StatusBadRequest},
		{"/api/antigravity/history?account=missing", http.StatusNotFound},
	} {
		req := httptest.NewRequest(http.MethodGet, bad.url, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != bad.want || rec.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%s status=%d ce=%q, want %d and empty", bad.url, rec.Code, rec.Header().Get("Content-Encoding"), bad.want)
		}
	}

	// 2. First request -> 200 OK + gzip + semantic ETag + X-History-To-Sec
	url := "/api/antigravity/history?account=ag-ultra-1&window=5h&range=24h"
	req1 := httptest.NewRequest(http.MethodGet, url, nil)
	req1.Header.Set("Accept-Encoding", "gzip")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("history 1st status=%d, want 200", rec1.Code)
	}
	if rec1.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("history 1st Content-Encoding=%q, want gzip", rec1.Header().Get("Content-Encoding"))
	}
	if rec1.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("history 1st Vary=%q, want Accept-Encoding", rec1.Header().Get("Vary"))
	}
	etag1 := rec1.Header().Get("ETag")
	if !strings.HasPrefix(etag1, `W/"hist-`) {
		t.Fatalf("history ETag=%q, want W/\"hist-...\"", etag1)
	}
	if rec1.Header().Get("X-History-To-Sec") == "" {
		t.Fatalf("history missing X-History-To-Sec header")
	}
	unpacked1 := gunzipBytes(t, rec1.Body.Bytes())
	var hResp AntigravityHistoryResponse
	if err := json.Unmarshal(unpacked1, &hResp); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	if len(hResp.Series) != 1 || len(hResp.Series[0].Points) != 48 {
		t.Fatalf("unexpected history points: %d", len(hResp.Series))
	}

	// 3. Advance UpdatedAtSec by 15s with identical points & estimated value -> MUST hit 304!
	cpa.histStoreMu.Lock()
	cpa.histStore.UpdatedAtSec = nowSec + 15
	cpa.histStoreMu.Unlock()

	req2 := httptest.NewRequest(http.MethodGet, url, nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	req2.Header.Set("If-None-Match", etag1)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("history unchanged status=%d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 || rec2.Header().Get("Content-Encoding") != "" {
		t.Fatalf("history 304 must have 0B body and no Content-Encoding, got len=%d ce=%q", rec2.Body.Len(), rec2.Header().Get("Content-Encoding"))
	}
	if rec2.Header().Get("X-History-To-Sec") != strconv.FormatInt(nowSec+15, 10) {
		t.Fatalf("history 304 X-History-To-Sec=%q, want %d", rec2.Header().Get("X-History-To-Sec"), nowSec+15)
	}

	// 4. Change estimated_remaining_percent & post_sample_requests -> ETag changes -> 200 OK
	est2 := round3(lastR - 2.4)
	cpa.histStoreMu.Lock()
	wh := cpa.histStore.Accounts["ag-ultra-1"].Windows["gemini_5h"]
	wh.PostSampleRequests = 5
	wh.EstimatedRemainingPercent = &est2
	cpa.histStoreMu.Unlock()

	req3 := httptest.NewRequest(http.MethodGet, url, nil)
	req3.Header.Set("Accept-Encoding", "gzip")
	req3.Header.Set("If-None-Match", etag1)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusOK {
		t.Fatalf("history changed estimate status=%d, want 200", rec3.Code)
	}
	if rec3.Header().Get("ETag") == etag1 {
		t.Fatalf("history ETag must change when estimated_point value changes")
	}
}

func TestDTOCleanupsRecentBarsAndOmitEmpty(t *testing.T) {
	// Fallback when HasRecentBuckets == false must still serialize recent_bars
	hFallback := AntigravityAccountHistory{
		Available:        true,
		HasRecentBuckets: false,
		RecentBars:       []AntigravityRecentBar{{OK: true, TimestampMs: 1700000000000, StatusCode: 200}},
	}
	bFallback, err := json.Marshal(hFallback)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bFallback), `"recent_bars"`) {
		t.Fatalf("expected recent_bars when HasRecentBuckets=false, got %s", string(bFallback))
	}

	// Ineligible AIStudioAccountModel should omit zero numeric fields
	mIneligible := AIStudioAccountModel{
		ModelID:     "gemini-test",
		Status:      "ineligible",
		StatusLabel: "档位不支持",
	}
	bModel, err := json.Marshal(mIneligible)
	if err != nil {
		t.Fatal(err)
	}
	sModel := string(bModel)
	for _, omitted := range []string{"limit_requests", "used_requests", "effective_remaining_requests", "limit_tokens", "used_tokens", "effective_remaining_tokens", "effective_percent", "cooldown_until_ms"} {
		if strings.Contains(sModel, omitted) {
			t.Fatalf("expected zero field %q to be omitted, got %s", omitted, sModel)
		}
	}
}
