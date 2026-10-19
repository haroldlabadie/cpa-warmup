package main

import (
	"context"
	"testing"
	"time"
)

func clockMS(t *testing.T, s string) int64 {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v.UnixMilli()
}
func TestFourSlotsHaveNinetyMinuteExclusiveGrace(t *testing.T) {
	s := defaults()
	for _, hour := range []string{"05", "10", "15", "20"} {
		start := clockMS(t, "2026-10-19T"+hour+":00:00+08:00")
		for _, offset := range []int64{0, 1, 5399999} {
			id, got := slotAt(start+offset, s)
			if id == "" || got != start {
				t.Fatalf("hour=%s offset=%d id=%s start=%d", hour, offset, id, got)
			}
		}
		if id, _ := slotAt(start+5400000, s); id != "" {
			t.Fatalf("expired grace admitted %s", id)
		}
		if id, _ := slotAt(start-1, s); id != "" {
			t.Fatalf("before slot admitted %s", id)
		}
	}
	if next := nextSlot(clockMS(t, "2026-10-19T20:00:00+08:00"), s); next != "2026-10-20T05:00:00+08:00" {
		t.Fatalf("next day rollover: %s", next)
	}
}
func TestActiveUntilUsesRequestCompletionAndOneSecondTolerance(t *testing.T) {
	now := int64(1800000000000)
	end := now + 10000
	w := Window{Kind: "five_hour", Scope: "all", Remaining: 100, EndMS: end, Duration: fiveHour}
	start := end - fiveHour
	for _, tc := range []struct {
		name         string
		latest, want int64
	}{{"no-proof", 0, 0}, {"old-completion", start - 1001, 0}, {"rounding-tolerance", start - 1000, end}, {"inside", start + 5000, end}, {"future", now + 1, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := activeUntil(w, now, tc.latest)
			if err != nil || got != tc.want {
				t.Fatalf("got=%d want=%d err=%v", got, tc.want, err)
			}
		})
	}
	w.Remaining = 99
	if got, err := activeUntil(w, now, 0); got != end || err != nil {
		t.Fatal("positive usage ignored")
	}
	w.EndMS = now
	if _, err := activeUntil(w, now, 0); err == nil {
		t.Fatal("expired nonzero evidence accepted")
	}
	w.Remaining = 100
	if got, err := activeUntil(w, now, 0); got != 0 || err != nil {
		t.Fatal("expired zero rejected")
	}
}
func scheduleEngine(t *testing.T) *Engine {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Engine{cfg: Bootstrap{Enabled: true, Observe: true}, settings: defaults(), store: testStore(t), ctx: ctx, cancel: cancel, observations: map[string]any{}}
}
func TestSchedulePreservesUnknownAttemptAndDoesNotResend(t *testing.T) {
	e := scheduleEngine(t)
	now := clockMS(t, "2026-10-19T10:10:00+08:00")
	a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index", Prefix: "prefix"}
	e.store.state.Identities[a.Name] = a.identity()
	e.store.state.Slots["2026-10-19T05:00+08:00"] = map[string]Record{a.Name: {"status": "unconfirmed", "attempt_token": "unknown", "sent_ms": now - 100000, "account": a.Name, "identity": a.identity()}}
	e.api = eventsAPI(t, func(q map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	sample := &Sample{Observed: now, Windows: []Window{{Kind: "five_hour", Scope: "all", Remaining: 100, EndMS: now - 20000, Duration: fiveHour}, {Kind: "weekly", Scope: "all", Remaining: 100, EndMS: now + week, Duration: week}}}
	if err := e.schedule(a, "all", sample, []Account{a}, defaults(), now); err != nil {
		t.Fatal(err)
	}
	r := e.store.state.Slots["2026-10-19T10:00+08:00"][a.Name]
	if str(r, "last_reason") != "unresolved-prior-request" || num(r, "attempts") != 0 {
		t.Fatalf("uncertain prior attempt bypassed %#v", r)
	}
	old := e.store.state.Slots["2026-10-19T05:00+08:00"][a.Name]
	if !unresolved(old) || str(old, "attempt_token") != "unknown" {
		t.Fatalf("prior uncertainty cleared %#v", old)
	}
}
func TestScheduleWaitsFifteenSecondsAfterExpiredZeroBucket(t *testing.T) {
	for _, offset := range []int64{0, 14999, 15000} {
		t.Run(time.Duration(offset).String(), func(t *testing.T) {
			e := scheduleEngine(t)
			start := clockMS(t, "2026-10-19T10:00:00+08:00")
			now := start + 60000
			a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index", Prefix: "prefix"}
			e.api = eventsAPI(t, func(q map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
			sample := &Sample{Observed: now, Windows: []Window{{Kind: "five_hour", Scope: "all", Remaining: 100, EndMS: now - offset, Duration: fiveHour}, {Kind: "weekly", Scope: "all", Remaining: 100, EndMS: now + week, Duration: week}}}
			if err := e.schedule(a, "all", sample, []Account{a}, defaults(), now); err != nil {
				t.Fatal(err)
			}
			r := e.store.state.Slots["2026-10-19T10:00+08:00"][a.Name]
			want := "reset-buffer"
			if offset == 15000 {
				want = "observe-or-paused"
			}
			if str(r, "last_reason") != want || num(r, "attempts") != 0 {
				t.Fatalf("offset=%d record=%#v want=%s", offset, r, want)
			}
		})
	}
}
func TestExpireRetainsHistoricalUnknownButSkipsPending(t *testing.T) {
	e := scheduleEngine(t)
	now := clockMS(t, "2026-10-19T12:00:00+08:00")
	e.store.state.Slots["2026-10-01T05:00+08:00"] = map[string]Record{"a": {"status": "unconfirmed", "attempt_token": "u"}}
	e.store.state.Slots["2026-10-19T10:00+08:00"] = map[string]Record{"a": {"status": "pending", "last_reason": "disabled"}}
	e.expire(defaults(), now)
	if !unresolved(e.store.state.Slots["2026-10-01T05:00+08:00"]["a"]) {
		t.Fatal("historical uncertainty pruned")
	}
	if r := e.store.state.Slots["2026-10-19T10:00+08:00"]["a"]; str(r, "status") != "skipped" {
		t.Fatalf("expired pending not skipped %#v", r)
	}
}
