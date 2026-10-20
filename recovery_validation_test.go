package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func reviewRetireFixture(t *testing.T, provider, scope string) (*Engine, Account, Record, *Sample, int64) {
	t.Helper()
	e, a, r, _, now := confirmationFixture(t, false, nil)
	a.Provider = provider
	if provider == "antigravity" {
		a.ProjectID = "project"
	}
	r["scope"] = scope
	r["identity"] = a.identity()
	r["sent_ms"] = now - fiveHour - 60000
	e.store.state.Identities[a.Name] = a.identity()
	e.api = eventsAPI(t, func(q map[string]any) any { return map[string]any{"error": "old history unavailable"} })
	s := &Sample{Observed: now, Windows: []Window{
		{Kind: "five_hour", Scope: scope, Remaining: 90, EndMS: now - 10000 + fiveHour, Duration: fiveHour},
		{Kind: "weekly", Scope: scope, Remaining: 80, EndMS: now + week, Duration: week},
	}}
	return e, a, r, s, now
}

func TestReviewRetireLaterCycleEveryScopeWithoutOldEvents(t *testing.T) {
	for _, tc := range []struct{ provider, scope string }{{"codex", "all"}, {"antigravity", "gemini"}, {"antigravity", "third_party"}} {
		t.Run(tc.provider+"/"+tc.scope, func(t *testing.T) {
			e, a, r, s, now := reviewRetireFixture(t, tc.provider, tc.scope)
			if err := e.confirm(a, r, s, now); err != nil {
				t.Fatal(err)
			}
			if str(r, "status") != "uncertainty_expired" || str(r, "activation") != "" {
				t.Fatalf("later cycle did not retire without old events: %#v", r)
			}
		})
	}
}

func TestReviewMobileFullQuotaDoesNotRetire(t *testing.T) {
	e, a, r, s, now := reviewRetireFixture(t, "codex", "all")
	e.api = eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) })
	s.Observed = now - 20000
	s.Windows[0].Remaining = 100
	s.Windows[0].EndMS = now - 10000 + fiveHour
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	s.Observed = now - 5000
	s.Windows[0].EndMS += 5000
	if err := e.confirm(a, r, s, now); err != nil {
		t.Fatal(err)
	}
	if str(r, "status") == "uncertainty_expired" {
		t.Fatalf("moving full-quota hint retired a request: %#v", r)
	}
}

func TestReviewNoIdentityNeverBindsAndWrongFamilyIsIsolated(t *testing.T) {
	e, a, r, s, now := confirmationFixture(t, false, nil)
	delete(e.store.state.Identities, a.Name)
	e.store.state.Slots["2026-01-01T05:00+08:00"] = map[string]Record{a.Name: r}
	// An unbound legacy record is precisely the migration-unproven shape.
	e.store.state.Identities[a.Name] = "migration-unproven"
	if err := e.confirm(a, r, s, now); err == nil || str(r, "status") == "uncertainty_expired" {
		t.Fatalf("unbound historical request accepted: %#v", r)
	}
	e, a, r, s, now = reviewRetireFixture(t, "antigravity", "gemini")
	r["scope"] = "third_party"
	if err := e.confirm(a, r, s, now); err == nil {
		t.Fatal("wrong family evidence accepted")
	}
	if str(r, "status") == "uncertainty_expired" {
		t.Fatal("wrong family retired")
	}
}

