package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

// Snapshot publication is a display side effect. Failure never replaces a
// healthy family sample, and observe mode never writes monitoring data.
func (e *Engine) publishSample(ctx context.Context, a Account, sample *Sample, now int64) error {
	if e.cfg.Observe || !e.cfg.Enabled || sample == nil || sample.Observed > now || now-sample.Observed > 90000 {
		return nil
	}
	windows := []map[string]any{}
	fingerprint := []any{}
	e.store.mu.Lock()
	latest := map[string]int64{}
	for _, w := range sample.Windows {
		latest[w.Scope] = e.store.state.LatestSuccess[a.identity()+":"+w.Scope]
	}
	e.store.mu.Unlock()
	for _, w := range sample.Windows {
		active := w.Kind != "five_hour"
		if !active {
			until, err := activeUntil(w, now, latest[w.Scope])
			active = err == nil && until > 0
		}
		scope := w.Scope
		if scope == "all" {
			scope = "codex_main"
		}
		if scope == "third_party" {
			scope = "claude_gpt"
		}
		row := map[string]any{"provider_window_id": w.ID, "window_kind": w.Kind, "window_mode": "fixed", "model_scope_kind": "family", "model_scope_key": scope, "source": "api_query", "source_observation_id": fmt.Sprintf("warmup-v1:%s:%d", a.Index, sample.Observed), "observed_at_ms": sample.Observed, "boundary_accuracy": "unknown", "cycle_end_ms": w.EndMS, "duration_seconds": w.Duration / 1000, "used_percent": w.used(), "remaining_percent": w.Remaining}
		var boundary any
		if active {
			row["cycle_start_ms"] = w.EndMS - w.Duration
			row["boundary_accuracy"] = "derived"
			boundary = w.EndMS
		}
		windows = append(windows, row)
		fingerprint = append(fingerprint, []any{w.ID, math.Round(w.Remaining*1000) / 1000, boundary})
	}
	raw, _ := json.Marshal(fingerprint)
	digest := hash(string(raw))
	e.mu.Lock()
	if e.snapshotTimes == nil {
		e.snapshotTimes = map[string]int64{}
		e.snapshotDigests = map[string]string{}
	}
	last, prior := e.snapshotTimes[a.Name], e.snapshotDigests[a.Name]
	e.mu.Unlock()
	if now-last < 300000 && (prior == digest || now-last < 20000) {
		return nil
	}
	target := map[string]string{"auth_file_snapshot": a.Name, "auth_provider_snapshot": a.Provider, "account_snapshot": a.Email, "auth_index": a.Index, "auth_account_id_snapshot": a.AccountID, "auth_project_id_snapshot": a.ProjectID}
	scope := "codex:rate-limits"
	if a.Provider == "antigravity" {
		scope = "antigravity:quota-windows"
	}
	payload := map[string]any{"entries": []any{map[string]any{"row_key": a.Name, "provider": a.Provider, "account": target, "observation": map[string]any{"source": "api_query", "source_observation_id": fmt.Sprintf("warmup-v1:%s:%d", a.Index, sample.Observed), "observed_at_ms": sample.Observed, "inventory_scope_key": scope, "inventory_mode": "partial"}, "windows": windows}}}
	var response map[string]any
	if err := e.api.call(ctx, "/v0/management/quota-snapshots", payload, &response); err != nil {
		return err
	}
	e.mu.Lock()
	e.snapshotTimes[a.Name] = now
	e.snapshotDigests[a.Name] = digest
	e.mu.Unlock()
	return nil
}
