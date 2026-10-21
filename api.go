package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

const fiveHour int64 = 18000000
const week int64 = 604800000

type Account struct {
	Source      string   `json:"source"`
	RuntimeOnly bool     `json:"runtime_only"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Provider    string   `json:"provider"`
	Index       string   `json:"auth_index"`
	Email       string   `json:"email"`
	AccountID   string   `json:"account_id"`
	ProjectID   string   `json:"project_id"`
	Prefix      string   `json:"prefix"`
	Disabled    bool     `json:"disabled"`
	Excluded    []string `json:"excluded_models"`
}

func (a Account) identity() string {
	b, _ := json.Marshal([]string{a.Name, a.Provider, a.Index, a.Email, a.AccountID, a.ProjectID})
	return hash(string(b))
}

var hostCall func(string, any, any) error

func inventory() ([]Account, error) {
	if hostCall == nil {
		return nil, errors.New("host unavailable")
	}
	var listed struct {
		Files []Account `json:"files"`
	}
	if e := hostCall("host.auth.list", map[string]any{}, &listed); e != nil {
		return nil, e
	}
	out := []Account{}
	seenNames := map[string]bool{}
	seenIDs := map[string]bool{}
	seenIndices := map[string]bool{}
	for _, a := range listed.Files {
		if a.Source != "file" || a.RuntimeOnly {
			continue
		}
		if a.Provider != "codex" && a.Provider != "antigravity" {
			continue
		}
		if a.Name == "" || path.Base(a.Name) != a.Name || a.Index == "" || a.ID == "" {
			return nil, errors.New("incomplete inventory identity")
		}
		if seenNames[a.Name] || seenIDs[a.ID] || seenIndices[a.Index] {
			return nil, errors.New("duplicate inventory identity")
		}
		seenNames[a.Name] = true
		seenIDs[a.ID] = true
		seenIndices[a.Index] = true
		var file struct {
			JSON json.RawMessage `json:"json"`
		}
		if e := hostCall("host.auth.get", map[string]string{"auth_index": a.Index}, &file); e != nil {
			return nil, e
		}
		var raw struct {
			Type         string   `json:"type"`
			Email        string   `json:"email"`
			AccountID    string   `json:"account_id"`
			ProjectID    string   `json:"project_id"`
			Prefix       string   `json:"prefix"`
			Disabled     bool     `json:"disabled"`
			Excluded     []string `json:"excluded_models"`
			ExcludedDash []string `json:"excluded-models"`
		}
		if e := json.Unmarshal(file.JSON, &raw); e != nil {
			return nil, errors.New("invalid auth metadata")
		}
		if raw.Type != a.Provider || !strings.EqualFold(strings.TrimSpace(raw.Email), strings.TrimSpace(a.Email)) {
			return nil, errors.New("credential file changed during inventory read")
		}
		a.AccountID = raw.AccountID
		a.ProjectID = raw.ProjectID
		a.Prefix = raw.Prefix
		a.Disabled = a.Disabled || raw.Disabled
		a.Excluded = raw.Excluded
		if a.Excluded == nil {
			a.Excluded = raw.ExcludedDash
		}
		out = append(out, a)
	}
	return out, nil
}

type Window struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	Scope     string  `json:"scope"`
	Remaining float64 `json:"remaining"`
	EndMS     int64   `json:"end_ms"`
	Duration  int64   `json:"duration_ms"`
}

func (w Window) used() float64 { return math.Round((100-w.Remaining)*1e6) / 1e6 }

type Sample struct {
	Observed int64    `json:"observed_ms"`
	Windows  []Window `json:"windows"`
}

func number(v any) (float64, error) {
	switch x := v.(type) {
	case float64:
		if !math.IsNaN(x) && !math.IsInf(x, 0) {
			return x, nil
		}
	case json.Number:
		f, e := x.Float64()
		if e == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, nil
		}
	case string:
		f, e := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if e == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, nil
		}
	}
	return 0, errors.New("non numeric quota")
}
func obj(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case Record:
		return map[string]any(m)
	}
	return nil
}
func text(v any) string { s, _ := v.(string); return s }
func parseCodex(p map[string]any) ([]Window, error) {
	primary := obj(p["rate_limit"])
	if primary == nil {
		return nil, errors.New("missing Codex rate_limit")
	}
	groups := []struct {
		label, scope string
		data         map[string]any
	}{{"codex", "all", primary}}
	if raw, ok := p["additional_rate_limits"]; ok && raw != nil {
		xs, ok := raw.([]any)
		if !ok {
			return nil, errors.New("invalid additional limits")
		}
		for _, v := range xs {
			m := obj(v)
			if m == nil {
				return nil, errors.New("invalid additional limit")
			}
			label := text(m["limit_name"])
			if label == "" {
				label = text(m["metered_feature"])
			}
			if label == "" {
				label = "additional"
			}
			d := obj(m["rate_limit"])
			if d == nil {
				d = m
			}
			groups = append(groups, struct {
				label, scope string
				data         map[string]any
			}{label, label, d})
		}
	}
	out := []Window{}
	for _, g := range groups {
		for _, role := range []string{"primary_window", "secondary_window"} {
			v := g.data[role]
			if v == nil {
				continue
			}
			m := obj(v)
			if m == nil {
				return nil, errors.New("invalid Codex window")
			}
			duration, e1 := number(m["limit_window_seconds"])
			used, e2 := number(m["used_percent"])
			end, e3 := number(m["reset_at"])
			if e1 != nil || e2 != nil || e3 != nil || duration <= 0 || duration > 315576000 || end <= 0 || end > 253402300799 || used < 0 || used > 100 {
				return nil, errors.New("invalid Codex quota")
			}
			kind := "other"
			if int64(duration)*1000 == fiveHour {
				kind = "five_hour"
			}
			if int64(duration)*1000 == week {
				kind = "weekly"
			}
			bid := role
			if kind == "five_hour" {
				bid = "five-hour"
			} else if kind == "weekly" {
				bid = "weekly"
			} else if int64(duration)*1000 == 2592000000 {
				bid = "monthly"
			}
			if g.label != "codex" {
				bid = g.label + ":" + bid
			}
			out = append(out, Window{bid, kind, g.scope, 100 - used, int64(end) * 1000, int64(duration) * 1000})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("empty Codex quota")
	}
	return out, nil
}
func parseAG(p map[string]any) ([]Window, error) {
	groups, ok := p["groups"].([]any)
	if !ok {
		return nil, errors.New("missing Antigravity groups")
	}
	out := []Window{}
	invalid := map[string]bool{}
	for _, v := range groups {
		g := obj(v)
		if g == nil {
			return nil, errors.New("invalid group")
		}
		label := strings.ToLower(text(g["displayName"]))
		if label == "" {
			label = strings.ToLower(text(g["display_name"]))
		}
		gs := ""
		if strings.Contains(label, "gemini") {
			gs = "gemini"
		} else if strings.Contains(label, "claude") || strings.Contains(label, "gpt") {
			gs = "third_party"
		}
		buckets, ok := g["buckets"].([]any)
		if !ok {
			if gs != "" {
				invalid[gs] = true
			}
			continue
		}
		for _, b := range buckets {
			m := obj(b)
			if m == nil {
				invalid[gs] = true
				continue
			}
			bid := text(m["bucketId"])
			if bid == "" {
				bid = text(m["bucket_id"])
			}
			scope := gs
			if strings.Contains(label+strings.ToLower(bid), "gemini") {
				scope = "gemini"
			} else if strings.HasPrefix(bid, "3p-") || strings.Contains(label, "claude") {
				scope = "third_party"
			}
			if scope == "" {
				continue
			}
			kind := ""
			switch strings.ToLower(text(m["window"])) {
			case "5h", "five_hour", "five-hour":
				kind = "five_hour"
			case "weekly", "week":
				kind = "weekly"
			}
			reset := text(m["resetTime"])
			if reset == "" {
				reset = text(m["reset_time"])
			}
			t, e := time.Parse(time.RFC3339Nano, reset)
			remaining, has := m["remainingFraction"]
			if !has {
				remaining = m["remaining_fraction"]
			}
			if remaining == nil {
				remaining = float64(0)
			}
			fraction, fe := number(remaining)
			if bid == "" || kind == "" || e != nil || fe != nil || fraction < 0 || fraction > 1 {
				invalid[scope] = true
				continue
			}
			duration := fiveHour
			if kind == "weekly" {
				duration = week
			}
			out = append(out, Window{groupSlug(label) + ":" + bid, kind, scope, fraction * 100, t.UnixMilli(), duration})
		}
	}
	filtered := []Window{}
	for _, scope := range []string{"gemini", "third_party"} {
		has5, hasW := false, false
		for _, w := range out {
			if w.Scope == scope {
				has5 = has5 || w.Kind == "five_hour"
				hasW = hasW || w.Kind == "weekly"
			}
		}
		if !invalid[scope] && has5 && hasW {
			for _, w := range out {
				if w.Scope == scope {
					filtered = append(filtered, w)
				}
			}
		}
	}
	if len(filtered) == 0 {
		return nil, errors.New("empty complete Antigravity family")
	}
	return filtered, nil
}

type API struct {
	cfg     Bootstrap
	secrets Secrets
	client  *http.Client
}

func newAPI(c Bootstrap, s Secrets) *API {
	return &API{c, s, &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 4}}}
}
func (a *API) call(ctx context.Context, p string, payload, out any) error {
	return a.callMethod(ctx, http.MethodPost, p, payload, out)
}
func (a *API) callMethod(ctx context.Context, method, p string, payload, out any) error {
	b, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.cfg.CPAMPURL, "/")+p, bytes.NewReader(b))
	if e != nil {
		return errors.New("request setup failed")
	}
	req.Header.Set("Authorization", "Bearer "+a.secrets.ManagementKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cpa-warmup/"+pluginVersion)
	response, e := a.client.Do(req)
	if e != nil {
		return errors.New("management transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("management HTTP %d", response.StatusCode)
	}
	raw, e := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if e != nil || len(raw) > 8<<20 {
		return errors.New("management response exceeded limit")
	}
	if e = decodePreserving(raw, out); e != nil {
		return errors.New("invalid management JSON")
	}
	return nil
}
func (a *API) upstream(ctx context.Context, acc Account, url, method string, payload any) (map[string]any, error) {
	headers := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json"}
	if acc.Provider == "codex" {
		if acc.AccountID == "" {
			return nil, errors.New("missing account_id")
		}
		headers["Chatgpt-Account-Id"] = acc.AccountID
		headers["User-Agent"] = "codex_cli_rs/0.116.0"
	} else {
		headers["User-Agent"] = "antigravity/cli/1.0.13 (aidev_client; os_type=darwin; arch=arm64)"
	}
	query := map[string]any{"auth_index": acc.Index, "method": method, "url": url, "header": headers}
	if payload != nil {
		b, _ := json.Marshal(payload)
		query["data"] = string(b)
	}
	var result struct {
		Status int    `json:"status_code"`
		Body   string `json:"body"`
	}
	if e := a.call(ctx, "/v0/management/api-call", query, &result); e != nil {
		return nil, e
	}
	if result.Status != 200 {
		return nil, fmt.Errorf("quota HTTP %d", result.Status)
	}
	var p map[string]any
	if decodePreserving([]byte(result.Body), &p) != nil {
		return nil, errors.New("quota JSON invalid")
	}
	return p, nil
}
func (a *API) probe(ctx context.Context, acc Account) (*Sample, error) {
	var windows []Window
	if acc.Provider == "codex" {
		p, e := a.upstream(ctx, acc, "https://chatgpt.com/backend-api/wham/usage", "GET", nil)
		if e != nil {
			return nil, e
		}
		var e2 error
		windows, e2 = parseCodex(p)
		if e2 != nil {
			return nil, e2
		}
	} else {
		if acc.ProjectID == "" {
			return nil, errors.New("missing project_id")
		}
		seen := map[string]bool{}
		for _, u := range []string{"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"} {
			p, e := a.upstream(ctx, acc, u, "POST", map[string]string{"project": acc.ProjectID})
			if e != nil {
				continue
			}
			ws, e := parseAG(p)
			if e != nil {
				continue
			}
			for _, scope := range []string{"gemini", "third_party"} {
				if seen[scope] {
					continue
				}
				found := false
				for _, w := range ws {
					if w.Scope == scope {
						windows = append(windows, w)
						found = true
					}
				}
				if found {
					seen[scope] = true
				}
			}
			if len(seen) == 2 {
				break
			}
		}
		if len(windows) == 0 {
			return nil, errors.New("quota unavailable")
		}
	}
	return &Sample{time.Now().UnixMilli(), windows}, nil
}

const geminiWarmupPrompt = `Summarize the following passage in about 30 words:

The Domain Name System (DNS) is a hierarchical, decentralized naming system for computers and services connected to the Internet. It translates easily memorized domain names into numerical IP addresses needed for locating and identifying computer services and devices with underlying network protocols. By delegating the responsibility of assigning domain names and mapping those names to Internet resources, DNS provides a globally distributed, fault-tolerant directory service fundamental to modern web infrastructure.`

func (a *API) send(ctx context.Context, model, uuid string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	content := "Reply 1."
	maxTokens := 1
	if modelScope(model) == "gemini" {
		content = geminiWarmupPrompt
		maxTokens = 50
	}
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true, "messages": []map[string]string{{"role": "user", "content": content}}, "reasoning_effort": "low", "max_tokens": maxTokens})
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(a.cfg.CPAURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if e != nil {
		return "", errors.New("generation setup failed")
	}
	req.Header.Set("Authorization", "Bearer "+a.secrets.PrewarmKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cpa-quota-guard/2 "+uuid)
	req.Header.Set("Idempotency-Key", uuid)
	client := *a.client
	client.Timeout = 45 * time.Second
	resp, e := client.Do(req)
	if e != nil {
		return "", errors.New("generation transport uncertain")
	}
	defer resp.Body.Close()
	trace := resp.Header.Get("X-CPA-Trace-ID")
	if resp.StatusCode != 200 {
		return trace, fmt.Errorf("generation HTTP %d; awaiting persistent event", resp.StatusCode)
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 1<<20+1))
	scanner.Buffer(make([]byte, 4096), 65536)
	size := 0
	for scanner.Scan() {
		line := scanner.Text()
		size += len(line) + 1
		if size > 1<<20 {
			return trace, errors.New("generation response exceeded limit")
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data != "[DONE]" && data != "" {
				var v any
				if json.Unmarshal([]byte(data), &v) != nil {
					return trace, errors.New("invalid SSE; awaiting persistent event")
				}
			}
		}
	}
	if scanner.Err() != nil {
		return trace, errors.New("stream uncertain")
	}
	return trace, nil
}

// modelCatalog reads the public CPA model list using the dedicated prewarm
// key. Only public IDs and owners are returned; the key never leaves the
// plugin process.
func modelCatalog(ctx context.Context, a *API) ([]map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(a.cfg.CPAURL, "/")+"/v1/models", nil)
	if err != nil {
		return nil, errors.New("catalog request setup failed")
	}
	req.Header.Set("Authorization", "Bearer "+a.secrets.PrewarmKey)
	req.Header.Set("User-Agent", "cpa-warmup/"+pluginVersion)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, errors.New("catalog transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("catalog HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("catalog response too large")
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil, errors.New("catalog JSON invalid")
	}
	seen := map[string]bool{}
	out := []map[string]string{}
	for _, item := range body.Data {
		slash := strings.LastIndex(item.ID, "/")
		name := item.ID
		if slash >= 0 {
			name = item.ID[slash+1:]
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, map[string]string{"name": name, "provider": item.OwnedBy})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out, nil
}

func groupSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	return b.String()
}
