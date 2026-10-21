package main

import (
	"testing"
)

// hintSample models Antigravity's answer when no five-hour cycle is active:
// the reset is measured from the observation time and usage rounds to zero.
func hintSample(scope string, observed int64) *Sample {
	return &Sample{Observed: observed, Windows: []Window{
		{Kind: "five_hour", Scope: scope, Remaining: 100, EndMS: observed + fiveHour, Duration: fiveHour},
		{Kind: "weekly", Scope: scope, Remaining: 80, EndMS: observed + week, Duration: week},
	}}
}

func TestRollingZeroUsageHintRetiresOnlyAfterTwoMatchingSamples(t *testing.T) {
	for _, tc := range []struct{ provider, scope string }{{"codex", "all"}, {"antigravity", "gemini"}, {"antigravity", "third_party"}} {
		t.Run(tc.provider+"/"+tc.scope, func(t *testing.T) {
			e := scheduleEngine(t)
			a := Account{ID: "id", Name: "a.json", Provider: tc.provider, Index: "index", Prefix: "prefix"}
			if tc.provider == "antigravity" {
				a.ProjectID = "project"
			}
			e.store.state.Identities[a.Name] = a.identity()
			now := int64(1800000000000)
			r := Record{"status": "unconfirmed", "sent_ms": now - fiveHour - 200000, "attempt_token": "uuid", "requested_model": "prefix/model", "identity": a.identity(), "scope": tc.scope, "account": a.Name, "slot": "2026-10-19T05:00+08:00", "last_reason": "missing-event-or-new-cycle"}
			e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
			// One sample can never prove the absence of a cycle.
			if err := e.confirm(a, r, hintSample(tc.scope, now-20000), now-20000); err != nil {
				t.Fatal(err)
			}
			if str(r, "status") == "uncertainty_expired" {
				t.Fatalf("one sample retired: %#v", r)
			}
			if err := e.confirm(a, r, hintSample(tc.scope, now), now); err != nil {
				t.Fatal(err)
			}
			if str(r, "status") != "uncertainty_expired" || str(r, "retirement") != "expired-without-active-cycle" {
				t.Fatalf("rolling zero-usage hint did not retire: %#v", r)
			}
			if str(r, "activation") != "" || r["confirmed_ms"] != nil || r["cycle_end_ms"] != nil {
				t.Fatalf("retirement invented success: %#v", r)
			}
		})
	}
}

func TestFixedZeroUsageCycleStaysUnresolvedUntilStable(t *testing.T) {
	e := scheduleEngine(t)
	a := Account{ID: "id", Name: "a.json", Provider: "antigravity", ProjectID: "project", Index: "index", Prefix: "prefix"}
	e.store.state.Identities[a.Name] = a.identity()
	now := int64(1800000000000)
	r := Record{"status": "unconfirmed", "sent_ms": now - fiveHour - 200000, "attempt_token": "uuid", "requested_model": "prefix/model", "identity": a.identity(), "scope": "gemini", "account": a.Name}
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	// A fixed boundary is a real cycle whose usage rounds to zero. Two stable
	// samples are the documented retirement evidence, not the rolling-hint path.
	fixed := func(observed int64) *Sample {
		return &Sample{Observed: observed, Windows: []Window{
			{Kind: "five_hour", Scope: "gemini", Remaining: 100, EndMS: now + 3*3600000, Duration: fiveHour},
			{Kind: "weekly", Scope: "gemini", Remaining: 80, EndMS: now + week, Duration: week},
		}}
	}
	if err := e.confirm(a, r, fixed(now-20000), now-20000); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") == "uncertainty_expired" {
		t.Fatalf("single fixed sample retired: %#v", r)
	}
	if err := e.confirm(a, r, fixed(now), now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "uncertainty_expired" || str(r, "retirement") != "later-cycle-without-success-event" {
		t.Fatalf("stable fixed boundary did not retire: %#v", r)
	}
}

func TestYoungRecordIsNeverAbandoned(t *testing.T) {
	e := scheduleEngine(t)
	a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index", Prefix: "prefix"}
	e.store.state.Identities[a.Name] = a.identity()
	now := int64(1800000000000)
	r := Record{"status": "unconfirmed", "sent_ms": now - 60000, "attempt_token": "uuid", "requested_model": "prefix/model", "identity": a.identity(), "scope": "all", "account": a.Name}
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	for _, at := range []int64{now - 20000, now} {
		if err := e.confirm(a, r, hintSample("all", at), at); err != nil {
			t.Fatal(err)
		}
	}
	if str(r, "status") == "uncertainty_expired" {
		t.Fatalf("record inside own cycle abandoned: %#v", r)
	}
}

func TestDisabledFamilyStillReconcilesUnresolvedHistory(t *testing.T) {
	e := scheduleEngine(t)
	now := int64(1800000000000)
	a := Account{ID: "id", Name: "a.json", Provider: "antigravity", ProjectID: "project", Index: "index", Prefix: "prefix"}
	e.store.state.Identities[a.Name] = a.identity()
	key := recordKey(a.Name, "third_party")
	e.store.state.Slots["2026-10-19T05:00+08:00"] = map[string]Record{key: {"status": "unconfirmed", "sent_ms": now - fiveHour - 200000, "attempt_token": "uuid", "requested_model": "prefix/model", "identity": a.identity(), "scope": "third_party", "account": a.Name}}
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	if err := e.reconcileScope(a, "third_party", hintSample("third_party", now-20000), now-20000); err != nil {
		t.Fatal(err)
	}
	if err := e.reconcileScope(a, "third_party", hintSample("third_party", now), now); err != nil {
		t.Fatal(err)
	}
	r := e.store.state.Slots["2026-10-19T05:00+08:00"][key]
	if str(r, "status") != "uncertainty_expired" || str(r, "retirement") != "expired-without-active-cycle" {
		t.Fatalf("disabled family kept unresolved history: %#v", r)
	}
}