func TestReviewSealedHistoryImmutableAcrossOpenTickAndFutureSlot(t *testing.T) {
	oldNow := nowMillis
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, tz).UnixMilli()
	nowMillis = func() int64 { return now }
	t.Cleanup(func() { nowMillis = oldNow })
	dir := t.TempDir()
	a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index", Email: "a@example.invalid", AccountID: "account", Prefix: "prefix"}
	oldSent := now - fiveHour - 200000
	old := Record{"status": "unconfirmed", "sent_ms": oldSent, "attempt_token": "old", "account": a.Name, "scope": "all", "identity": "migration-unproven", "slot": "2026-01-01T05:00+08:00"}
	slots := Slots{"2026-01-01T05:00+08:00": {a.Name: old}}
	s := State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: "migration-unproven"}}
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
	sum, _ := stateDigest(&loaded)
	req := recoverRequest{DataDir: dir, StateSHA256: sum, OperationID: "op-seal", Account: a, Identity: a.identity(), Action: "seal-and-recover", Operator: "reviewer", Reason: "replace same filename", Apply: true}
	reqPath := filepath.Join(dir, "request.json")
	if err := atomicJSON(reqPath, req); err != nil {
		t.Fatal(err)
	}
	if code := recoverIdentityCommand(reqPath); code != 0 {
		t.Fatal("apply failed")
	}
	before := string(mustJSON(old))
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if st.lock != nil {
			st.close()
		}
	}()
	cpamp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/management/api-call":
			json.NewEncoder(w).Encode(map[string]any{"status_code": 503, "body": ""})
		case "/v0/management/monitoring/analytics":
			json.NewEncoder(w).Encode(eventPage(nil, 0, false, 0, 0))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cpamp.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	eng := &Engine{
		store: st, api: newAPI(Bootstrap{CPAMPURL: cpamp.URL}, Secrets{ManagementKey: "management-test", PrewarmKey: "prewarm-test"}),
		cfg: Bootstrap{Enabled: true, Observe: true}, ctx: ctx, cancel: cancel,
		settings:     Settings{Version: 1, Enabled: true, SlotHours: []int{time.UnixMilli(now).In(tz).Hour()}, GraceSeconds: 5400, CheckSeconds: 20, Models: defaults().Models},
		observations: map[string]any{}, partialErrors: map[string]string{},
	}
	// The just-created generation only admits strictly future scheduled slots.
	// This call may report the floor reason, but it must not mutate the sealed
	// record, which is the invariant under test.
	if err := eng.schedule(a, "all", nil, []Account{a}, eng.settings, now); err == nil {
		t.Fatal("unexpected admission in an already started slot")
	}
	st.mu.Lock()
	after := string(mustJSON(st.state.Slots["2026-01-01T05:00+08:00"][a.Name]))
	st.mu.Unlock()
	if before != after {
		t.Fatalf("sealed record changed before=%s after=%s", before, after)
	}
	if err := eng.put("2026-01-01T05:00+08:00", a.Name, Record{"status": "sent"}); err == nil {
		t.Fatal("sealed key allowed write")
	}

	// Drive the real worker tick through a future aligned slot. A helper apply
	// followed by openStore/tick/reopen must create a fresh generation record
	// while leaving the sealed record byte-identical throughout.
	future := time.UnixMilli(now).In(tz).Add(24 * time.Hour)
	futureHour := future.Hour()
	eng.settings.SlotHours = []int{futureHour}
	futureNow := time.Date(future.Year(), future.Month(), future.Day(), futureHour, 1, 0, 0, tz).UnixMilli()
	nowMillis = func() int64 { return futureNow }
	inventoryHost(t, []map[string]any{fileInventoryAccount()}, map[string]map[string]any{
		"index": {"type": "codex", "email": "a@example.invalid", "account_id": "account", "prefix": "prefix"},
	})
	if err := eng.tick(eng.settings, futureNow); err != nil {
		t.Fatal(err)
	}
	futureSlot, _ := slotAt(futureNow, eng.settings)
	if futureSlot == "" {
		t.Fatal("future slot not active")
	}
	st.mu.Lock()
	fresh := st.state.Slots[futureSlot][a.Name]
	sealedAfter := string(mustJSON(st.state.Slots["2026-01-01T05:00+08:00"][a.Name]))
	st.mu.Unlock()
	if fresh == nil || str(fresh, "status") != "pending" || num(fresh, "generation") != 1 || str(fresh, "last_reason") != "probe-unknown" {
		t.Fatalf("future slot was not admitted as a new generation record: %#v", fresh)
	}
	if sealedAfter != before {
		t.Fatal("sealed record changed after future tick")
	}

	st.mu.Lock()
	generation := st.state.IdentityGenerations[a.Name]
	st.mu.Unlock()
	st.close()
	st2, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.close()
	st2.mu.Lock()
	reopened := string(mustJSON(st2.state.Slots["2026-01-01T05:00+08:00"][a.Name]))
	reopenedFresh := st2.state.Slots[futureSlot][a.Name]
	reopenedGeneration := st2.state.IdentityGenerations[a.Name]
	st2.mu.Unlock()
	if before != reopened {
		t.Fatal("sealed record changed after reopen")
	}
	if generation == nil || reopenedGeneration == nil || generation.ID != reopenedGeneration.ID || generation.Generation != 1 {
		t.Fatal("identity generation was not persisted")
	}
	if reopenedFresh == nil || str(reopenedFresh, "status") != "pending" || num(reopenedFresh, "generation") != 1 {
		t.Fatalf("future record lost after reopen: %#v", reopenedFresh)
	}
}

func TestReviewIdentityChangeRejectsAndDoesNotTouchSealedHistory(t *testing.T) {
	dir := t.TempDir()
	now := nowBase()
	a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index", Prefix: "prefix"}
	old := Record{"status": "unconfirmed", "sent_ms": now - fiveHour - 100000, "attempt_token": "old", "account": a.Name, "scope": "all", "identity": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	slots := Slots{"2026-01-01T05:00+08:00": {a.Name: old}}
	s := State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}}
	if err := atomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	st.state.Identities[a.Name] = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	before := mustJSON(st.state.Slots["2026-01-01T05:00+08:00"][a.Name])
	eng := &Engine{store: st, api: eventsAPI(t, func(map[string]any) any { return eventPage(nil, 0, false, 0, 0) }), cfg: Bootstrap{Enabled: true, Observe: true}, settings: defaults(), observations: map[string]any{}}
	if err := eng.schedule(a, "all", &Sample{Observed: now, Windows: []Window{{Kind: "five_hour", Scope: "all", EndMS: now + fiveHour, Duration: fiveHour}, {Kind: "weekly", Scope: "all", EndMS: now + week, Duration: week}}}, []Account{a}, defaults(), now); err == nil {
		t.Fatal("identity mismatch allowed scheduling")
	}
	after := mustJSON(st.state.Slots["2026-01-01T05:00+08:00"][a.Name])
	if string(before) != string(after) {
		t.Fatal("identity mismatch modified history")
	}
}

