package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

var tz = time.FixedZone("Asia/Shanghai", 8*3600)

// nowMillis is a test seam for deterministic worker ticks. Production keeps
// the wall clock; tests can advance it without sleeping a full day.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

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
	partialErrors   map[string]string
	observations    map[string]any
	snapshotTimes   map[string]int64
	snapshotDigests map[string]string
}

var errStopped = errors.New("worker stopped")

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
func (e *Engine) snapshotSettings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, _ := json.Marshal(e.settings)
	var s Settings
	_ = json.Unmarshal(b, &s)
	return s
}
func (e *Engine) setPartial(key, message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if message == "" {
		delete(e.partialErrors, key)
		return
	}
	e.partialErrors[key] = message
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
			if err := e.tick(s, nowMillis()); err != nil && e.ctx.Err() == nil {
				e.mu.Lock()
				if e.lastError == "" || !strings.HasPrefix(e.lastError, "state persistence") {
					e.lastError = err.Error()
				}
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
func parseSlot(s string) (time.Time, error) {
	t, e := time.Parse("2006-01-02T15:04-07:00", s)
	if e != nil {
		return time.Parse(time.RFC3339, s)
	}
	return t, nil
}

// tick reports the first fatal condition and keeps partial account/family
// errors in lastError. A successful save no longer erases probe failures.
func (e *Engine) tick(s Settings, now int64) error {
	e.mu.Lock()
	e.lastTick = now
	e.lastError = ""
	e.partialErrors = map[string]string{}
	e.mu.Unlock()
	accounts, err := inventory()
	if err != nil {
		e.mu.Lock()
		e.lastError = "credential inventory unavailable"
		e.mu.Unlock()
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
		current, readErr := inventory()
		if readErr != nil {
			e.setPartial(a.Name, "inventory refresh unavailable")
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
			e.setPartial(a.Name, "credential changed during tick")
			continue
		}
		accounts = current
		if probeErr == nil {
			if publishErr := e.publishSample(e.ctx, a, sample, nowMillis()); publishErr != nil {
				e.setPartial(a.Name+"/snapshot", "quota snapshot publication failed")
			}
		}
		e.mu.Lock()
		if probeErr != nil {
			e.observations[a.Name] = map[string]any{"error": "quota unavailable"}
			e.partialErrors[a.Name+"/probe"] = "quota unavailable"
		} else {
			e.observations[a.Name] = sample
		}
		e.mu.Unlock()
		for _, scope := range scopesFor(a) {
			if err := e.schedule(a, scope, sample, accounts, s, nowMillis()); err != nil {
				if err == errStopped || e.ctx.Err() != nil {
					return err
				}
				e.setPartial(a.Name+"/"+scope, err.Error())
			}
		}
	}
	if err := e.save(); err != nil {
		return err
	}
	e.mu.Lock()
	if len(e.partialErrors) > 0 {
		keys := make([]string, 0, len(e.partialErrors))
		for k := range e.partialErrors {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+": "+e.partialErrors[k])
		}
		e.lastError = strings.Join(parts, "; ")
	}
	e.mu.Unlock()
	return nil
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
