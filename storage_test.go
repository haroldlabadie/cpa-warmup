package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}

func TestStorePreservesLegacyUnknownFieldsAndUncertainAttempts(t *testing.T) {
	dir := t.TempDir()
	raw := `{"schema":1,"plugin":"cpa-warmup","migration":{"original_sha":"abc"},"legacy":{"version":2,"accounts":{"a":{"manual":true}},"future_legacy":{"n":9007199254740993},"slots":{"2026-10-19T10:00+08:00":{"a":{"status":"unconfirmed","attempt_token":"old","event_id":9007199254740993,"future":{"nested":[1,"x",null]}}}}},"latest_success":{},"attempt_gates":{},"identities":{}}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := s.state.Slots["2026-10-19T10:00+08:00"]["a"]
	if !unresolved(r) || num(r, "event_id") != 9007199254740993 {
		t.Fatalf("lost uncertain attempt or integer: %#v", r)
	}
	r["last_reason"] = "still-unknown"
	if err = s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.close()
	s, err = openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if string(s.state.Legacy["future_legacy"]) != "{\n      \"n\": 9007199254740993\n    }" {
		var got map[string]json.Number
		if err = json.Unmarshal(s.state.Legacy["future_legacy"], &got); err != nil || got["n"] != "9007199254740993" {
			t.Fatalf("unknown legacy lost: %s", s.state.Legacy["future_legacy"])
		}
	}
	r = s.state.Slots["2026-10-19T10:00+08:00"]["a"]
	if str(r, "status") != "unconfirmed" || num(r, "event_id") != 9007199254740993 || r["future"] == nil {
		t.Fatalf("roundtrip changed historical proof: %#v", r)
	}
}

func TestStoreRejectsCorruptionAndReleasesLockOnFailure(t *testing.T) {
	for _, raw := range []string{`{`, `{"schema":2,"plugin":"cpa-warmup","legacy":{"slots":{}}}`, `{"schema":1,"plugin":"wrong","legacy":{"slots":{}}}`, `{"schema":1,"plugin":"cpa-warmup","legacy":{"slots":null}}`, `{"schema":1,"plugin":"cpa-warmup","legacy":{"slots":{}},"unknown":true}`, `{"schema":1,"plugin":"cpa-warmup","legacy":{"slots":{}}} {}`} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "state.json")
			if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if s, err := openStore(dir); err == nil {
				s.close()
				t.Fatal("accepted corrupt state")
			}
			if err := os.WriteFile(p, []byte(`{"schema":1,"plugin":"cpa-warmup","legacy":{"version":2,"accounts":{},"slots":{}}}`), 0600); err != nil {
				t.Fatal(err)
			}
			s, err := openStore(dir)
			if err != nil {
				t.Fatalf("failed open leaked lock: %v", err)
			}
			s.close()
		})
	}
}

func TestStoreMissingSentJournalCannotReinitialize(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.state.Slots["2026-10-19T10:00+08:00"] = map[string]Record{"a.json": {"status": "sent", "attempt_token": "uuid", "sent_ms": int64(1000)}}
	if err = s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.close()
	p := filepath.Join(dir, "state.json")
	proof, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openStore(dir); err == nil {
		reopened.close()
		t.Fatal("missing previously initialized sent journal was reset")
	}
	if _, err = os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("missing journal was silently replaced")
	}
	// Explicit restoration of the last complete journal permits reopening; the
	// failed missing-state attempt must release its lock without losing proof.
	if err = os.WriteFile(p, proof, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := openStore(dir)
	if err != nil {
		t.Fatalf("restored journal cannot reopen: %v", err)
	}
	defer reopened.close()
	if str(reopened.state.Slots["2026-10-19T10:00+08:00"]["a.json"], "attempt_token") != "uuid" {
		t.Fatal("sent proof changed after recovery")
	}
}

func TestStoreSingleWorkerAndPermissions(t *testing.T) {
	dir := t.TempDir()
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if other, err := openStore(dir); err == nil {
		other.close()
		t.Fatal("second worker acquired same directory")
	}
	for _, p := range []string{dir, filepath.Join(dir, "state.json"), filepath.Join(dir, "worker.lock")} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("private path has public bits: %s %v", p, info.Mode())
		}
	}
	s.close()
	other, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	other.close()
}

func TestStoreRejectsMalformedSlotJournal(t *testing.T) {
	cases := []struct {
		name   string
		slot   string
		record Record
	}{
		{"invalid-date", "not-a-slot", Record{"status": "pending"}},
		{"unknown-status", "2026-10-19T10:00+08:00", Record{"status": "future-status"}},
		{"missing-status", "2026-10-19T10:00+08:00", Record{}},
		{"string-attempts", "2026-10-19T10:00+08:00", Record{"status": "pending", "attempts": "1"}},
		{"negative-attempts", "2026-10-19T10:00+08:00", Record{"status": "pending", "attempts": -1}},
		{"fractional-attempts", "2026-10-19T10:00+08:00", Record{"status": "pending", "attempts": 1.5}},
		{"negative-sent", "2026-10-19T10:00+08:00", Record{"status": "sent", "sent_ms": -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := openStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			s.state.Slots[tc.slot] = map[string]Record{"a": tc.record}
			if err = s.saveLocked(); err != nil {
				t.Fatal(err)
			}
			s.close()
			if reopened, err := openStore(dir); err == nil {
				reopened.close()
				t.Fatalf("malformed journal accepted %#v", tc.record)
			}
		})
	}
}
