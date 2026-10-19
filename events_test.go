package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func eventsAPI(t *testing.T, handler func(map[string]any) any) *API {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/management/monitoring/analytics" || r.Method != "POST" {
			t.Errorf("unexpected event endpoint %s %s", r.Method, r.URL.Path)
		}
		var q map[string]any
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		json.NewEncoder(w).Encode(handler(q))
	}))
	t.Cleanup(s.Close)
	return newAPI(Bootstrap{CPAMPURL: s.URL}, Secrets{ManagementKey: "management-test", PrewarmKey: "prewarm-test"})
}
func eventPage(items []Event, total int, more bool, ms, id int64) map[string]any {
	return map[string]any{"events": map[string]any{"items": items, "total_count": total, "has_more": more, "next_before_ms": ms, "next_before_id": id}}
}
func TestEventsFollowBothCursorFieldsWithFixedBounds(t *testing.T) {
	calls := 0
	a := eventsAPI(t, func(q map[string]any) any {
		calls++
		if q["from_ms"] != float64(100) || q["to_ms"] != float64(1000) {
			t.Error("bounds changed")
		}
		p := obj(obj(q["include"])["events_page"])
		if calls == 1 {
			return eventPage([]Event{{Hash: "new", Timestamp: 800}}, 2, true, 800, 44)
		}
		if p["before_ms"] != float64(800) || p["before_id"] != float64(44) {
			t.Errorf("dual cursor missing: %#v", p)
		}
		return eventPage([]Event{{Hash: "old", Timestamp: 200}}, 2, false, 0, 0)
	})
	rows, err := a.events(context.Background(), 100, 1000, map[string]any{"include_failed": true}, "")
	if err != nil || len(rows) != 2 || calls != 2 {
		t.Fatalf("rows=%#v calls=%d err=%v", rows, calls, err)
	}
}
func TestEventsRejectIncompleteChangingAndNonprogressingHistory(t *testing.T) {
	cases := []struct {
		name string
		page func(int) map[string]any
	}{
		{"total-changed", func(n int) map[string]any {
			if n == 1 {
				return eventPage([]Event{{Hash: "a", Timestamp: 800}}, 2, true, 800, 4)
			}
			return eventPage([]Event{{Hash: "b", Timestamp: 200}}, 3, false, 0, 0)
		}},
		{"cursor-stuck", func(n int) map[string]any { return eventPage([]Event{{Hash: "a", Timestamp: 800}}, 2, true, 800, 4) }},
		{"missing-id", func(n int) map[string]any { return eventPage([]Event{{Hash: "a", Timestamp: 800}}, 2, true, 800, 0) }},
		{"truncated", func(n int) map[string]any { return eventPage([]Event{{Hash: "a", Timestamp: 800}}, 2, false, 0, 0) }},
		{"outside-bounds", func(n int) map[string]any { return eventPage([]Event{{Hash: "a", Timestamp: 1000}}, 1, false, 0, 0) }},
		{"no-hash", func(n int) map[string]any { return eventPage([]Event{{Timestamp: 800}}, 1, false, 0, 0) }},
		{"raw-deleted", func(n int) map[string]any {
			p := eventPage(nil, 0, false, 0, 0)
			p["coverage"] = map[string]any{"raw_complete": true, "raw_deleted_event_count": 1}
			return p
		}},
		{"fidelity", func(n int) map[string]any {
			p := eventPage(nil, 0, false, 0, 0)
			p["coverage"] = map[string]any{"raw_complete": true, "fidelity_limitations": []string{"unknown"}}
			return p
		}},
		{"not-complete", func(n int) map[string]any {
			p := eventPage(nil, 0, false, 0, 0)
			p["coverage"] = map[string]any{"raw_complete": false}
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			a := eventsAPI(t, func(q map[string]any) any { n++; return tc.page(n) })
			if rows, err := a.events(context.Background(), 100, 1000, nil, ""); err == nil {
				t.Fatalf("unsafe history accepted %#v", rows)
			}
		})
	}
}
func TestAttemptUsesRawWildcardAndRejectsMissingUA(t *testing.T) {
	a := eventsAPI(t, func(q map[string]any) any {
		if q["search_query"] != "attempt-uuid%" {
			t.Errorf("raw path escape missing %#v", q)
		}
		if _, ok := q["search_api_key_hash"]; ok {
			t.Error("unsafe OR key filter")
		}
		f := obj(q["filters"])
		if f["api_key_hashes"] == nil || f["auth_files"] == nil {
			t.Errorf("missing AND narrowing %#v", f)
		}
		return eventPage([]Event{{Hash: "event", Timestamp: 200}}, 1, false, 0, 0)
	})
	if _, err := a.attempt(context.Background(), Account{Name: "a.json"}, Record{"sent_ms": int64(150), "attempt_token": "attempt-uuid"}, 1000); err == nil {
		t.Fatal("projection row with missing UA accepted")
	}
}
func TestExactAttemptEvidence(t *testing.T) {
	a := Account{Name: "a.json", Provider: "codex", Index: "index"}
	r := Record{"sent_ms": int64(10000), "attempt_token": "uuid", "requested_model": "prefix/gpt-model", "request_id": "request", "trace": "codex-index-request"}
	base := Event{Hash: "hash", RequestID: "request", Timestamp: 10001, Model: "gpt-model", RequestedModel: "prefix/gpt-model", UA: "cpa-quota-guard/2 uuid", Index: "index", KeyHash: "key", File: "a.json", Provider: "codex", Reasoning: "low"}
	if !eventMatches(a, r, base, "key", 20000) {
		t.Fatal("exact evidence rejected")
	}
	cases := []struct {
		name   string
		mutate func(*Event)
	}{{"wrong-key", func(e *Event) { e.KeyHash = "other" }}, {"wrong-model", func(e *Event) { e.RequestedModel = "other" }}, {"wrong-index", func(e *Event) { e.Index = "other" }}, {"wrong-file", func(e *Event) { e.File = "other" }}, {"wrong-provider", func(e *Event) { e.Provider = "other" }}, {"wrong-low", func(e *Event) { e.Reasoning = "high" }}, {"wrong-UA", func(e *Event) { e.UA += " extra" }}, {"missing-UA", func(e *Event) { e.UA = "" }}, {"wrong-trace-request", func(e *Event) { e.RequestID = "other" }}, {"missing-request", func(e *Event) { e.RequestID = "" }}, {"too-early", func(e *Event) { e.Timestamp = 8999 }}, {"future", func(e *Event) { e.Timestamp = 21001 }}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			tc.mutate(&e)
			if eventMatches(a, r, e, "key", 20000) {
				t.Fatal("mismatched evidence accepted")
			}
		})
	}
	delete(r, "request_id")
	r["trace"] = "malformed"
	if eventMatches(a, r, base, "key", 20000) {
		t.Fatal("malformed trace accepted")
	}
}
func TestCompletionUsesNonnegativeLatency(t *testing.T) {
	for _, n := range []int64{-100, 0, 5000} {
		v := Event{Timestamp: 1000, Latency: &n}
		want := int64(1000)
		if n > 0 {
			want += n
		}
		if v.completion() != want {
			t.Fatalf("latency=%d completion=%d", n, v.completion())
		}
	}
}
func TestLatestQueriesLongRunningHistoryAndUsesCompletion(t *testing.T) {
	now := int64(1800000000000)
	split := now - fiveHour - 45000 - 3600000
	a := Account{Name: "a", Index: "i", Provider: "codex"}
	longLatency := int64(8 * 3600000)
	n := 0
	api := eventsAPI(t, func(q map[string]any) any {
		n++
		f := obj(q["filters"])
		if q["search_query"] != nil {
			t.Error("activity query searched UA")
		}
		if n == 1 {
			return eventPage(nil, 0, false, 0, 0)
		}
		if q["from_ms"] != float64(1) || q["to_ms"] != float64(split) || f["min_latency_ms"] != float64(3600000) {
			t.Errorf("missing long-running historical query %#v", q)
		}
		return eventPage([]Event{{Hash: "long", Timestamp: now - longLatency - 500, Latency: &longLatency, Model: "gpt-6-luna", RequestID: "r", File: a.Name, Index: a.Index, Provider: a.Provider}}, 1, false, 0, 0)
	})
	latest, err := api.latest(context.Background(), a, "all", now)
	if err != nil || latest != now-500 || n != 2 {
		t.Fatalf("latest=%d calls=%d err=%v", latest, n, err)
	}
}
func TestSendUsesPublicStreamingEndpointAndDedicatedIdentity(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer prewarm" || r.Header.Get("User-Agent") != "cpa-quota-guard/2 uuid" || r.Header.Get("Idempotency-Key") != "uuid" {
			t.Error("incorrect public generation identity")
		}
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if p["stream"] != true || p["reasoning_effort"] != "low" || p["max_tokens"] != float64(1) || p["model"] != "prefix/model" {
			t.Errorf("unsafe generation payload %#v", p)
		}
		w.Header().Set("X-CPA-Trace-ID", "codex-i-r")
		w.Write([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	api := newAPI(Bootstrap{CPAURL: server.URL}, Secrets{PrewarmKey: "prewarm"})
	trace, err := api.send(context.Background(), "prefix/model", "uuid")
	if !called || err != nil || !strings.HasSuffix(trace, "-r") {
		t.Fatalf("called=%v trace=%s err=%v", called, trace, err)
	}
}
