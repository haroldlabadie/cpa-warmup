package main

import (
	"errors"
	"strings"
)

// confirm is deliberately order-sensitive: retirement eligibility is computed
// from the current family window before any old-event query, so an unavailable
// historical event feed cannot freeze an old request forever. If the event
// query is available, a successful event still takes precedence over retirement.
func (e *Engine) confirm(a Account, r Record, sample *Sample, now int64) error {
	if num(r, "sent_ms") <= 0 || str(r, "attempt_token") == "" {
		return errors.New("historical attempt proof incomplete")
	}
	if id := str(r, "identity"); id != "" && id != a.identity() {
		return errors.New("historical credential identity mismatch")
	}
	if e.sealedAttempt(a.Name, r) {
		return errors.New("sealed-history-immutable")
	}
	if status, reason := e.boundIdentity(a.Name, a.identity()); status != "ok" {
		return errors.New("identity:" + reason)
	}
	scope := str(r, "scope")
	if scope == "" {
		scope = "all"
		if a.Provider == "antigravity" {
			scope = "gemini"
		}
		r["scope"] = scope
	}
	// Provider and family must agree with the record before trusting any
	// window. A sibling family shares the account but never its evidence.
	if !scopeAllowed(a.Provider, scope) {
		return errors.New("historical model family mismatch")
	}
	retire := laterCycleEligible(a, r, sample, now)
	decided, queryErr := e.queryAttempt(a, r, sample, now)
	if queryErr != nil {
		if retire {
			markRetired(r, sample, now)
			return nil
		}
		return queryErr
	}
	if decided {
		return nil
	}
	if retire {
		markRetired(r, sample, now)
	}
	return nil
}

// laterCycleEligible checks the pure time/freshness/scope evidence. It updates
// only the bounded stability candidate. It never claims request success.
func laterCycleEligible(a Account, r Record, sample *Sample, now int64) bool {
	sent := num(r, "sent_ms")
	scope := str(r, "scope")
	if sent <= 0 || sample == nil || !scopeAllowed(a.Provider, scope) || sample.Observed < sent || sample.Observed > now || now-sample.Observed > 90000 {
		return false
	}
	if !complete(sample, scope, now) || weeklyWindow(sample, scope) == nil {
		return false
	}
	w := warmWindow(sample, scope)
	if w == nil || w.Duration != fiveHour || w.EndMS <= now {
		return false
	}
	// The request's longest execution plus its old five-hour cycle must be over.
	if now < sent+fiveHour+45000 {
		return false
	}
	start := w.EndMS - w.Duration
	if start <= sent+45000 || start > now {
		return false
	}
	stable := stableBoundary(r, "recovery_boundary", w, sample, now)
	return w.used() > 0 || stable
}

func markRetired(r Record, sample *Sample, now int64) {
	w := warmWindow(sample, str(r, "scope"))
	if w == nil {
		return
	}
	r["status"] = "uncertainty_expired"
	r["retired_ms"] = now
	r["recovery_cycle_end_ms"] = w.EndMS
	r["recovery_cycle_start_ms"] = w.EndMS - w.Duration
	r["retirement"] = "later-cycle-without-success-event"
	r["last_reason"] = "old-request:later-cycle-observed"
}

func (e *Engine) queryAttempt(a Account, r Record, sample *Sample, now int64) (bool, error) {
	rows, err := e.api.attempt(e.ctx, a, r, now)
	if err != nil {
		return false, err
	}
	matched := []Event{}
	successful := []Event{}
	mismatched := false
	for _, v := range rows {
		if !eventMatches(a, r, v, hash(strings.TrimSpace(e.api.secrets.PrewarmKey)), now) {
			mismatched = true
			continue
		}
		matched = append(matched, v)
		if !v.Failed {
			successful = append(successful, v)
		}
	}
	// Only a matching success can prove the request outcome. A mismatched row
	// is retained as uncertainty when no such success exists, allowing an
	// independently established later cycle to retire the old request.
	if len(successful) > 0 {
		event := sortedSuccessful(successful)[len(successful)-1]
		scope := str(r, "scope")
		if scope == "" {
			scope = "all"
			if a.Provider == "antigravity" {
				scope = "gemini"
			}
		}
		w := warmWindow(sample, scope)
		stable := false
		if w != nil && sample != nil {
			stable = stableBoundary(r, "boundary_candidate", w, sample, now)
		}
		if w != nil && w.Duration == fiveHour && sample.Observed >= event.Timestamp && sample.Observed <= now && now-sample.Observed <= 90000 && (w.used() > 0 || stable) && w.EndMS-w.Duration <= event.Timestamp+120000 && event.Timestamp < w.EndMS && w.EndMS > now {
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
			return true, nil
		}
		// A proved success is recorded after the bounded wait for every
		// provider. It never claims a cycle or activation.
		if now-num(r, "sent_ms") >= 180000 {
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
			return true, nil
		}
	}
	// Without an exact successful match, any mismatched row makes the event
	// set uncertain. Do not retry or confirm from a partial match; returning
	// false lets confirm retire only on independently valid later-cycle proof.
	if mismatched {
		r["status"] = "unconfirmed"
		r["last_reason"] = "event-evidence-mismatch"
		return false, nil
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
			return true, nil
		}
		if safe {
			r["status"] = "retry"
			r["next_attempt_ms"] = now + 60000
			r["last_reason"] = "upstream-failed"
			return true, nil
		}
	}
	if len(matched) > 0 || now-num(r, "sent_ms") >= 180000 {
		r["status"] = "unconfirmed"
		r["last_reason"] = "missing-event-or-new-cycle"
	}
	return false, nil
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
