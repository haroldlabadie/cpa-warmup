package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewStableFutureCycleRetiresOnlyWithTwoFreshSamples(t *testing.T) {
	e, a, r, s, now := reviewRetireFixture(t, "codex", "all")
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	// The later cycle starts before both observations. Only its boundary is
	// stable; moving the boundary must prevent retirement.
	s.Windows[0].Remaining = 100
	s.Windows[0].EndMS = now - 20000 + fiveHour
	s.Observed = now - 10000
	if err := e.confirm(a, r, s, now-10000); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") == "uncertainty_expired" {
		t.Fatal("one sample retired")
	}
	s2 := *s
	s2.Observed = now
	if err := e.confirm(a, r, &s2, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "uncertainty_expired" {
		t.Fatalf("two stable fresh samples did not retire: %#v", r)
	}

	e, a, r, s, now = reviewRetireFixture(t, "codex", "all")
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	s.Windows[0].Remaining = 100
	s.Windows[0].EndMS = now - 20000 + fiveHour
	s.Observed = now - 10000
	_ = e.confirm(a, r, s, now-10000)
	s2 = *s
	s2.Observed = now
	s2.Windows[0].EndMS += 4000
	if err := e.confirm(a, r, &s2, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") == "uncertainty_expired" {
		t.Fatal("moving boundary retired")
	}

	e, a, r, s, now = reviewRetireFixture(t, "codex", "all")
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	s.Windows[0].Remaining = 100
	s.Observed = now - 120000
	_ = e.confirm(a, r, s, now-120000)
	s2 = *s
	s2.Observed = now
	if err := e.confirm(a, r, &s2, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") == "uncertainty_expired" {
		t.Fatal("expired candidate made stability")
	}
}

func TestMismatchedEventStillRetiresAfterProvenLaterCycle(t *testing.T) {
	e, a, r, s, now := reviewRetireFixture(t, "codex", "all")
	v := Event{Hash: "other", RequestID: "wrong", Timestamp: now - 50000, RequestedModel: "prefix/other", Model: "model", UA: "cpa-quota-guard/2 uuid", Index: a.Index, File: a.Name, Provider: a.Provider, KeyHash: hash("prewarm-test"), Reasoning: "low"}
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage([]Event{v}, 1, false, 0, 0) })
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "uncertainty_expired" || str(r, "retirement") != "later-cycle-without-success-event" {
		t.Fatalf("mismatched event kept a proven later cycle blocked: %#v", r)
	}
}

func TestMismatchedEventRemainsUnconfirmedWithoutLaterCycle(t *testing.T) {
	e, a, r, s, now := confirmationFixture(t, false, nil)
	matchedFailure := Event{Hash: "failed", RequestID: "failed-request", Timestamp: now - 150000, RequestedModel: "prefix/model", UA: "cpa-quota-guard/2 uuid", Index: a.Index, File: a.Name, Provider: a.Provider, KeyHash: hash("prewarm-test"), Reasoning: "low", Failed: true, Status: intPtr(429)}
	wrong := matchedFailure
	wrong.Hash, wrong.RequestID, wrong.RequestedModel = "other", "wrong", "prefix/other"
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage([]Event{wrong, matchedFailure}, 2, false, 0, 0) })
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "unconfirmed" || str(r, "last_reason") != "event-evidence-mismatch" || !unresolved(r) || r["next_attempt_ms"] != nil {
		t.Fatalf("mixed mismatched evidence became retryable or resolved: %#v", r)
	}
}

func intPtr(v int) *int { return &v }

