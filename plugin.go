package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
)

var globalMu sync.RWMutex
var lifecycleMu sync.Mutex
var active *Engine

func current() *Engine { globalMu.RLock(); defer globalMu.RUnlock(); return active }
func quiesce()         { lifecycleMu.Lock(); defer lifecycleMu.Unlock(); stopCurrent() }
func stopCurrent() {
	e := current()
	if e != nil {
		e.mu.Lock()
		already := e.stopped
		e.mu.Unlock()
		if !already {
			e.stop()
		} else {
			e.cancel()
			<-e.done
			e.store.close()
		}
	}
}
func stopOnPanic() {
	if e := current(); e != nil {
		e.fail("callback panic; automatic actions stopped")
	}
}
func configure(raw []byte) error {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	var req lifecycleRequest
	if json.Unmarshal(raw, &req) != nil || req.SchemaVersion < 6 {
		return errors.New("CPA RPC schema 6 required")
	}
	cfg, err := decodeBootstrap(req.ConfigYAML)
	if err != nil {
		return err
	}
	secretInfo, err := os.Lstat(cfg.SecretFile)
	if err != nil || !secretInfo.Mode().IsRegular() || secretInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("secret file must be a regular non-symlink file")
	}
	if secretInfo.Size() > 65536 {
		return errors.New("secret file exceeds size limit")
	}
	b, err := readLimited(cfg.SecretFile, 65536)
	if err != nil {
		return errors.New("secret file unavailable")
	}
	info, err := os.Stat(cfg.SecretFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("secret file requires mode 0600")
	}
	var secrets Secrets
	if strict(b, &secrets) != nil || strings.TrimSpace(secrets.ManagementKey) == "" || strings.TrimSpace(secrets.PrewarmKey) == "" {
		return errors.New("invalid secret file")
	}
	secrets.ManagementKey = strings.TrimSpace(secrets.ManagementKey)
	secrets.PrewarmKey = strings.TrimSpace(secrets.PrewarmKey)
	settings := defaults()
	b, err = readLimited(cfg.SettingsFile, 65536)
	if err == nil {
		if strict(b, &settings) != nil {
			return errors.New("settings decode failed")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("settings unavailable")
	}
	if err = settings.validate(); err != nil {
		return err
	}
	stopCurrent()
	store, err := openStore(cfg.DataDir)
	if err != nil {
		return err
	}
	if os.IsNotExist(err) {
		return err
	}
	if _, err = os.Stat(cfg.SettingsFile); os.IsNotExist(err) {
		if err = atomicJSON(cfg.SettingsFile, settings); err != nil {
			store.close()
			return err
		}
	}
	instance, nonceErr := uuid()
	if nonceErr != nil {
		store.close()
		return nonceErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{cfg: cfg, settings: settings, instance: instance, revision: settingsDigest(settings), store: store, api: newAPI(cfg, secrets), ctx: ctx, cancel: cancel, done: make(chan struct{}), observations: map[string]any{}}
	globalMu.Lock()
	active = e
	globalMu.Unlock()
	go e.loop()
	return nil
}
func handleMethod(method string, raw []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		if err := configure(raw); err != nil {
			return nil, err
		}
		return okEnvelope(registration{SchemaVersion: 6, Metadata: metadata{Name: "定向预热", Version: pluginVersion, Author: "Harold Labadie", GitHubRepository: "https://github.com/haroldlabadie/cpa-warmup", ConfigFields: []configField{{Name: "data_dir", Type: "string", Description: "Private persistent state directory"}, {Name: "secret_file", Type: "string", Description: "Private JSON management/prewarm keys file"}, {Name: "cpa_url", Type: "string", Description: "Loopback CPA HTTP URL"}, {Name: "cpamp_url", Type: "string", Description: "Private CPAMP HTTP URL"}, {Name: "observe", Type: "boolean", Description: "Probe and reconcile without generation"}}}, Capabilities: capabilities{RequestInterceptor: true, ManagementAPI: true}})
	case "plugin.quiesce":
		quiesce()
		return okEnvelope(map[string]bool{"quiesced": true})
	case methodRequestInterceptBefore, methodRequestInterceptAfter:
		return intercept(method, raw)
	case methodManagementRegister:
		return okEnvelope(managementRegistrationResponse{Routes: []managementRoute{{Method: "GET", Path: managementBase + "/status"}, {Method: "GET", Path: managementBase + "/settings"}, {Method: "PUT", Path: managementBase + "/settings"}}, Resources: []resourceRoute{{Path: "/settings", Menu: "定向预热", Description: "只读进度与预热设置"}}})
	case methodManagementHandle:
		return management(raw)
	default:
		return errorEnvelope("unknown_method", "unsupported method", 0), nil
	}
}
func denial(reason string) requestInterceptResponse {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "warmup_identity_denied", "message": reason}})
	return requestInterceptResponse{Terminate: true, StatusCode: 403, ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}}, ResponseBody: body}
}
func intercept(method string, raw []byte) (out []byte, err error) {
	defer func() {
		if recover() != nil {
			stopOnPanic()
			out, _ = okEnvelope(denial("warmup gate unavailable"))
			err = nil
		}
	}()
	var req requestInterceptRequest
	if json.Unmarshal(raw, &req) != nil {
		return okEnvelope(denial("malformed interceptor request"))
	}
	e := current()
	if e == nil {
		return okEnvelope(requestInterceptResponse{})
	}
	scope, _ := req.Metadata["caller_scope"].(string)
	expected := hash("cli-proxy-api:caller-scope:v1\x00" + e.api.secrets.PrewarmKey)
	ua := req.Headers.Get("User-Agent")
	token := strings.TrimPrefix(ua, "cpa-quota-guard/2 ")
	isWarmUA := strings.HasPrefix(ua, "cpa-quota-guard/2 ")
	// Reserve this dedicated key for persisted worker attempts. A different key
	// spoofing our UA cannot alter journals or borrow any policy privilege.
	if scope != expected {
		if isWarmUA {
			return okEnvelope(denial("warmup caller identity mismatch"))
		}
		return okEnvelope(requestInterceptResponse{})
	}
	e.mu.Lock()
	stopped := e.stopped || !e.cfg.Enabled || e.cfg.Observe || !e.settings.Enabled
	e.mu.Unlock()
	if stopped || e.ctx.Err() != nil {
		return okEnvelope(denial("warmup worker paused"))
	}
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	gate, ok := e.store.state.AttemptGates[token]
	if !isWarmUA || !ok || gate.CallerScope != scope || req.TraceID == "" || req.RequestID == "" || !req.Stream || gate.RequestedModel != req.RequestedModel {
		return okEnvelope(denial("unregistered warmup attempt"))
	}
	var body map[string]any
	if json.Unmarshal(req.Body, &body) != nil || text(body["reasoning_effort"]) != "low" || body["stream"] != true {
		return okEnvelope(denial("warmup requires stream and low reasoning"))
	}
	if method == methodRequestInterceptBefore {
		if gate.TraceID != "" && (gate.TraceID != req.TraceID || gate.RequestID != req.RequestID) {
			return okEnvelope(denial("attempt already bound to another request"))
		}
		gate.TraceID = req.TraceID
		gate.RequestID = req.RequestID
	} else {
		id, _ := req.Metadata["selected_auth_id"].(string)
		index, _ := req.Metadata["selected_auth_index"].(string)
		if gate.TraceID != req.TraceID || gate.RequestID != req.RequestID || id == "" || index == "" || id != gate.AuthID || index != gate.AuthIndex {
			return okEnvelope(denial("selected credential identity mismatch"))
		}
		expectedModel := gate.RequestedModel
		if slash := strings.LastIndex(expectedModel, "/"); slash >= 0 {
			expectedModel = expectedModel[slash+1:]
		}
		if req.Model != expectedModel && req.Model != gate.RequestedModel {
			return okEnvelope(denial("selected execution model mismatch"))
		}
		gate.AfterAuth = true
	}
	e.store.state.AttemptGates[token] = gate
	if e.store.saveLocked() != nil {
		e.fail("gate persistence failed; worker stopped")
		return okEnvelope(denial("warmup journal unavailable"))
	}
	return okEnvelope(requestInterceptResponse{})
}
func statusSnapshot(e *Engine) map[string]any {
	e.mu.Lock()
	result := map[string]any{"plugin": pluginID, "version": pluginVersion, "instance_nonce": e.instance, "enabled": e.cfg.Enabled, "observe": e.cfg.Observe, "worker_alive": e.running, "quiesced": e.stopped, "last_tick_ms": e.lastTick, "last_error": e.lastError, "revision": e.revision, "next_slot": nextSlot(nowMillis(), e.settings)}
	names := []string{}
	for name := range e.observations {
		names = append(names, name)
	}
	partial := map[string]string{}
	for k, v := range e.partialErrors {
		partial[k] = v
	}
	sort.Strings(names)
	e.mu.Unlock()
	result["accounts"] = names
	e.store.mu.Lock()
	slots := map[string]any{}
	for sid, records := range e.store.state.Slots {
		slots[sid] = records
	}
	b, _ := json.Marshal(slots)
	var copy any
	json.Unmarshal(b, &copy)
	result["slots"] = copy
	result["partial_errors"] = partial
	result["families"] = familySnapshot(e.store.state.Slots, e.store.state.IdentityGenerations)
	result["identity_generations"] = e.store.state.IdentityGenerations
	result["identity_recovery"] = e.store.state.IdentityRecovery
	result["active_attempt_count"] = len(e.store.state.AttemptGates)
	e.store.mu.Unlock()
	return result
}

// familySnapshot is a display-only view of the latest decisions for each
// account/family. It never mutates the journal or invents a successful state.
func familySnapshot(slots Slots, generations map[string]*Generation) map[string]any {
	out := map[string]any{}
	for _, records := range slots {
		for key, r := range records {
			name := accountName(key)
			scope := str(r, "scope")
			if scope == "" {
				scope = "all"
			}
			entry := obj(out[name])
			if entry == nil {
				entry = map[string]any{"unresolved": []any{}, "generation": nil}
			}
			if unresolved(r) {
				entry["unresolved"] = append(entry["unresolved"].([]any), map[string]any{"slot": str(r, "slot"), "scope": scope, "status": str(r, "status"), "reason": str(r, "last_reason")})
			}
			if g := generations[name]; g != nil {
				entry["generation"] = map[string]any{"id": g.ID, "number": g.Generation, "floor_ms": g.FloorMS, "sealed": len(g.Sealed)}
			}
			out[name] = entry
		}
	}
	return out
}
func revisionETag(r string) string     { return "\"" + r + "\"" }
func settingsDigest(s Settings) string { b, _ := json.Marshal(s); return hash(string(b)) }
