package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

type Event struct {
	Hash           string `json:"event_hash"`
	RequestID      string `json:"request_id"`
	Timestamp      int64  `json:"timestamp_ms"`
	Latency        *int64 `json:"latency_ms"`
	Model          string `json:"model"`
	RequestedModel string `json:"requested_model"`
	UA             string `json:"user_agent"`
	Index          string `json:"auth_index"`
	KeyHash        string `json:"api_key_hash"`
	File           string `json:"auth_file_snapshot"`
	Provider       string `json:"auth_provider_snapshot"`
	Reasoning      string `json:"reasoning_effort"`
	Failed         bool   `json:"failed"`
	Status         *int   `json:"fail_status_code"`
}

func (e Event) completion() int64 {
	if e.Latency != nil && *e.Latency > 0 {
		return e.Timestamp + *e.Latency
	}
	return e.Timestamp
}

type eventsResponse struct {
	Events *struct {
		Items    []Event `json:"items"`
		BeforeMS int64   `json:"next_before_ms"`
		BeforeID int64   `json:"next_before_id"`
		More     bool    `json:"has_more"`
		Total    *int64  `json:"total_count"`
	} `json:"events"`
	Coverage *struct {
		Complete    bool     `json:"raw_complete"`
		Deleted     int64    `json:"raw_deleted_event_count"`
		Limitations []string `json:"fidelity_limitations"`
	} `json:"coverage"`
}

// All pages share fixed bounds. A late insert changing the count invalidates the
// entire result; never decide idleness from a truncated or changing history.
func (a *API) events(ctx context.Context, from, to int64, filters map[string]any, search string) ([]Event, error) {
	if from < 1 {
		from = 1
	}
	if to <= from {
		return []Event{}, nil
	}
	beforeMS, beforeID := int64(0), int64(0)
	total := int64(-1)
	seen := map[string]bool{}
	out := []Event{}
	bytesRead := 0
	for page := 0; page < 1024; page++ {
		p := map[string]any{"limit": 1000}
		if beforeMS != 0 {
			p["before_ms"] = beforeMS
			p["before_id"] = beforeID
		}
		req := map[string]any{"from_ms": from, "to_ms": to, "filters": filters, "include": map[string]any{"events_page": p}}
		if search != "" {
			req["search_query"] = search
		}
		var response eventsResponse
		if e := a.call(ctx, "/v0/management/monitoring/analytics", req, &response); e != nil {
			return nil, e
		}
		serialized, _ := json.Marshal(response)
		bytesRead += len(serialized)
		if bytesRead > 64<<20 {
			return nil, errors.New("history byte capacity exceeded")
		}
		if response.Coverage != nil && (!response.Coverage.Complete || response.Coverage.Deleted > 0 || len(response.Coverage.Limitations) > 0) {
			return nil, errors.New("history coverage incomplete")
		}
		r := response.Events
		if r == nil || r.Total == nil {
			return nil, errors.New("events page incomplete")
		}
		if total < 0 {
			total = *r.Total
		} else if total != *r.Total {
			return nil, errors.New("history changed while paging")
		}
		if total < 0 || total > 100000 {
			return nil, errors.New("history capacity exceeded")
		}
		for _, e := range r.Items {
			if e.Hash == "" || e.Timestamp < from || e.Timestamp >= to {
				return nil, errors.New("invalid persistent event")
			}
			key := e.Hash
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, e)
		}
		if !r.More {
			if int64(len(out)) != total {
				return nil, errors.New("history count mismatch")
			}
			return out, nil
		}
		if len(r.Items) == 0 || r.BeforeMS <= 0 || r.BeforeID <= 0 || beforeMS > 0 && (r.BeforeMS > beforeMS || r.BeforeMS == beforeMS && r.BeforeID >= beforeID) {
			return nil, errors.New("non progressing history cursor")
		}
		beforeMS, beforeID = r.BeforeMS, r.BeforeID
	}
	return nil, errors.New("history pagination limit")
}
func modelScope(model string) string {
	m := strings.ToLower(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.Index(m, "("); i >= 0 {
		m = m[:i]
	}
	if strings.HasPrefix(m, "gemini-") {
		return "gemini"
	}
	if strings.HasPrefix(m, "claude-") || strings.HasPrefix(m, "gpt-") {
		return "third_party"
	}
	return ""
}
func (a *API) latest(ctx context.Context, acc Account, scope string, now int64) (int64, error) {
	split := now - fiveHour - 45000 - 3600000
	if split < 1 {
		split = 1
	}
	f := map[string]any{"include_failed": true, "auth_files": []string{acc.Name}, "auth_indices": []string{acc.Index}, "providers": []string{acc.Provider}}
	recent, e := a.events(ctx, split, now, f, "")
	if e != nil {
		return 0, e
	}
	oldFilter := map[string]any{}
	for k, v := range f {
		oldFilter[k] = v
	}
	oldFilter["min_latency_ms"] = 3600000
	historical, e := a.events(ctx, 1, split, oldFilter, "")
	if e != nil {
		return 0, e
	}
	latest := int64(0)
	for _, v := range append(recent, historical...) {
		if v.Failed {
			continue
		}
		if v.File != acc.Name || v.Index != acc.Index || v.Provider != acc.Provider {
			return 0, errors.New("history identity mismatch")
		}
		if v.RequestID == "" || v.Model == "" && v.RequestedModel == "" {
			return 0, errors.New("history fields missing")
		}
		if acc.Provider == "codex" || modelScope(v.RequestedModel) == scope || modelScope(v.Model) == scope {
			if end := v.completion(); end > now+1000 {
				return 0, errors.New("history completion in future")
			} else if end > latest {
				latest = end
			}
		}
	}
	return latest, nil
}
func (a *API) attempt(ctx context.Context, acc Account, rec Record, now int64) ([]Event, error) {
	token := str(rec, "attempt_token")
	if token == "" {
		return nil, errors.New("attempt UUID missing")
	}
	// '%' forces CPAMP v1.14.7 off its projection path, which omits UA.
	// Exact local checks remain mandatory. No search_api_key_hash OR filter.
	f := map[string]any{"include_failed": true, "api_key_hashes": []string{hash(strings.TrimSpace(a.secrets.PrewarmKey))}, "auth_files": []string{acc.Name}}
	rows, e := a.events(ctx, num(rec, "sent_ms")-1000, now, f, token+"%")
	if e != nil {
		return nil, e
	}
	for _, r := range rows {
		if r.UA == "" {
			return nil, errors.New("raw event UA missing")
		}
	}
	return rows, nil
}
func eventMatches(a Account, r Record, e Event, keyhash string, now int64) bool {
	if e.Provider != a.Provider || e.KeyHash != keyhash || e.UA != "cpa-quota-guard/2 "+str(r, "attempt_token") || e.File != a.Name || e.Index != a.Index || e.RequestedModel != str(r, "requested_model") || e.Reasoning != "low" || e.RequestID == "" || e.Timestamp < num(r, "sent_ms")-1000 || e.Timestamp > now+1000 {
		return false
	}
	if id := str(r, "request_id"); id != "" && id != e.RequestID {
		return false
	}
	if trace := str(r, "trace"); trace != "" {
		p := strings.SplitN(trace, "-", 3)
		if len(p) != 3 || p[1] != e.Index || p[2] != e.RequestID {
			return false
		}
	}
	return true
}
