package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

func warmWindow(sample *Sample, scope string) *Window {
	if sample == nil {
		return nil
	}
	for _, w := range sample.Windows {
		if w.Scope == scope && w.Kind == "five_hour" {
			c := w
			return &c
		}
	}
	return nil
}
func weeklyWindow(sample *Sample, scope string) *Window {
	if sample == nil {
		return nil
	}
	for _, w := range sample.Windows {
		if w.Scope == scope && w.Kind == "weekly" {
			c := w
			return &c
		}
	}
	return nil
}
func complete(sample *Sample, scope string, now int64) bool {
	if sample == nil || now-sample.Observed < 0 || now-sample.Observed > 90000 {
		return false
	}
	has5, hasW := false, false
	for _, w := range sample.Windows {
		if w.Scope == scope {
			if w.Kind == "weekly" {
				if w.EndMS <= now {
					return false
				}
				hasW = true
			}
			if w.Kind == "five_hour" {
				has5 = true
			}
		}
	}
	return has5 && hasW
}
func activeUntil(w Window, now, latest int64) (int64, error) {
	if w.EndMS <= now {
		if w.used() > 0 {
			return 0, errors.New("expired nonzero 5h evidence")
		}
		return 0, nil
	}
	if w.used() > 0 {
		return w.EndMS, nil
	}
	if latest > 0 && latest >= w.EndMS-w.Duration-1000 && latest <= now {
		return w.EndMS, nil
	}
	return 0, nil
}

// scopeAllowed is the family allowlist. Codex has one credential-wide window;
// Antigravity has two independent model families. A record of another family
// or provider cannot borrow evidence from a sample.
func scopeAllowed(provider, scope string) bool {
	if provider == "codex" {
		return scope == "all"
	}
	if provider == "antigravity" {
		return scope == "gemini" || scope == "third_party"
	}
	return false
}

func scopesFor(a Account) []string {
	if a.Provider == "antigravity" {
		return []string{"gemini", "third_party"}
	}
	return []string{"all"}
}

// targetsFor keeps one provider's families independent. A scope that is not
// configured for this provider is never scheduled.
func targetsFor(s Settings, a Account) []Target {
	out := []Target{}
	for _, t := range s.Targets {
		scope := scopeForTarget(a.Provider, t.Scope)
		if scopeAllowed(a.Provider, scope) && targetSelected(t, a) {
			t.Scope = scope
			out = append(out, t)
		}
	}
	return out
}

func targetSelected(t Target, a Account) bool {
	if len(t.Accounts) == 0 {
		return true
	}
	for _, name := range t.Accounts {
		if name == a.Name {
			return true
		}
	}
	return false
}

func accountSelected(s Settings, a Account) bool {
	for _, t := range s.Targets {
		if scopeAllowed(a.Provider, scopeForTarget(a.Provider, t.Scope)) && targetSelected(t, a) {
			return true
		}
	}
	return false
}

// scopeForTarget maps the public Codex channel to the credential-wide scope
// used by the scheduler. Other channels stay as-is so a Codex target cannot
// accidentally match an Antigravity family (or vice versa).
func scopeForTarget(provider, scope string) string {
	if provider == "codex" && scope == "codex" {
		return "all"
	}
	return scope
}

func targetForScope(s Settings, provider, scope string) (Target, bool) {
	for _, t := range s.Targets {
		if scopeForTarget(provider, t.Scope) == scope {
			return t, true
		}
	}
	return Target{}, false
}
func recordKey(name, scope string) string {
	if scope == "third_party" {
		return name + "::third_party"
	}
	return name
}

func (e *Engine) records(key string) map[string]Record {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	out := map[string]Record{}
	sealed := sealedIndex(e.store.state.IdentityGenerations[accountName(key)])
	for sid, rs := range e.store.state.Slots {
		if r := rs[key]; r != nil {
			if _, ok := sealed[sid+"\x00"+key]; ok {
				continue
			}
			out[sid] = cloneRecord(r)
		}
	}
	return out
}

// sealedIndex identifies exact sealed slot/key pairs. A sealed record is
// immutable evidence; no scheduling decision may rewrite its status/reason.
func sealedIndex(g *Generation) map[string]struct{} {
	out := map[string]struct{}{}
	if g == nil {
		return out
	}
	for _, s := range g.Sealed {
		out[s.Slot+"\x00"+s.Key] = struct{}{}
	}
	return out
}

