package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const managementBase = "/v0/management/plugins/cpa-warmup"

//go:embed web/settings.html
var page []byte

//go:embed web/settings.js
var pageScript []byte

func nowMS() int64 { return time.Now().UnixMilli() }
func management(raw []byte) ([]byte, error) {
	var req managementRequest
	if json.Unmarshal(raw, &req) != nil {
		return managementJSON(400, map[string]string{"error": "invalid management request"}, nil)
	}
	p := strings.TrimRight(req.Path, "/")
	if req.Method == "GET" && p == "/v0/resource/plugins/cpa-warmup/settings" {
		return okEnvelope(managementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": []string{"text/html; charset=utf-8"}, "Content-Security-Policy": []string{"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; frame-ancestors 'self'"}}, Body: []byte(strings.Replace(string(page), "/*SETTINGS_JS*/", string(pageScript), 1))})
	}
	e := current()
	if e == nil {
		return managementJSON(503, map[string]string{"error": "not configured"}, nil)
	}
	if req.Method == "GET" && p == managementBase+"/status" {
		return managementJSON(200, statusSnapshot(e), nil)
	}
	if req.Method == "GET" && p == managementBase+"/settings" {
		e.mu.Lock()
		s, r := e.settings, e.revision
		e.mu.Unlock()
		return managementJSON(200, map[string]any{"settings": s, "revision": r}, http.Header{"ETag": []string{revisionETag(r)}})
	}
	if req.Method == "PUT" && p == managementBase+"/settings" {
		if len(req.Body) > 65536 {
			return managementJSON(413, map[string]string{"error": "settings too large"}, nil)
		}
		var s Settings
		if strict(req.Body, &s) != nil || s.validate() != nil {
			return managementJSON(422, map[string]string{"error": "invalid or unknown settings"}, nil)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if req.Headers.Get("If-Match") == "" {
			return managementJSON(428, map[string]string{"error": "If-Match required"}, nil)
		}
		if req.Headers.Get("If-Match") != revisionETag(e.revision) {
			return managementJSON(412, map[string]string{"error": "settings changed; reload"}, nil)
		}
		if err := atomicJSON(e.cfg.SettingsFile, s); err != nil {
			return managementJSON(500, map[string]string{"error": "settings persistence failed"}, nil)
		}
		e.settings = s
		e.revision = settingsDigest(s)
		return managementJSON(200, map[string]any{"settings": s, "revision": e.revision}, http.Header{"ETag": []string{revisionETag(e.revision)}})
	}
	return managementJSON(404, map[string]string{"error": "route not found"}, nil)
}
func managementJSON(code int, v any, headers http.Header) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	return okEnvelope(managementResponse{StatusCode: code, Headers: headers, Body: b})
}
