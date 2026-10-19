package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadinessRequiresMatchingActiveHostInstance(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   map[string]any
		want   bool
	}{
		{"matching", 200, map[string]any{"instance_nonce": "instance", "worker_alive": true, "enabled": true, "observe": false, "quiesced": false}, true},
		{"old-nonce", 200, map[string]any{"instance_nonce": "old-instance", "worker_alive": true, "enabled": true}, false},
		{"missing-fields", 200, map[string]any{}, false},
		{"missing-nonce", 200, map[string]any{"worker_alive": true, "enabled": true}, false},
		{"inactive", 200, map[string]any{"instance_nonce": "instance", "worker_alive": false, "enabled": true}, false},
		{"disabled", 200, map[string]any{"instance_nonce": "instance", "worker_alive": true, "enabled": false}, false},
		{"observe", 200, map[string]any{"instance_nonce": "instance", "worker_alive": true, "enabled": true, "observe": true}, false},
		{"quiesced", 200, map[string]any{"instance_nonce": "instance", "worker_alive": true, "enabled": true, "quiesced": true}, false},
		{"not-registered", 404, map[string]any{}, false},
		{"unavailable", 503, map[string]any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "GET" || r.URL.Path != managementBase+"/status" {
					t.Errorf("readiness caused non-status action %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer management-test" {
					t.Error("status handshake missing management auth")
				}
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(tc.body)
			}))
			defer server.Close()
			e := &Engine{instance: "instance", api: newAPI(Bootstrap{CPAMPURL: server.URL}, Secrets{ManagementKey: "management-test"})}
			if got := e.ready(context.Background()); got != tc.want {
				t.Fatalf("readiness=%v want=%v", got, tc.want)
			}
			if calls != 1 {
				t.Fatalf("handshake calls %d", calls)
			}
		})
	}
}

func TestReadinessEmptyInstanceOrCancellationNeverCallsHost(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{}`)) }))
	defer server.Close()
	e := &Engine{api: newAPI(Bootstrap{CPAMPURL: server.URL}, Secrets{})}
	if e.ready(context.Background()) {
		t.Fatal("empty instance accepted")
	}
	e.instance = "instance"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e.ready(ctx) {
		t.Fatal("canceled context accepted")
	}
	if calls != 0 {
		t.Fatalf("paused handshake made %d host calls", calls)
	}
}
