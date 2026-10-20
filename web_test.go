package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callManagement(t *testing.T, req managementRequest) managementResponse {
	t.Helper()
	raw, _ := json.Marshal(req)
	b, err := management(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err = json.Unmarshal(b, &env); err != nil || !env.OK {
		t.Fatalf("invalid envelope %s %v", b, err)
	}
	var out managementResponse
	if err = json.Unmarshal(env.Result, &out); err != nil {
		t.Fatal(err)
	}
	normalized := http.Header{}
	for k, vs := range out.Headers {
		for _, v := range vs {
			normalized.Add(k, v)
		}
	}
	out.Headers = normalized
	return out
}
func TestSettingsContentDigestSurvivesEngineReloadAndRejectsStaleWrites(t *testing.T) {
	e, _ := gateEngine(t)
	e.cfg.SettingsFile = filepath.Join(t.TempDir(), "settings.json")
	e.revision = settingsDigest(e.settings)
	original := callManagement(t, managementRequest{Method: "GET", Path: managementBase + "/settings"})
	etag := original.Headers.Get("ETag")
	if original.StatusCode != 200 || etag == "" || etag == `"1"` {
		t.Fatalf("noncontent revision %#v", original)
	}
	next := e.settings
	next.Enabled = false
	payload, _ := json.Marshal(next)
	if got := callManagement(t, managementRequest{Method: "PUT", Path: managementBase + "/settings", Body: payload}); got.StatusCode != 428 {
		t.Fatalf("missing If-Match status %d", got.StatusCode)
	}
	if got := callManagement(t, managementRequest{Method: "PUT", Path: managementBase + "/settings", Body: payload, Headers: http.Header{"If-Match": []string{`"stale"`}}}); got.StatusCode != 412 {
		t.Fatalf("stale If-Match status %d", got.StatusCode)
	}
	updated := callManagement(t, managementRequest{Method: "PUT", Path: managementBase + "/settings", Body: payload, Headers: http.Header{"If-Match": []string{etag}}})
	if updated.StatusCode != 200 || updated.Headers.Get("ETag") == etag {
		t.Fatalf("successful update failed to change digest: %#v", updated)
	}
	saved, err := os.ReadFile(e.cfg.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Settings
	if err = strict(saved, &loaded); err != nil {
		t.Fatal(err)
	}
	reloaded := &Engine{cfg: e.cfg, settings: loaded, revision: settingsDigest(loaded), store: e.store, api: e.api, ctx: e.ctx, cancel: e.cancel}
	globalMu.Lock()
	active = reloaded
	globalMu.Unlock()
	afterReload := callManagement(t, managementRequest{Method: "GET", Path: managementBase + "/settings"})
	if afterReload.Headers.Get("ETag") != updated.Headers.Get("ETag") {
		t.Fatalf("same persisted content changed ETag %s vs %s", afterReload.Headers.Get("ETag"), updated.Headers.Get("ETag"))
	}
	if got := callManagement(t, managementRequest{Method: "PUT", Path: managementBase + "/settings", Body: payload, Headers: http.Header{"If-Match": []string{etag}}}); got.StatusCode != 412 {
		t.Fatalf("pre-reload stale ETag accepted: %d", got.StatusCode)
	}
}
func TestSettingsRejectUnknownFieldsAndOversizeWithoutMutation(t *testing.T) {
	e, _ := gateEngine(t)
	e.cfg.SettingsFile = filepath.Join(t.TempDir(), "settings.json")
	e.revision = settingsDigest(e.settings)
	payload, _ := json.Marshal(e.settings)
	var unknown map[string]any
	json.Unmarshal(payload, &unknown)
	unknown["secret_override"] = "do not accept"
	body, _ := json.Marshal(unknown)
	headers := http.Header{"If-Match": []string{revisionETag(e.revision)}}
	if got := callManagement(t, managementRequest{Method: "PUT", Path: managementBase + "/settings", Body: body, Headers: headers}); got.StatusCode != 422 {
		t.Fatalf("unknown field accepted %d", got.StatusCode)
	}
	if got := callManagement(t, managementRequest{Method: "PUT", Path: managementBase + "/settings", Body: make([]byte, 65537), Headers: headers}); got.StatusCode != 413 {
		t.Fatalf("oversize accepted %d", got.StatusCode)
	}
	if _, err := os.Stat(e.cfg.SettingsFile); !os.IsNotExist(err) {
		t.Fatal("rejected settings mutated persisted file")
	}
}
func TestPublicSettingsShellContainsNoRuntimeStateOrSecrets(t *testing.T) {
	e, _ := gateEngine(t)
	e.api.secrets.ManagementKey = "secret-management-unique"
	e.api.secrets.PrewarmKey = "secret-prewarm-unique"
	e.lastError = "secret-runtime-error-unique"
	e.store.state.Slots["secret-slot-unique"] = map[string]Record{"private-account-unique": {"status": "unconfirmed"}}
	out := callManagement(t, managementRequest{Method: "GET", Path: "/v0/resource/plugins/cpa-warmup/settings"})
	if out.StatusCode != 200 || !strings.HasPrefix(out.Headers.Get("Content-Type"), "text/html") || out.Headers.Get("Content-Security-Policy") == "" {
		t.Fatalf("public shell missing headers %#v", out)
	}
	html := string(out.Body)
	for _, secret := range []string{e.api.secrets.ManagementKey, e.api.secrets.PrewarmKey, e.lastError, "secret-slot-unique", "private-account-unique"} {
		if strings.Contains(html, secret) {
			t.Fatalf("public static resource leaked runtime %q", secret)
		}
	}
	js := string(pageScript)
	if strings.Contains(js, "innerHTML") || !strings.Contains(js, "cell.textContent=String(value)") || !strings.Contains(js, "option.textContent=name") {
		t.Fatal("dynamic status/account values must use textContent")
	}
	if !strings.Contains(js, "cpa_warmup_mgmt_key") || !strings.Contains(js, "localStorage.setItem") {
		t.Fatal("manual management key must persist in browser storage")
	}
	if strings.Contains(js, "innerHTML") {
		t.Fatal("unsafe HTML rendering")
	}
}
