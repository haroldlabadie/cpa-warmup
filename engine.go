package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

var tz = time.FixedZone("Asia/Shanghai", 8*3600)

type Engine struct {
	mu              sync.Mutex
	cfg             Bootstrap
	settings        Settings
	instance        string
	revision        string
	store           *Store
	api             *API
	ctx             context.Context
	cancel          context.CancelFunc
	done            chan struct{}
	running         bool
	stopped         bool
	lastTick        int64
	lastError       string
	observations    map[string]any
	snapshotTimes   map[string]int64
	snapshotDigests map[string]string
}

func (e *Engine) fail(message string) {
	e.mu.Lock()
	e.lastError = message
	e.stopped = true
	e.mu.Unlock()
	e.cancel()
}
func (e *Engine) save() error {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if err := e.store.saveLocked(); err != nil {
		e.fail("state persistence failed; automatic actions stopped")
		return err
	}
	return nil
}
func cloneRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var out Record
	decodePreserving(b, &out)
	return out
}
func (e *Engine) put(slot, key string, r Record) error {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if e.store.state.Slots[slot] == nil {
		e.store.state.Slots[slot] = map[string]Record{}
	}
	e.store.state.Slots[slot][key] = cloneRecord(r)
	if err := e.store.saveLocked(); err != nil {
		e.fail("state persistence failed; automatic actions stopped")
		return err
	}
	return nil
}
func (e *Engine) records(key string) map[string]Record {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	out := map[string]Record{}
	for sid, rs := range e.store.state.Slots {
		if r := rs[key]; r != nil {
			out[sid] = cloneRecord(r)
		}
	}
	return out
}
func (e *Engine) snapshotSettings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := json.Marshal(e.settings)
	var s Settings
	json.Unmarshal(b, &s)
	return s
}
func (e *Engine) loop() {
	defer close(e.done)
	defer func() {
		if recover() != nil {
			e.fail("worker panic; automatic actions stopped")
		}
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	for {
		if e.ctx.Err() != nil {
			return
		}
		s := e.snapshotSettings()
		if e.cfg.Enabled {
			if err := e.tick(s, time.Now().UnixMilli()); err != nil {
				e.mu.Lock()
				e.lastError = err.Error()
				e.mu.Unlock()
			}
		}
		timer := time.NewTimer(time.Duration(s.CheckSeconds) * time.Second)
		select {
		case <-e.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (e *Engine) stop() {
	e.cancel()
	<-e.done
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	e.store.close()
}
func slotAt(now int64, s Settings) (string, int64) {
	t := time.UnixMilli(now).In(tz)
	var best time.Time
	for _, h := range s.SlotHours {
		candidate := time.Date(t.Year(), t.Month(), t.Day(), h, 0, 0, 0, tz)
		if !candidate.After(t) && candidate.After(best) {
			best = candidate
		}
	}
	if best.IsZero() || now >= best.UnixMilli()+int64(s.GraceSeconds)*1000 {
		return "", 0
	}
	return best.Format("2006-01-02T15:04-07:00"), best.UnixMilli()
}
func nextSlot(now int64, s Settings) string {
	t := time.UnixMilli(now).In(tz)
	for d := 0; d < 2; d++ {
		day := t.AddDate(0, 0, d)
		for _, h := range s.SlotHours {
			x := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, tz)
			if x.After(t) {
				return x.Format(time.RFC3339)
			}
		}
	}
	return ""
}
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
func excluded(a Account, model string) bool {
	for _, pattern := range a.Excluded {
		if ok, _ := path.Match(strings.ToLower(strings.TrimSpace(pattern)), strings.ToLower(model)); ok {
			return true
		}
	}
	return false
}
func validPrefix(a Account, inventory []Account) bool {
	if a.Prefix == "" || len(a.Prefix) > 128 || strings.ContainsAny(a.Prefix, "/\\ \t\r\n") {
		return false
	}
	for _, b := range inventory {
		if b.ID != a.ID && b.Prefix == a.Prefix {
			return false
		}
	}
	return true
}
func (e *Engine) identityOK(a Account) bool {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	old, exists := e.store.state.Identities[a.Name]
	if exists {
		return old == a.identity()
	}
	// A new installation binds its first inventory. Migrated attempts must have
	// been bound by the offline migration helper, never by guessing a filename.
	for _, rs := range e.store.state.Slots {
		for _, r := range rs {
			if str(r, "account") == a.Name && unresolved(r) {
				return false
			}
		}
	}
	e.store.state.Identities[a.Name] = a.identity()
	if e.store.saveLocked() != nil {
		e.fail("identity persistence failed")
		return false
	}
	return true
}
func (e *Engine) tick(s Settings, now int64) error {
	e.mu.Lock()
	e.lastTick = now
	e.mu.Unlock()
	accounts, err := inventory()
	if err != nil {
		return errors.New("credential inventory unavailable")
	}
	e.expire(s, now)
	allowed := map[string]bool{}
	for _, n := range s.Accounts {
		allowed[n] = true
	}
	for _, a := range accounts {
		if e.ctx.Err() != nil {
			return e.ctx.Err()
		}
		if len(allowed) > 0 && !allowed[a.Name] {
			continue
		}
		sample, probeErr := e.api.probe(e.ctx, a)
		// Quota refresh may update token metadata. Re-read identity/routing before
		// scheduling so a concurrent disable or credential replacement is respected.
		current, readErr := inventory()
		if readErr != nil {
			return errors.New("inventory refresh unavailable")
		}
		found := false
		for _, b := range current {
			if b.ID == a.ID && b.Index == a.Index && b.identity() == a.identity() {
				a = b
				found = true
				break
			}
		}
		if !found {
			continue
		}
		accounts = current
		if probeErr == nil {
			if publishErr := e.publishSample(e.ctx, a, sample, time.Now().UnixMilli()); publishErr != nil {
				e.mu.Lock()
				e.lastError = "quota snapshot publication failed"
				e.mu.Unlock()
			}
		}
		e.mu.Lock()
		if probeErr != nil {
			e.observations[a.Name] = map[string]any{"error": "quota unavailable"}
		} else {
			e.observations[a.Name] = sample
		}
		e.mu.Unlock()
		scopes := []string{"all"}
		if a.Provider == "antigravity" {
			scopes = []string{"gemini", "third_party"}
		}
		for _, scope := range scopes {
			if err := e.schedule(a, scope, sample, accounts, s, time.Now().UnixMilli()); err != nil && e.ctx.Err() == nil {
				e.mu.Lock()
				e.lastError = err.Error()
				e.mu.Unlock()
			}
		}
	}
	return e.save()
}
func parseSlot(s string) (time.Time, error) {
	t, e := time.Parse("2006-01-02T15:04-07:00", s)
	if e != nil {
		return time.Parse(time.RFC3339, s)
	}
	return t, nil
}
func (e *Engine) expire(s Settings, now int64) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	cutoff := time.UnixMilli(now).In(tz).AddDate(0, 0, -7).Format("2006-01-02")
	for sid, rs := range e.store.state.Slots {
		t, err := parseSlot(sid)
		if err != nil {
			e.fail("invalid slot timestamp; stopped")
			return
		}
		active := false
		for _, r := range rs {
			if now >= t.UnixMilli()+int64(s.GraceSeconds)*1000 && (str(r, "status") == "pending" || str(r, "status") == "retry") {
				r["status"] = "skipped"
				r["last_reason"] = "slot-expired:" + str(r, "last_reason")
			}
			active = active || unresolved(r)
		}
		if sid[:10] < cutoff && !active {
			for _, r := range rs {
				delete(e.store.state.AttemptGates, str(r, "attempt_token"))
			}
			delete(e.store.state.Slots, sid)
		}
	}
}
func (e *Engine) schedule(a Account, scope string, sample *Sample, inventory []Account, s Settings, now int64) error {
	key := a.Name
	if scope == "third_party" {
		key += "::third_party"
	}
	records := e.records(key)
	if !e.identityOK(a) {
		return errors.New("credential identity changed or migration proof missing; history retained")
	}
	for sid, r := range records {
		if unresolved(r) {
			if err := e.confirm(a, r, sample, now); err != nil {
				r["last_reason"] = "event-query-unknown"
			}
			if err := e.put(sid, key, r); err != nil {
				return err
			}
		}
	}
	sid, start := slotAt(now, s)
	if sid == "" {
		return nil
	}
	records = e.records(key)
	r := records[sid]
	if r == nil {
		r = Record{"slot": sid, "status": "pending", "attempts": 0, "scope": scope, "account": a.Name, "identity": a.identity()}
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
	model := s.Models[scope]
	if scope == "all" {
		model = s.Models["codex"]
	}
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
		if due >= start+int64(s.GraceSeconds)*1000 {
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
	if err = e.put(sid, key, r); err != nil {
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

func uuid() (string, error) {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", e
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
func (e *Engine) confirm(a Account, r Record, sample *Sample, now int64) error {
	if num(r, "sent_ms") <= 0 || str(r, "attempt_token") == "" {
		return errors.New("historical attempt proof incomplete")
	}
	if id := str(r, "identity"); id != "" && id != a.identity() {
		return errors.New("historical credential identity mismatch")
	}
	rows, err := e.api.attempt(e.ctx, a, r, now)
	if err != nil {
		return err
	}
	matched := []Event{}
	successful := []Event{}
	for _, v := range rows {
		if !eventMatches(a, r, v, hash(strings.TrimSpace(e.api.secrets.PrewarmKey)), now) {
			r["status"] = "unconfirmed"
			r["last_reason"] = "event-evidence-mismatch"
			return nil
		}
		matched = append(matched, v)
		if !v.Failed {
			successful = append(successful, v)
		}
	}
	if len(successful) > 0 {
		sort.Slice(successful, func(i, j int) bool { return successful[i].Timestamp < successful[j].Timestamp })
		event := successful[len(successful)-1]
		w := warmWindow(sample, str(r, "scope"))
		if str(r, "scope") == "" {
			scope := "all"
			if a.Provider == "antigravity" {
				scope = "gemini"
			}
			w = warmWindow(sample, scope)
		}
		stable := false
		if w != nil && sample != nil {
			candidate := obj(r["boundary_candidate"])
			stable = sample.Observed-num(candidate, "observed_ms") >= 10000 && num(candidate, "observed_ms") > 0 && abs(w.EndMS-num(candidate, "end_ms")) <= 3000
			if sample.Observed >= event.Timestamp {
				r["boundary_candidate"] = Record{"observed_ms": sample.Observed, "end_ms": w.EndMS}
			}
		}
		if w != nil && sample.Observed >= event.Timestamp && sample.Observed <= now && now-sample.Observed <= 90000 && (w.used() > 0 || stable) && w.EndMS-w.Duration <= event.Timestamp+120000 && event.Timestamp < w.EndMS && w.EndMS > now {
			start := w.EndMS - w.Duration
			r["status"] = "confirmed"
			r["event_hash"] = event.Hash
			r["request_id"] = event.RequestID
			r["confirmed_ms"] = now
			r["cycle_start_ms"] = start
			r["cycle_end_ms"] = w.EndMS
			r["activation"] = "existing_cycle"
			if start >= event.Timestamp-2000 && start <= event.Timestamp+120000 {
				r["activation"] = "new_cycle"
			}
			r["confirmed_auth_index"] = a.Index
			r["confirmation_evidence"] = "event_and_quota"
			if str(r, "trace") != "" {
				r["confirmation_evidence"] = "trace_and_event"
			}
			r["last_reason"] = "verified"
			return nil
		}
		if a.Provider == "codex" && now-num(r, "sent_ms") >= 180000 {
			r["status"] = "request_completed"
			r["event_hash"] = event.Hash
			r["request_id"] = event.RequestID
			r["completed_ms"] = now
			r["confirmed_auth_index"] = a.Index
			r["confirmation_evidence"] = "event_only"
			if str(r, "trace") != "" {
				r["confirmation_evidence"] = "trace_and_event"
			}
			r["last_reason"] = "successful-request:cycle-unverified"
			return nil
		}
	}
	if a.Provider == "codex" && len(successful) == 0 && complete(sample, "all", now) {
		w := warmWindow(sample, "all")
		candidate := obj(r["recovery_boundary"])
		stable := num(candidate, "observed_ms") > 0 && sample.Observed-num(candidate, "observed_ms") >= 10000 && abs(w.EndMS-num(candidate, "end_ms")) <= 3000
		r["recovery_boundary"] = Record{"observed_ms": sample.Observed, "end_ms": w.EndMS}
		if w.EndMS > now && w.EndMS-w.Duration > num(r, "sent_ms")+45000 && w.EndMS-w.Duration <= now && now >= num(r, "sent_ms")+fiveHour+45000 && (w.used() > 0 || stable) {
			r["status"] = "uncertainty_expired"
			r["retired_ms"] = now
			r["recovery_cycle_end_ms"] = w.EndMS
			r["last_reason"] = "old-request:later-cycle-observed"
			return nil
		}
	}
	if len(matched) > 0 && len(successful) == 0 {
		safe := true
		policy := false
		for _, v := range matched {
			if v.Status == nil {
				safe = false
				continue
			}
			switch *v.Status {
			case 400, 401, 403, 404, 422, 429:
			default:
				safe = false
			}
			if *v.Status == 403 {
				policy = true
			}
		}
		if policy {
			r["status"] = "error"
			r["last_reason"] = "policy-blocked"
			return nil
		}
		if safe {
			r["status"] = "retry"
			r["next_attempt_ms"] = now + 60000
			r["last_reason"] = "upstream-failed"
			return nil
		}
	}
	if len(matched) > 0 || now-num(r, "sent_ms") >= 180000 {
		r["status"] = "unconfirmed"
		r["last_reason"] = "missing-event-or-new-cycle"
	}
	return nil
}
func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
