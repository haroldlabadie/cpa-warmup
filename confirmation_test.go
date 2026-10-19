package main

import (
	"strconv"
	"testing"
)

func confirmationFixture(t *testing.T, failed bool, status *int) (*Engine, Account, Record, *Sample, int64) {
	t.Helper()
	now := int64(1800000000000)
	a := Account{ID: "id", Name: "a.json", Index: "index", Provider: "codex"}
	r := Record{"status": "sent", "sent_ms": now - 200000, "attempt_token": "uuid", "requested_model": "prefix/model", "identity": a.identity(), "scope": "all", "trace": "codex-index-request"}
	v := Event{Hash: "hash", RequestID: "request", Timestamp: now - 199999, RequestedModel: "prefix/model", Model: "model", UA: "cpa-quota-guard/2 uuid", Index: a.Index, File: a.Name, Provider: a.Provider, KeyHash: hash("prewarm-test"), Reasoning: "low", Failed: failed, Status: status}
	e := scheduleEngine(t)
	e.api = eventsAPI(t, func(q map[string]any) any { return eventPage([]Event{v}, 1, false, 0, 0) })
	s := &Sample{Observed: now, Windows: []Window{{Kind: "five_hour", Scope: "all", Remaining: 99, EndMS: v.Timestamp + fiveHour, Duration: fiveHour}, {Kind: "weekly", Scope: "all", Remaining: 80, EndMS: now + week, Duration: week}}}
	return e, a, r, s, now
}
func TestConfirmationSeparatesVerifiedNewCycleFromRequestOnly(t *testing.T) {
	e, a, r, s, now := confirmationFixture(t, false, nil)
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "confirmed" || str(r, "activation") != "new_cycle" || str(r, "confirmation_evidence") != "trace_and_event" {
		t.Fatalf("new cycle evidence not confirmed %#v", r)
	}
	e, a, r, _, now = confirmationFixture(t, false, nil)
	if err := e.confirm(a, r, nil, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "request_completed" || str(r, "activation") != "" {
		t.Fatalf("successful request invented quota activation %#v", r)
	}
}
func TestSuccessfulCodexRequestWaits180SecondsBeforeRequestOnly(t *testing.T) {
	e, a, r, _, now := confirmationFixture(t, false, nil)
	if err := e.confirm(a, r, nil, now-20001); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") == "request_completed" || str(r, "status") == "confirmed" {
		t.Fatalf("premature request completion %#v", r)
	}
	if err := e.confirm(a, r, nil, now-20000); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "request_completed" {
		t.Fatalf("180s did not complete %#v", r)
	}
}
func TestConfirmationRetriesOnlyProvenSafeFailures(t *testing.T) {
	for _, code := range []int{0, 400, 401, 403, 404, 422, 429, 500, 503} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var status *int
			if code != 0 {
				c := code
				status = &c
			}
			e, a, r, s, now := confirmationFixture(t, true, status)
			if err := e.confirm(a, r, s, now); err != nil {
				t.Fatal(err)
			}
			want := "unconfirmed"
			if code == 403 {
				want = "error"
			} else if code == 400 || code == 401 || code == 404 || code == 422 || code == 429 {
				want = "retry"
			}
			if str(r, "status") != want {
				t.Fatalf("status %d got %#v expected %s", code, r, want)
			}
			if want == "retry" && num(r, "next_attempt_ms") != now+60000 {
				t.Fatal("retry did not delay 60s")
			}
		})
	}
}
func TestMissingOrMismatchedEvidenceRemainsUnconfirmed(t *testing.T) {
	e, a, r, s, now := confirmationFixture(t, false, nil)
	e.api = eventsAPI(t, func(q map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	if !unresolved(r) || str(r, "status") == "retry" {
		t.Fatalf("missing evidence made retry safe %#v", r)
	}
	e, a, r, s, now = confirmationFixture(t, false, nil)
	r["requested_model"] = "wrong/model"
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "unconfirmed" || str(r, "last_reason") != "event-evidence-mismatch" {
		t.Fatalf("wrong evidence accepted %#v", r)
	}
}
func TestAntigravityRequestOnlyNeverClaimsActivation(t *testing.T) {
	e, a, r, _, now := confirmationFixture(t, false, nil)
	a.Provider = "antigravity"
	r["identity"] = a.identity()
	r["scope"] = "gemini"
	v := Event{Hash: "hash", RequestID: "request", Timestamp: now - 199999, RequestedModel: "prefix/model", Model: "model", UA: "cpa-quota-guard/2 uuid", Index: a.Index, File: a.Name, Provider: a.Provider, KeyHash: hash("prewarm-test"), Reasoning: "low"}
	r["trace"] = "antigravity-index-request"
	e.api = eventsAPI(t, func(q map[string]any) any { return eventPage([]Event{v}, 1, false, 0, 0) })
	if err := e.confirm(a, r, nil, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "unconfirmed" {
		t.Fatalf("AG request only falsely completed %#v", r)
	}
}
