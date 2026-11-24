package main

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"strings"
)

func stableHashHex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

func tierSortRank(tier string) int {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "ultra":
		return 0
	case "pro":
		return 1
	case "free":
		return 2
	default:
		return 3
	}
}

func normalizeAntigravityTier(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case strings.Contains(s, "ultra"):
		return "Ultra"
	case strings.Contains(s, "pro"):
		return "Pro"
	case strings.Contains(s, "free"):
		return "Free"
	default:
		return "Unknown"
	}
}

func normalizeAISTier(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "2", "ultra", "tier2", "g1-ultra-tier":
		return "Ultra"
	case "1", "pro", "tier1", "g1-pro-tier":
		return "Pro"
	case "0", "3", "free", "plus", "tier0", "":
		return "Free"
	default:
		if strings.Contains(s, "ultra") {
			return "Ultra"
		}
		if strings.Contains(s, "pro") {
			return "Pro"
		}
		return "Free"
	}
}

func clampPercent(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func round3(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func floatPtr(v float64) *float64 {
	x := v
	return &x
}

func int64Ptr(v int64) *int64 {
	x := v
	return &x
}
