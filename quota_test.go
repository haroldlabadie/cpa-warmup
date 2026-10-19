package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func quotaObject(t *testing.T, s string) map[string]any {
	t.Helper()
	var p map[string]any
	if err := decodePreserving([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func codexWindow(seconds int64, used float64) map[string]any {
	return map[string]any{"limit_window_seconds": float64(seconds), "used_percent": used, "reset_at": float64(1800000000)}
}
func TestCodexMainPoolDoesNotBorrowAdditionalPool(t *testing.T) {
	p := map[string]any{"rate_limit": map[string]any{"primary_window": codexWindow(18000, 0), "secondary_window": codexWindow(604800, 7)}, "additional_rate_limits": []any{map[string]any{"limit_name": "spark", "rate_limit": map[string]any{"primary_window": codexWindow(18000, 100), "secondary_window": codexWindow(604800, 100)}}}}
	ws, err := parseCodex(p)
	if err != nil {
		t.Fatal(err)
	}
	s := &Sample{Observed: 1700000000000, Windows: ws}
	if w := warmWindow(s, "all"); w == nil || w.Remaining != 100 || !complete(s, "all", s.Observed) {
		t.Fatalf("additional pool contaminated main pool: %#v", ws)
	}
	if w := warmWindow(s, "spark"); w == nil || w.Remaining != 0 {
		t.Fatal("additional evidence lost")
	}
}
func TestCodexFreeWeeklyOnlyHasNoFiveHour(t *testing.T) {
	ws, err := parseCodex(map[string]any{"rate_limit": map[string]any{"primary_window": codexWindow(604800, 4)}})
	if err != nil {
		t.Fatal(err)
	}
	s := &Sample{Observed: 1700000000000, Windows: ws}
	if warmWindow(s, "all") != nil || complete(s, "all", s.Observed) {
		t.Fatal("weekly-only account invented 5h window")
	}
}
func TestCodexRejectsMalformedQuota(t *testing.T) {
	for _, raw := range []string{`{}`, `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":101,"reset_at":1}}}`, `{"rate_limit":{"primary_window":{"limit_window_seconds":18000,"used_percent":"invalid","reset_at":1}}}`, `{"rate_limit":{},"additional_rate_limits":{}}`} {
		if _, err := parseCodex(quotaObject(t, raw)); err == nil {
			t.Fatalf("accepted invalid quota %s", raw)
		}
	}
}

func TestSupplierNumericStringsAndOverflowBounds(t *testing.T) {
	p := quotaObject(t, `{"rate_limit":{"primary_window":{"limit_window_seconds":"18000","used_percent":" 8.5 ","reset_at":"1800000000"},"secondary_window":{"limit_window_seconds":"604800","used_percent":"3","reset_at":"1800000001"}}}`)
	windows, err := parseCodex(p)
	if err != nil || len(windows) != 2 || windows[0].Remaining != 91.5 || windows[0].Duration != fiveHour {
		t.Fatalf("Codex numeric strings failed: %#v %v", windows, err)
	}
	ag, err := parseAG(map[string]any{"groups": []any{agGroup("Gemini", "0.92", true), agGroup("Claude", " 0.97 ", true)}})
	if err != nil || len(ag) != 4 {
		t.Fatalf("Antigravity numeric strings failed: %#v %v", ag, err)
	}
	for _, w := range ag {
		if w.Scope == "gemini" && w.Remaining != 92 || w.Scope == "third_party" && w.Remaining != 97 {
			t.Fatalf("incorrect string fraction %#v", w)
		}
	}
	for _, bad := range []any{"NaN", "Inf", "-Inf", "1e1000", "", nil} {
		if _, err := number(bad); err == nil {
			t.Fatalf("invalid numeric string accepted %#v", bad)
		}
	}
	for _, mutate := range []func(map[string]any){func(w map[string]any) { w["limit_window_seconds"] = "315576001" }, func(w map[string]any) { w["reset_at"] = "253402300800" }} {
		w := codexWindow(18000, 0)
		mutate(w)
		if _, err := parseCodex(map[string]any{"rate_limit": map[string]any{"primary_window": w}}); err == nil {
			t.Fatalf("overflow bound accepted %#v", w)
		}
	}
}
func agGroup(scope string, remaining any, includeWeekly bool) map[string]any {
	b := []any{map[string]any{"bucketId": scope + "-five", "window": "5h", "remainingFraction": remaining, "resetTime": "2030-01-01T00:00:00Z"}}
	if includeWeekly {
		b = append(b, map[string]any{"bucketId": scope + "-week", "window": "weekly", "remainingFraction": remaining, "resetTime": "2030-01-02T00:00:00Z"})
	}
	return map[string]any{"displayName": scope, "buckets": b}
}
func TestAntigravityMissingFamilyDoesNotInventEvidence(t *testing.T) {
	ws, err := parseAG(map[string]any{"groups": []any{agGroup("Gemini", float64(.8), true), agGroup("Claude", float64(.5), false)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("partial family retained: %#v", ws)
	}
	for _, w := range ws {
		if w.Scope != "gemini" {
			t.Fatalf("incomplete family retained: %#v", ws)
		}
	}
	if _, err := parseAG(map[string]any{"groups": []any{agGroup("Gemini", float64(2), true)}}); err == nil {
		t.Fatal("invalid remaining accepted")
	}
}
func TestAntigravityFallbackFillsOnlyMissingFamily(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var q map[string]any
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		groups := []any{agGroup("Gemini", float64(.8), true)}
		if strings.Contains(text(q["url"]), "daily-cloudcode") {
			groups = []any{agGroup("Gemini", float64(.1), true), agGroup("Claude", float64(.4), true)}
		}
		b, _ := json.Marshal(map[string]any{"groups": groups})
		json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": string(b)})
	}))
	defer server.Close()
	api := newAPI(Bootstrap{CPAMPURL: server.URL}, Secrets{ManagementKey: "test"})
	s, err := api.probe(context.Background(), Account{Provider: "antigravity", Index: "i", ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(s.Windows) != 4 {
		t.Fatalf("fallback calls=%d windows=%#v", calls, s.Windows)
	}
	if warmWindow(s, "gemini").Remaining != 80 || warmWindow(s, "third_party").Remaining != 40 {
		t.Fatalf("fallback overwrote first complete family: %#v", s.Windows)
	}
}
func TestQuotaFreshnessAndMissingWeekly(t *testing.T) {
	now := time.Now().UnixMilli()
	s := &Sample{Observed: now, Windows: []Window{{Kind: "five_hour", Scope: "all", EndMS: now + 1}}}
	if complete(s, "all", now) {
		t.Fatal("accepted missing weekly")
	}
	s.Windows = append(s.Windows, Window{Kind: "weekly", Scope: "all", EndMS: now + 100000})
	if !complete(s, "all", now) {
		t.Fatal("fresh complete sample rejected")
	}
	s.Observed = now - 90001
	if complete(s, "all", now) {
		t.Fatal("stale sample accepted")
	}
	s.Observed = now + 1
	if complete(s, "all", now) {
		t.Fatal("future sample accepted")
	}
}
