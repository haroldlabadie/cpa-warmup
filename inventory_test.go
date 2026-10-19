package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func inventoryHost(t *testing.T, files []map[string]any, metadata map[string]map[string]any) *int {
	t.Helper()
	old := hostCall
	calls := new(int)
	hostCall = func(method string, payload, out any) error {
		var value any
		switch method {
		case "host.auth.list":
			value = map[string]any{"files": files}
		case "host.auth.get":
			*calls++
			index := payload.(map[string]string)["auth_index"]
			raw, ok := metadata[index]
			if !ok {
				return fmt.Errorf("unexpected auth read %s", index)
			}
			b, _ := json.Marshal(raw)
			value = map[string]any{"json": json.RawMessage(b)}
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
		b, _ := json.Marshal(value)
		return json.Unmarshal(b, out)
	}
	t.Cleanup(func() { hostCall = old })
	return calls
}
func fileInventoryAccount() map[string]any {
	return map[string]any{"source": "file", "runtime_only": false, "id": "id", "name": "a.json", "provider": "codex", "auth_index": "index", "email": "a@example.invalid"}
}
func TestInventoryIgnoresMemoryAndRuntimeOnlyCredentials(t *testing.T) {
	file := fileInventoryAccount()
	calls := inventoryHost(t, []map[string]any{{"source": "memory", "provider": "codex"}, {"source": "file", "runtime_only": true, "provider": "antigravity"}, file, {"source": "file", "provider": "other"}}, map[string]map[string]any{"index": {"type": "codex", "email": "a@example.invalid", "account_id": "account", "prefix": "prefix"}})
	got, err := inventory()
	if err != nil || len(got) != 1 || *calls != 1 {
		t.Fatalf("mixed inventory failed: %#v reads=%d err=%v", got, *calls, err)
	}
	if got[0].AccountID != "account" || got[0].Prefix != "prefix" {
		t.Fatalf("file metadata not loaded %#v", got[0])
	}
}
func TestInventoryRejectsDuplicateRoutingIdentifiers(t *testing.T) {
	for _, field := range []string{"name", "id", "auth_index"} {
		t.Run(field, func(t *testing.T) {
			first := fileInventoryAccount()
			second := map[string]any{"source": "file", "id": "id2", "name": "b.json", "provider": "codex", "auth_index": "index2", "email": "a@example.invalid"}
			second[field] = first[field]
			inventoryHost(t, []map[string]any{first, second}, map[string]map[string]any{"index": {}, "index2": {}})
			if got, err := inventory(); err == nil {
				t.Fatalf("duplicate %s accepted %#v", field, got)
			}
		})
	}
}
func TestInventoryRejectsListFileIdentityMismatch(t *testing.T) {
	for _, raw := range []map[string]any{{"type": "antigravity", "email": "a@example.invalid"}, {"type": "codex", "email": "different@example.invalid"}} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			inventoryHost(t, []map[string]any{fileInventoryAccount()}, map[string]map[string]any{"index": raw})
			if got, err := inventory(); err == nil {
				t.Fatalf("list/file mismatch accepted %#v", got)
			}
		})
	}
}