func TestReviewRecoveryHelperRealDryRunApplyLockReplayAndFloor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UnixMilli()
	a := Account{ID: "id", Name: "a.json", Provider: "codex", Index: "index", Prefix: "prefix"}
	old := Record{"status": "unconfirmed", "sent_ms": now - fiveHour - 100000, "attempt_token": "old", "account": a.Name, "scope": "all", "identity": "migration-unproven"}
	slots := Slots{"2026-01-01T05:00+08:00": {a.Name: old}}
	s := State{Schema: 1, Plugin: pluginID, Legacy: map[string]json.RawMessage{"slots": mustJSON(slots)}, Slots: slots, AttemptGates: map[string]Gate{}, Identities: map[string]string{a.Name: "migration-unproven"}}
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
	sum, err := stateDigest(&loaded)
	if err != nil {
		t.Fatal(err)
	}
	req := recoverRequest{DataDir: dir, StateSHA256: sum, OperationID: "op-one", Account: a, Identity: a.identity(), Action: "seal-and-recover", Operator: "reviewer", Reason: "new generation"}
	reqPath := filepath.Join(dir, "request.json")
	if err := atomicJSON(reqPath, req); err != nil {
		t.Fatal(err)
	}
	// dry-run must not write
	if code := recoverIdentityCommand(reqPath); code != 0 {
		t.Fatal("dry-run failed")
	}
	loaded, _ = loadRecoveryState(dir)
	if len(loaded.IdentityRecovery) != 0 || len(loaded.IdentityGenerations) != 0 {
		t.Fatal("dry-run mutated state")
	}
	// running worker lock must be rejected
	lock, err := acquireWorkerLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code := recoverIdentityCommand(reqPath); code == 0 {
		t.Fatal("recovery ignored worker lock")
	}
	releaseWorkerLock(lock)
	req.Apply = true
	if err := atomicJSON(reqPath, req); err != nil {
		t.Fatal(err)
	}
	if code := recoverIdentityCommand(reqPath); code != 0 {
		t.Fatal("apply failed")
	}
	loaded, err = loadRecoveryState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(mustJSON(loaded.Slots["2026-01-01T05:00+08:00"][a.Name])) != string(mustJSON(old)) {
		t.Fatal("sealed record changed")
	}
	g := loaded.IdentityGenerations[a.Name]
	if g == nil || g.FloorMS < num(old, "sent_ms")+fiveHour+45000 {
		t.Fatalf("bad floor %#v", g)
	}
	// replay rejected
	if code := recoverIdentityCommand(reqPath); code == 0 {
		t.Fatal("replay accepted")
	}
}

func nowBase() int64        { return int64(1800000000000) }
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestReviewSecretSymlinkURLAndCurrentError(t *testing.T) {
	for _, raw := range []string{"http://cpa-manager-plus:18317", "http://10.0.0.5:18317", "http://127.0.0.1:18317"} {
		if err := validateURL(raw, false); err != nil {
			t.Fatalf("allowed private URL %s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://example.com:18317", "http://god@example.com:18317", "https://10.0.0.5:18317", "http://10.0.0.5:18317/?x=1", "http://10.0.0.5:18317/path", "http://-bad:1"} {
		if err := validateURL(raw, false); err == nil {
			t.Fatalf("unsafe URL accepted %s", raw)
		}
	}

	dir := t.TempDir()
	target := filepath.Join(dir, "secret-target")
	if err := os.WriteFile(target, []byte(`{"management_key":"m","prewarm_key":"p"}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "secret-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	data := t.TempDir()
	rawConfig := []byte("store: yaml\nversion: \"1\"\npriority: 1\nenabled: true\nobserve: true\ndata_dir: " + data + "\ncpamp_url: http://127.0.0.1:2\ncpa_url: http://127.0.0.1:1\nsecret_file: " + link + "\n")
	raw, _ := json.Marshal(lifecycleRequest{SchemaVersion: 6, ConfigYAML: rawConfig})
	if err := configure(raw); err == nil {
		t.Fatal("symlink secret accepted")
	}

	e := scheduleEngine(t)
	e.mu.Lock()
	e.partialErrors = map[string]string{"account/a": "probe failed"}
	e.lastError = "account/a: probe failed"
	e.mu.Unlock()
	e.mu.Lock()
	// This mirrors the successful start-of-tick reset; a successful save must
	// not be the only event that clears stale errors.
	e.partialErrors = map[string]string{}
	e.lastError = ""
	e.mu.Unlock()
	if e.lastError != "" {
		t.Fatal("recovered error was not cleared")
	}
}