func TestReviewBindRequiresMachineProofAndSharesPlan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gate   bool
		record bool
		wantOK bool
	}{
		{"matching-record-identity", false, true, true},
		{"matching-persisted-gate", true, false, true},
		{"no-machine-proof", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Now().UnixMilli()
			a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index", Prefix: "prefix"}
			r := Record{"status": "unconfirmed", "sent_ms": now - fiveHour - 50000, "attempt_token": "proof", "account": a.Name, "scope": "all", "target_auth_id": a.ID, "target_auth_index": a.Index}
			if tc.record {
				r["identity"] = a.identity()
			}
			slots := Slots{"2026-01-01T05:00+08:00": {a.Name: r}}
			s := State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: "migration-unproven"}}
			if tc.gate {
				s.AttemptGates["proof"] = Gate{AuthID: a.ID, AuthIndex: a.Index, Identity: a.identity(), SentMS: num(r, "sent_ms")}
			}
			if err := atomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
				t.Fatal(err)
			}
			if f, err := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600); err != nil {
				t.Fatal(err)
			} else {
				f.Close()
			}
			loaded, err := loadRecoveryState(dir)
			if err != nil {
				t.Fatal(err)
			}
			digest, _ := stateDigest(&loaded)
			req := recoverRequest{DataDir: dir, StateSHA256: digest, OperationID: "bind-one", Account: a, Identity: a.identity(), Action: "bind", Operator: "reviewer", Reason: "machine evidence checked"}
			reqPath := filepath.Join(dir, "request.json")
			if err := atomicJSON(reqPath, req); err != nil {
				t.Fatal(err)
			}
			if code := recoverIdentityCommand(reqPath); (code == 0) != tc.wantOK {
				t.Fatalf("dry-run result %d want %v", code, tc.wantOK)
			}
			if tc.wantOK {
				req.Apply = true
				if err := atomicJSON(reqPath, req); err != nil {
					t.Fatal(err)
				}
				if code := recoverIdentityCommand(reqPath); code != 0 {
					t.Fatal("apply failed")
				}
				after, err := loadRecoveryState(dir)
				if err != nil {
					t.Fatal(err)
				}
				if after.Identities[a.Name] != a.identity() || len(after.IdentityRecovery) != 1 || after.IdentityRecovery[0].StateDigest != digest {
					t.Fatalf("bind not audited %#v", after.IdentityRecovery)
				}
				if string(mustJSON(after.Slots["2026-01-01T05:00+08:00"][a.Name])) != string(mustJSON(r)) {
					t.Fatal("bind rewrote historical record")
				}
			}
		})
	}
}

func TestReviewSealRejectsFutureMissingSentAndStaleDigest(t *testing.T) {
	for _, sent := range []int64{0, time.Now().UnixMilli() + 60000} {
		dir := t.TempDir()
		a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index"}
		r := Record{"status": "unconfirmed", "attempt_token": "x", "account": a.Name, "scope": "all"}
		if sent != 0 {
			r["sent_ms"] = sent
		}
		slots := Slots{"2026-01-01T05:00+08:00": {a.Name: r}}
		s := State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: "migration-unproven"}}
		if err := atomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
			t.Fatal(err)
		}
		loaded, _ := loadRecoveryState(dir)
		digest, _ := stateDigest(&loaded)
		req := recoverRequest{DataDir: dir, StateSHA256: digest, OperationID: "seal-bad", Account: a, Identity: a.identity(), Action: "seal-and-recover", Operator: "reviewer", Reason: "bad time"}
		path := filepath.Join(dir, "request.json")
		if err := atomicJSON(path, req); err != nil {
			t.Fatal(err)
		}
		if code := recoverIdentityCommand(path); code == 0 {
			t.Fatalf("accepted sent=%d", sent)
		}
	}

	dir := t.TempDir()
	a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index"}
	r := Record{"status": "unconfirmed", "attempt_token": "y", "sent_ms": time.Now().UnixMilli() - fiveHour - 60000, "account": a.Name, "scope": "all"}
	slots := Slots{"2026-01-01T05:00+08:00": {a.Name: r}}
	s := State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: "migration-unproven"}}
	if err := atomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	loaded, _ := loadRecoveryState(dir)
	digest, _ := stateDigest(&loaded)
	beforeState, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	staleDigest := "0" + digest[1:]
	if staleDigest == digest {
		staleDigest = "1" + digest[1:]
	}
	req := recoverRequest{DataDir: dir, StateSHA256: staleDigest, OperationID: "seal-stale", Account: a, Identity: a.identity(), Action: "seal-and-recover", Operator: "reviewer", Reason: "stale", Apply: true}
	path := filepath.Join(dir, "request.json")
	if err := atomicJSON(path, req); err != nil {
		t.Fatal(err)
	}
	if code := recoverIdentityCommand(path); code == 0 {
		t.Fatal("stale apply digest accepted")
	}
	afterState, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || string(afterState) != string(beforeState) {
		t.Fatal("rejected stale apply mutated state")
	}

	dir = t.TempDir()
	id := a.identity()
	seed := State{Schema: 2, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: id}, IdentityGenerations: map[string]*Generation{a.Name: {ID: "missing-audit", Identity: id, CreatedMS: 100, Operator: "reviewer", Reason: "x", FloorMS: 200, Generation: 1}}}
	if err := atomicJSON(filepath.Join(dir, "state.json"), seed); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(dir); err == nil {
		s.close()
		t.Fatal("generation without sealed evidence accepted")
	}

	if err := atomicJSON(filepath.Join(dir, "state.json"), State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: id}, IdentityRecovery: []IdentityRecovery{{ID: "x", Action: "bind", Account: a.Name, Operator: "reviewer", Reason: "x", CreatedMS: 1, AfterIdentity: id}}}); err != nil {
		t.Fatal(err)
	}
	if s, err := openStore(dir); err == nil {
		s.close()
		t.Fatal("schema 1 recovery audit accepted")
	}
}

