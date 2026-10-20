package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

type Record map[string]any
type Slots map[string]map[string]Record
type Gate struct {
	AuthID         string `json:"auth_id"`
	AuthIndex      string `json:"auth_index"`
	Identity       string `json:"identity"`
	RequestedModel string `json:"requested_model"`
	CallerScope    string `json:"caller_scope"`
	SentMS         int64  `json:"sent_ms"`
	TraceID        string `json:"trace_id,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	AfterAuth      bool   `json:"after_auth"`
}

// State has one persistent source for request history: Legacy["slots"]. Slots
// is an in-memory projection only. Schema 2 adds identity generations and
// recovery audit; unknown legacy fields are retained byte-for-byte.
type State struct {
	Schema              int                        `json:"schema"`
	Plugin              string                     `json:"plugin"`
	Migration           json.RawMessage            `json:"migration,omitempty"`
	Legacy              map[string]json.RawMessage `json:"legacy"`
	LatestSuccess       map[string]int64           `json:"latest_success"`
	AttemptGates        map[string]Gate            `json:"attempt_gates"`
	Identities          map[string]string          `json:"identities"`
	IdentityGenerations map[string]*Generation     `json:"identity_generations,omitempty"`
	IdentityRecovery    []IdentityRecovery         `json:"identity_recovery,omitempty"`
	Slots               Slots                      `json:"-"`
}
type Store struct {
	mu    sync.Mutex
	path  string
	state State
	lock  *os.File
}

func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var x any
	if d.Decode(&x) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func decodePreserving(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	var x any
	if d.Decode(&x) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func readLimited(path string, max int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	i, e := f.Stat()
	if e != nil || !i.Mode().IsRegular() || i.Size() > max {
		return nil, errors.New("invalid file")
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}
func atomicJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if len(b) > 16<<20 {
		return errors.New("state capacity exceeded")
	}
	dir := filepath.Dir(path)
	f, e := os.CreateTemp(dir, ".warmup-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func openStore(dir string) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("data_dir must be absolute")
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	i, e := os.Lstat(dir)
	if e != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid data_dir")
	}
	if e = os.Chmod(dir, 0700); e != nil {
		return nil, e
	}
	_, markerErr := os.Lstat(filepath.Join(dir, "worker.lock"))
	wasInitialized := markerErr == nil
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return nil, markerErr
	}
	lock, e := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, errors.New("another worker owns data_dir")
	}
	s := &Store{path: filepath.Join(dir, "state.json"), lock: lock}
	b, e := readLimited(s.path, 16<<20)
	if os.IsNotExist(e) && wasInitialized {
		s.close()
		return nil, errors.New("initialized state is missing; automatic actions stopped")
	}
	if os.IsNotExist(e) {
		s.state = State{Schema: stateSchema, Plugin: pluginID, Legacy: map[string]json.RawMessage{
			"version": json.RawMessage(`2`), "accounts": json.RawMessage(`{}`), "slots": json.RawMessage(`{}`),
		}, Slots: Slots{}}
	} else if e != nil {
		s.close()
		return nil, e
	} else if e = strict(b, &s.state); e != nil {
		s.close()
		return nil, errors.New("state decode failed; automatic actions stopped")
	}
	if (s.state.Schema != 1 && s.state.Schema != stateSchema) || s.state.Plugin != pluginID || s.state.Legacy == nil {
		s.close()
		return nil, errors.New("unsupported state schema")
	}
	if s.state.Schema == 1 && (s.state.IdentityGenerations != nil || s.state.IdentityRecovery != nil) {
		s.close()
		return nil, errors.New("schema 1 carries schema 2 state")
	}
	if s.state.Legacy["slots"] == nil {
		s.close()
		return nil, errors.New("missing legacy slots")
	}
	if e = decodePreserving(s.state.Legacy["slots"], &s.state.Slots); e != nil || s.state.Slots == nil {
		s.close()
		return nil, errors.New("invalid legacy slots")
	}
	if e = validateSlots(s.state.Slots); e != nil {
		s.close()
		return nil, e
	}
	if e = validateGenerations(s.state.IdentityGenerations, s.state.Slots); e != nil {
		s.close()
		return nil, e
	}
	if e = validateRecovery(s.state.IdentityRecovery); e != nil {
		s.close()
		return nil, e
	}
	if e = validateIdentityLinks(&s.state); e != nil {
		s.close()
		return nil, e
	}
	if s.state.AttemptGates == nil {
		s.state.AttemptGates = map[string]Gate{}
	}
	if s.state.LatestSuccess == nil {
		s.state.LatestSuccess = map[string]int64{}
	}
	if s.state.Identities == nil {
		s.state.Identities = map[string]string{}
	}
	if s.state.IdentityGenerations == nil {
		s.state.IdentityGenerations = map[string]*Generation{}
	}
	if s.state.IdentityRecovery == nil {
		s.state.IdentityRecovery = []IdentityRecovery{}
	}
	if e = s.saveLocked(); e != nil {
		s.close()
		return nil, e
	}
	return s, nil
}
func (s *Store) saveLocked() error {
	if s.state.Slots == nil {
		return errors.New("missing slots projection")
	}
	b, e := json.Marshal(s.state.Slots)
	if e != nil {
		return e
	}
	if s.state.Legacy == nil {
		s.state.Legacy = map[string]json.RawMessage{}
	}
	// Legacy.slots is the only persisted request history. Schema upgrades do
	// not create a second top-level copy that could diverge.
	s.state.Legacy["slots"] = b
	s.state.Schema = stateSchema
	s.state.Plugin = pluginID
	return atomicJSON(s.path, s.state)
}
func (s *Store) close() {
	if s.lock != nil {
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		s.lock.Close()
		s.lock = nil
	}
}
func hash(s string) string          { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func str(m Record, k string) string { v, _ := m[k].(string); return v }
func num(m Record, k string) int64 {
	switch x := m[k].(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		v, _ := x.Int64()
		return v
	}
	return 0
}
func unresolved(r Record) bool {
	switch str(r, "status") {
	case "sending", "sent", "unconfirmed":
		return true
	}
	return false
}
func validateSlots(slots Slots) error {
	for sid, records := range slots {
		if _, e := parseSlot(sid); e != nil {
			return errors.New("invalid persisted slot")
		}
		if records == nil {
			return errors.New("invalid persisted records")
		}
		for _, r := range records {
			if r == nil {
				return errors.New("invalid persisted record")
			}
			switch str(r, "status") {
			case "pending", "retry", "sending", "sent", "unconfirmed", "confirmed", "request_completed", "uncertainty_expired", "skipped", "error":
			default:
				return errors.New("unknown persisted record status")
			}
			for _, k := range []string{"attempts", "sent_ms", "next_attempt_ms", "deferred_until_ms"} {
				if v, ok := r[k]; ok {
					n, ok := v.(json.Number)
					if !ok {
						return errors.New("invalid persisted number")
					}
					x, e := n.Int64()
					if e != nil || x < 0 {
						return errors.New("invalid persisted number")
					}
				}
			}
		}
	}
	return nil
}

func helper(args []string) int {
	switch {
	case len(args) == 2 && args[0] == "validate-store":
		b, e := readLimited(args[1], 16<<20)
		if e != nil {
			return 1
		}
		var s State
		if strict(b, &s) != nil || (s.Schema != 1 && s.Schema != stateSchema) || s.Plugin != pluginID || s.Legacy == nil || s.Legacy["slots"] == nil || (s.Schema == 1 && (s.IdentityGenerations != nil || s.IdentityRecovery != nil)) {
			return 1
		}
		var slots Slots
		if decodePreserving(s.Legacy["slots"], &slots) != nil || validateSlots(slots) != nil {
			return 1
		}
		if validateGenerations(s.IdentityGenerations, slots) != nil || validateRecovery(s.IdentityRecovery) != nil {
			return 1
		}
		s.Slots = slots
		if validateIdentityLinks(&s) != nil {
			return 1
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"valid": true, "schema": s.Schema, "slot_groups": len(slots), "identities": len(s.Identities), "generations": len(s.IdentityGenerations), "recovery": len(s.IdentityRecovery)})
		return 0
	case len(args) == 2 && args[0] == "recover-identity":
		return recoverIdentityCommand(args[1])
	case len(args) == 1 && args[0] == "identity":
		b, e := io.ReadAll(io.LimitReader(os.Stdin, 65537))
		if e != nil || len(b) > 65536 {
			return 1
		}
		var a Account
		if strict(b, &a) != nil {
			return 1
		}
		json.NewEncoder(os.Stdout).Encode(map[string]string{"identity": a.identity()})
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: cpa-warmup-helper identity < metadata.json | validate-store < state.json | recover-identity < request.json")
		return 2
	}
}
