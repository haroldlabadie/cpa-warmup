package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func snapshotEngine(t *testing.T, handler http.HandlerFunc) *Engine {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	e := scheduleEngine(t)
	e.cfg.Observe = false
	e.api = newAPI(Bootstrap{CPAMPURL: server.URL}, Secrets{ManagementKey: "test"})
	return e
}
func snapshotSample(now int64, remaining float64) *Sample {
	return &Sample{Observed: now, Windows: []Window{{ID: "five", Kind: "five_hour", Scope: "all", Remaining: remaining, EndMS: now + fiveHour, Duration: fiveHour}, {ID: "week", Kind: "weekly", Scope: "all", Remaining: 80, EndMS: now + week, Duration: week}}}
}
func TestSnapshotObserveAndStaleSamplesNeverWrite(t *testing.T) {
	calls := 0
	e := snapshotEngine(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{}`)) })
	now := int64(1800000000000)
	a := Account{Name: "a", Provider: "codex", Index: "i"}
	e.cfg.Observe = true
	if err := e.publishSample(context.Background(), a, snapshotSample(now, 90), now); err != nil {
		t.Fatal(err)
	}
	e.cfg.Observe = false
	for _, s := range []*Sample{nil, snapshotSample(now-90001, 90), snapshotSample(now+1, 90)} {
		if err := e.publishSample(context.Background(), a, s, now); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatalf("observe/stale sample published %d times", calls)
	}
}
func TestSnapshotZeroQuotaOmitsUnprovenCycleStart(t *testing.T) {
	for _, remaining := range []float64{100, 92} {
		t.Run(fmtRemaining(remaining), func(t *testing.T) {
			now := int64(1800000000000)
			e := snapshotEngine(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/management/quota-snapshots" {
					t.Errorf("wrong endpoint %s", r.URL.Path)
				}
				var p map[string]any
				json.NewDecoder(r.Body).Decode(&p)
				entries := p["entries"].([]any)
				entry := obj(entries[0])
				rows := entry["windows"].([]any)
				five := obj(rows[0])
				_, hasStart := five["cycle_start_ms"]
				if hasStart != (remaining < 100) {
					t.Errorf("remaining=%v fabricated or missing start: %#v", remaining, five)
				}
				if five["model_scope_key"] != "codex_main" {
					t.Errorf("bad Codex scope %#v", five)
				}
				if obj(entry["observation"])["inventory_mode"] != "partial" {
					t.Error("snapshot must not replace other provider observations")
				}
				if _, ok := obj(rows[1])["cycle_start_ms"]; !ok {
					t.Error("weekly boundary omitted")
				}
				w.Write([]byte(`{}`))
			})
			if err := e.publishSample(context.Background(), Account{Name: "a", Provider: "codex", Index: "i"}, snapshotSample(now, remaining), now); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func fmtRemaining(n float64) string {
	if n == 100 {
		return "zero-used"
	}
	return "nonzero-used"
}
func TestSnapshotFailurePreservesHealthySampleAndRetries(t *testing.T) {
	now := int64(1800000000000)
	calls := 0
	e := snapshotEngine(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{}`))
	})
	a := Account{Name: "a", Provider: "codex", Index: "i"}
	sample := snapshotSample(now, 92)
	original, _ := json.Marshal(sample)
	e.observations[a.Name] = sample
	if err := e.publishSample(context.Background(), a, sample, now); err == nil {
		t.Fatal("publication error swallowed")
	}
	if e.snapshotTimes[a.Name] != 0 || e.snapshotDigests[a.Name] != "" {
		t.Fatal("failed publication registered success")
	}
	after, _ := json.Marshal(sample)
	if !reflect.DeepEqual(original, after) || e.observations[a.Name] != sample {
		t.Fatal("display failure replaced healthy probe")
	}
	if err := e.publishSample(context.Background(), a, sample, now); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || e.snapshotTimes[a.Name] != now {
		t.Fatalf("failed publication was not retried calls=%d", calls)
	}
}
