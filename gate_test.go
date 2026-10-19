package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
)

func gateEngine(t *testing.T) (*Engine, requestInterceptRequest) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := testStore(t)
	e := &Engine{cfg: Bootstrap{Enabled: true}, settings: defaults(), store: s, ctx: ctx, cancel: cancel, api: newAPI(Bootstrap{}, Secrets{PrewarmKey: "prewarm"})}
	scope := hash("cli-proxy-api:caller-scope:v1\x00prewarm")
	s.state.AttemptGates["uuid"] = Gate{AuthID: "auth-id", AuthIndex: "index", Identity: "identity", CallerScope: scope, RequestedModel: "prefix/model", SentMS: 1000}
	globalMu.Lock()
	old := active
	active = e
	globalMu.Unlock()
	t.Cleanup(func() { globalMu.Lock(); active = old; globalMu.Unlock() })
	body, _ := json.Marshal(map[string]any{"stream": true, "reasoning_effort": "low"})
	req := requestInterceptRequest{RequestID: "request", TraceID: "codex-index-request", RequestedModel: "prefix/model", Model: "model", Stream: true, Body: body, Headers: http.Header{"User-Agent": []string{"cpa-quota-guard/2 uuid"}}, Metadata: map[string]any{"caller_scope": scope, "selected_auth_id": "auth-id", "selected_auth_index": "index"}}
	return e, req
}
func callGate(t *testing.T, method string, req requestInterceptRequest) requestInterceptResponse {
	t.Helper()
	raw, _ := json.Marshal(req)
	b, err := intercept(method, raw)
	if err != nil {
		t.Fatalf("host fail-open risk: gate returned error: %v", err)
	}
	var env envelope
	if err = json.Unmarshal(b, &env); err != nil || !env.OK {
		t.Fatalf("gate did not return normal envelope: %s %v", b, err)
	}
	var out requestInterceptResponse
	if err = json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestGateBindsBeforeAndConfirmsActualSelectedIdentity(t *testing.T) {
	e, req := gateEngine(t)
	if out := callGate(t, methodRequestInterceptBefore, req); out.Terminate {
		t.Fatal("valid before denied")
	}
	g := e.store.state.AttemptGates["uuid"]
	if g.RequestID != req.RequestID || g.TraceID != req.TraceID || g.AfterAuth {
		t.Fatalf("before failed binding %#v", g)
	}
	if out := callGate(t, methodRequestInterceptAfter, req); out.Terminate {
		t.Fatal("valid actual identity denied")
	}
	if !e.store.state.AttemptGates["uuid"].AfterAuth {
		t.Fatal("selected identity was not persisted")
	}
}
func TestGateRejectsForgedOrIncompleteAttemptsWithoutError(t *testing.T) {
	cases := []struct {
		name   string
		method string
		mutate func(*requestInterceptRequest)
	}{
		{"wrong-caller-scope", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.Metadata["caller_scope"] = "forged" }},
		{"missing-caller-scope", methodRequestInterceptBefore, func(r *requestInterceptRequest) { delete(r.Metadata, "caller_scope") }},
		{"unknown-uuid", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.Headers.Set("User-Agent", "cpa-quota-guard/2 unknown") }},
		{"missing-ua", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.Headers.Del("User-Agent") }},
		{"missing-trace", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.TraceID = "" }},
		{"missing-request", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.RequestID = "" }},
		{"wrong-model", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.RequestedModel = "other/model" }},
		{"nonstream", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.Stream = false }},
		{"nonstream-body", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.Body = []byte(`{"stream":false,"reasoning_effort":"low"}`) }},
		{"high-reasoning", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.Body = []byte(`{"stream":true,"reasoning_effort":"high"}`) }},
		{"bad-body", methodRequestInterceptBefore, func(r *requestInterceptRequest) { r.Body = []byte(`{`) }},
		{"wrong-execution-model", methodRequestInterceptAfter, func(r *requestInterceptRequest) { r.Model = "other-model" }},
		{"wrong-selected-id", methodRequestInterceptAfter, func(r *requestInterceptRequest) { r.Metadata["selected_auth_id"] = "other" }},
		{"wrong-selected-index", methodRequestInterceptAfter, func(r *requestInterceptRequest) { r.Metadata["selected_auth_index"] = "other" }},
		{"missing-selected-id", methodRequestInterceptAfter, func(r *requestInterceptRequest) { delete(r.Metadata, "selected_auth_id") }},
		{"missing-selected-index", methodRequestInterceptAfter, func(r *requestInterceptRequest) { delete(r.Metadata, "selected_auth_index") }},
		{"changed-trace", methodRequestInterceptAfter, func(r *requestInterceptRequest) { r.TraceID = "other" }},
		{"changed-request", methodRequestInterceptAfter, func(r *requestInterceptRequest) { r.RequestID = "other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, req := gateEngine(t)
			if tc.method == methodRequestInterceptAfter {
				if callGate(t, methodRequestInterceptBefore, req).Terminate {
					t.Fatal("setup before denied")
				}
			}
			tc.mutate(&req)
			out := callGate(t, tc.method, req)
			if !out.Terminate || out.StatusCode != 403 {
				t.Fatalf("unsafe attempt allowed %#v", out)
			}
		})
	}
}
func TestGateRequiresBeforeBindingAndRejectsRebinding(t *testing.T) {
	_, req := gateEngine(t)
	if !callGate(t, methodRequestInterceptAfter, req).Terminate {
		t.Fatal("after allowed without before")
	}
	if callGate(t, methodRequestInterceptBefore, req).Terminate {
		t.Fatal("valid before denied")
	}
	req.RequestID = "another"
	if !callGate(t, methodRequestInterceptBefore, req).Terminate {
		t.Fatal("persisted attempt rebound")
	}
}
func TestGatePersistenceFailureTerminatesAndStopsWorker(t *testing.T) {
	e, req := gateEngine(t)
	e.store.path = filepath.Join(t.TempDir(), "does-not-exist", "state.json")
	out := callGate(t, methodRequestInterceptBefore, req)
	if !out.Terminate || !e.stopped || e.ctx.Err() == nil {
		t.Fatalf("persistence failure failed open %#v stopped=%v context=%v", out, e.stopped, e.ctx.Err())
	}
}
func TestGateDoesNotAffectOrdinaryRequests(t *testing.T) {
	_, req := gateEngine(t)
	req.Headers.Set("User-Agent", "ordinary-client")
	req.Metadata["caller_scope"] = "ordinary-key"
	if callGate(t, methodRequestInterceptBefore, req).Terminate {
		t.Fatal("ordinary caller denied")
	}
}

func TestGatePanicReturnsNormalTerminationAndStopsWorker(t *testing.T) {
	e, req := gateEngine(t)
	// An unavailable API reference simulates a callback-internal panic. Returning
	// a host error here would fail open; the callback must emit a normal denial.
	e.api = nil
	out := callGate(t, methodRequestInterceptBefore, req)
	if !out.Terminate || out.StatusCode != 403 || !e.stopped || e.ctx.Err() == nil {
		t.Fatalf("panic failed open: response=%#v stopped=%v context=%v", out, e.stopped, e.ctx.Err())
	}
}