func TestReviewSuccessEventStillWinsWhenEventConfirmsCycle(t *testing.T) {
	e, a, r, s, now := reviewRetireFixture(t, "codex", "all")
	v := Event{Hash: "hash", RequestID: "request", Timestamp: now - 50000, RequestedModel: "prefix/model", Model: "model", UA: "cpa-quota-guard/2 uuid", Index: a.Index, File: a.Name, Provider: a.Provider, KeyHash: hash("prewarm-test"), Reasoning: "low"}
	wrong := v
	wrong.Hash, wrong.RequestID, wrong.RequestedModel = "other", "wrong", "prefix/other"
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage([]Event{wrong, v}, 2, false, 0, 0) })
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") != "confirmed" || str(r, "activation") != "new_cycle" {
		t.Fatalf("successful event did not win over an unrelated row: %#v", r)
	}
}

func TestReviewBindCannotOverrideSealedGeneration(t *testing.T) {
	dir := t.TempDir()
	now := nowMillis()
	a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index"}
	r := Record{"status": "unconfirmed", "sent_ms": now - fiveHour - 60000, "attempt_token": "proof", "account": a.Name, "scope": "all", "identity": a.identity(), "target_auth_id": a.ID, "target_auth_index": a.Index}
	slots := Slots{"2026-01-01T05:00+08:00": {a.Name: r}}
	s := State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: "migration-unproven"}}
	if err := atomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if f, err := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600); err != nil {
		t.Fatal(err)
	} else {
		f.Close()
	}
	loaded, _ := loadRecoveryState(dir)
	digest, _ := stateDigest(&loaded)
	seal := recoverRequest{DataDir: dir, StateSHA256: digest, OperationID: "seal", Account: a, Identity: a.identity(), Action: "seal-and-recover", Operator: "reviewer", Reason: "seal", Apply: true}
	path := filepath.Join(dir, "request.json")
	if err := atomicJSON(path, seal); err != nil {
		t.Fatal(err)
	}
	if code := recoverIdentityCommand(path); code != 0 {
		t.Fatal("seal failed")
	}
	loaded, _ = loadRecoveryState(dir)
	digest, _ = stateDigest(&loaded)
	bind := seal
	bind.StateSHA256, bind.OperationID, bind.Action, bind.Apply = digest, "bind", "bind", true
	if err := atomicJSON(path, bind); err != nil {
		t.Fatal(err)
	}
	if code := recoverIdentityCommand(path); code == 0 {
		t.Fatal("bind over a sealed generation accepted")
	}
}