func (e *Engine) put(slot, key string, r Record) error {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if g := e.store.state.IdentityGenerations[accountName(key)]; g != nil {
		if _, ok := sealedIndex(g)[slot+"\x00"+key]; ok {
			return errors.New("sealed-history-immutable")
		}
	}
	if e.store.state.Slots[slot] == nil {
		e.store.state.Slots[slot] = map[string]Record{}
	}
	e.store.state.Slots[slot][key] = cloneRecord(r)
	if err := e.store.saveLocked(); err != nil {
		e.fail("state persistence failed; automatic actions stopped")
		return errStopped
	}
	return nil
}

func (e *Engine) schedule(a Account, scope string, sample *Sample, inventory []Account, s Settings, now int64) error {
	key := recordKey(a.Name, scope)
	start, reason := e.ensureIdentity(a)
	if start != "ok" {
		return errors.New("identity:" + reason + "; history retained")
	}
	records := e.records(key)
	for sid, r := range records {
		if !unresolved(r) {
			continue
		}
		// A family is independent, but every unresolved record still needs a
		// matching family/provider. Wrong-family records stay unresolved.
		if !scopeAllowed(a.Provider, str(r, "scope")) {
			r["last_reason"] = "historical-model-family-mismatch"
			if err := e.put(sid, key, r); err != nil {
				return err
			}
			continue
		}
		if err := e.confirm(a, r, sample, now); err != nil {
			if err == errStopped {
				return err
			}
			r["last_reason"] = "event-query-unknown"
		}
		if err := e.put(sid, key, r); err != nil {
			return err
		}
	}
	sid, slotStart := slotAt(now, s)
	if sid == "" {
		return nil
	}
	if ok, floorReason := e.slotAdmitted(a.Name, slotStart); !ok {
		// A new generation cannot use a slot that began at or before the
		// retirement floor. Do not create or rewrite a record in that slot;
		// the current tick error and family view carry the reason.
		return errors.New(floorReason)
	}
	records = e.records(key)
	r := records[sid]
	if r == nil {
		r = Record{"slot": sid, "status": "pending", "attempts": 0, "scope": scope, "account": a.Name, "identity": a.identity(), "generation": identityGeneration(e, a.Name)}
		if g := identityGenerationNumber(e, a.Name); g > 0 {
			r["generation"] = g
		}
	}
	wait := func(reason string) error { r["last_reason"] = reason; return e.put(sid, key, r) }
	switch str(r, "status") {
	case "confirmed", "request_completed", "uncertainty_expired", "sent", "sending", "skipped", "error", "unconfirmed":
		return nil
	}
	for oldid, old := range records {
		if oldid != sid && unresolved(old) {
			return wait("unresolved-prior-request")
		}
	}
	if sample == nil || now-sample.Observed < 0 || now-sample.Observed > 90000 {
		return wait("probe-unknown")
	}
	w := warmWindow(sample, scope)
	if w == nil {
		if a.Provider == "codex" {
			r["status"] = "skipped"
			return wait("no-five-hour-window")
		}
		return wait("family-quota-unknown")
	}
	if !complete(sample, scope, now) {
		return wait("family-quota-unknown")
	}
	if a.Disabled {
		return wait("disabled")
	}
	target, ok := targetForScope(s, a.Provider, scope)
	if !ok || target.Model == "" {
		return wait("model-not-configured")
	}
	model := target.Model
	if excluded(a, model) {
		return wait("model-excluded")
	}
	for _, q := range sample.Windows {
		if q.Scope == scope && (q.Kind == "five_hour" || q.Kind == "weekly") && q.Remaining <= 0 {
			return wait("quota-exhausted")
		}
	}
	latest, err := e.api.latest(e.ctx, a, scope, now)
	if err != nil {
		return wait("activity-history-unknown")
	}
	e.store.mu.Lock()
	e.store.state.LatestSuccess[a.identity()+":"+scope] = latest
	e.store.mu.Unlock()
	until, err := activeUntil(*w, now, latest)
	if err != nil {
		return wait("quota-boundary-unknown")
	}
	if until > 0 {
		due := until + 15000
		if due >= slotStart+int64(s.GraceSeconds)*1000 {
			if scope == "third_party" {
				r["status"] = "skipped"
				r["cycle_end_ms"] = until
				return wait("already-active")
			}
			delete(r, "deferred_until_ms")
			return wait(fmt.Sprintf("active-beyond-grace:%d", until))
		}
		r["deferred_until_ms"] = due
		return wait(fmt.Sprintf("active:%d", until))
	}
	if scope != "third_party" && num(r, "deferred_until_ms") != w.EndMS+15000 {
		delete(r, "deferred_until_ms")
	}
	if now < num(r, "deferred_until_ms") || now < num(r, "next_attempt_ms") {
		return wait("reset-buffer-or-retry-delay")
	}
	// Even an expired zero bucket must pass its advertised boundary + 15s.
	if w.EndMS <= now && now < w.EndMS+15000 {
		return wait("reset-buffer")
	}
	if num(r, "attempts") >= 3 {
		r["status"] = "error"
		return wait("attempt-limit")
	}
	// Repeat the sealed-generation floor immediately before any durable
	// attempt. Old slots are never rewritten or replayed.
	if ok, reason := e.slotAdmitted(a.Name, slotStart); !ok {
		return wait(reason)
	}
	if !validPrefix(a, inventory) {
		return wait("prefix-missing-conflicting-or-invalid")
	}
	if !e.cfg.Enabled || e.cfg.Observe || !s.Enabled {
		return wait("observe-or-paused")
	}
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	// Last routing check immediately before persisting the attempt. The after-auth
	// callback provides the execution-time identity guard for the remaining race.
	live, err := inventoryFn()
	if err != nil {
		return wait("inventory-unknown")
	}
	valid := false
	for _, b := range live {
		if b.ID == a.ID && b.identity() == a.identity() && !b.Disabled && b.Prefix == a.Prefix && !excluded(b, model) && validPrefix(b, live) {
			valid = true
			break
		}
	}
	if !valid {
		return wait("routing-changed")
	}
	if !e.ready(e.ctx) {
		return wait("host-hooks-not-ready")
	}
	if e.ctx.Err() != nil {
		return e.ctx.Err()
	}
	token, err := uuid()
	if err != nil {
		return err
	}
	for _, field := range []string{"request_id", "trace", "transport_error", "boundary_candidate"} {
		delete(r, field)
	}
	r["status"] = "sending"
	r["attempts"] = num(r, "attempts") + 1
	r["attempt_token"] = token
	r["sent_ms"] = now
	r["requested_model"] = a.Prefix + "/" + model
	r["target_auth_id"] = a.ID
	r["target_auth_index"] = a.Index
	r["identity"] = a.identity()
	gate := Gate{AuthID: a.ID, AuthIndex: a.Index, Identity: a.identity(), RequestedModel: str(r, "requested_model"), CallerScope: hash("cli-proxy-api:caller-scope:v1\x00" + strings.TrimSpace(e.api.secrets.PrewarmKey)), SentMS: now}
	e.store.mu.Lock()
	e.store.state.AttemptGates[token] = gate
	e.store.mu.Unlock()
	if err := e.put(sid, key, r); err != nil {
		return err
	}
	trace, sendErr := e.api.send(e.ctx, str(r, "requested_model"), token)
	r["status"] = "sent"
	if trace != "" {
		r["trace"] = trace
		p := strings.SplitN(trace, "-", 3)
		if len(p) == 3 && p[1] == a.Index {
			r["request_id"] = p[2]
		}
	}
	if sendErr != nil {
		r["transport_error"] = sendErr.Error()
	}
	return e.put(sid, key, r)
}

var inventoryFn = inventory

// stableBoundary records a new observation and returns true only when the
// previous candidate was fresh and at least ten seconds earlier with the same
// cycle boundary. A moving full-quota hint therefore never becomes stable.
func stableBoundary(r Record, field string, w *Window, sample *Sample, now int64) bool {
	if w == nil || sample == nil || now <= 0 || sample.Observed <= 0 || sample.Observed > now || now-sample.Observed > 90000 {
		return false
	}
	candidate := obj(r[field])
	candidateMS := num(candidate, "observed_ms")
	delta := sample.Observed - candidateMS
	stable := candidateMS > 0 && delta >= 10000 && delta <= 90000 && candidateMS <= now &&
		str(candidate, "scope") == w.Scope && num(candidate, "duration_ms") == w.Duration &&
		abs(w.EndMS-num(candidate, "end_ms")) <= 3000
	// Cache only a fresh, time-valid observation. Invalid or expired samples
	// must not poison the next stability comparison.
	r[field] = Record{"observed_ms": sample.Observed, "end_ms": w.EndMS, "scope": w.Scope, "duration_ms": w.Duration}
	return stable
}

func sortedSuccessful(events []Event) []Event {
	out := append([]Event(nil), events...)
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out
}
