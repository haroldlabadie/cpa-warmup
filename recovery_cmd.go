package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// recoverRequest is intentionally explicit and small: it carries no secret or
// credential contents. Apply is a separate flag, default is dry-run.
type recoverRequest struct {
	DataDir     string  `json:"data_dir"`
	StateSHA256 string  `json:"state_sha256"`
	OperationID string  `json:"operation_id"`
	Account     Account `json:"account"`
	Identity    string  `json:"current_identity"`
	Action      string  `json:"action"`
	Operator    string  `json:"operator"`
	Reason      string  `json:"reason"`
	Apply       bool    `json:"apply"`
}

func recoverIdentityCommand(path string) int {
	b, e := readLimited(path, 65536)
	if e != nil {
		recoverError("request file unavailable")
		return 1
	}
	var req recoverRequest
	if strict(b, &req) != nil || !filepath.IsAbs(req.DataDir) {
		recoverError("invalid recovery request")
		return 1
	}
	lock, e := acquireWorkerLock(req.DataDir)
	if e != nil {
		recoverError("worker is running or data_dir is invalid")
		return 1
	}
	defer releaseWorkerLock(lock)
	s, e := loadRecoveryState(req.DataDir)
	if e != nil {
		recoverError("state unavailable")
		return 1
	}
	digest, e := stateDigest(&s)
	if e != nil {
		recoverError("state digest failed")
		return 1
	}
	if req.Apply {
		// Apply must quote the exact current summary; this prevents applying a
		// stale operator decision to an unrelated or changed journal.
		if req.StateSHA256 == "" || req.StateSHA256 != digest {
			recoverError("state summary missing or mismatched")
			return 1
		}
	} else if req.StateSHA256 != "" && req.StateSHA256 != digest {
		recoverError("state summary mismatch")
		return 1
	}
	plan, e := planRecovery(&s, req)
	if e != nil {
		recoverError(e.Error())
		return 1
	}
	out := map[string]any{
		"dry_run": !req.Apply, "applied": false, "state_sha256": digest,
		"account": req.Account.Name, "action": plan.Action, "operation_id": plan.Operation,
		"before_identity": s.Identities[req.Account.Name], "current_identity": plan.Identity,
		"unresolved": len(plan.Sealed), "floor_ms": plan.FloorMS,
	}
	if req.Apply {
		if e = applyPlan(&s, plan, digest); e != nil {
			recoverError(e.Error())
			return 1
		}
		if validateGenerations(s.IdentityGenerations, s.Slots) != nil || validateRecovery(s.IdentityRecovery) != nil || validateIdentityLinks(&s) != nil {
			recoverError("recovery would produce invalid state")
			return 1
		}
		if e = saveRecoveryState(req.DataDir, &s); e != nil {
			recoverError("state save failed")
			return 1
		}
		out["applied"] = true
		out["dry_run"] = false
	}
	json.NewEncoder(os.Stdout).Encode(out)
	return 0
}

func recoverError(message string) {
	json.NewEncoder(os.Stderr).Encode(map[string]string{"error": message})
}

func acquireWorkerLock(dir string) (*os.File, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("absolute data_dir required")
	}
	i, e := os.Lstat(dir)
	if e != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("invalid data_dir")
	}
	f, e := os.OpenFile(filepath.Join(dir, "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = flockExclusive(f); e != nil {
		f.Close()
		return nil, errors.New("worker running")
	}
	return f, nil
}
func releaseWorkerLock(f *os.File) {
	if f != nil {
		flockRelease(f)
		f.Close()
	}
}
func loadRecoveryState(dir string) (State, error) {
	var s State
	b, e := readLimited(filepath.Join(dir, "state.json"), 16<<20)
	if e != nil {
		return s, e
	}
	if strict(b, &s) != nil || s.Plugin != pluginID || s.Legacy == nil || s.Legacy["slots"] == nil {
		return s, errors.New("invalid state")
	}
	if s.Schema != 1 && s.Schema != stateSchema {
		return s, errors.New("unsupported state schema")
	}
	if s.Schema == 1 && (s.IdentityGenerations != nil || s.IdentityRecovery != nil) {
		return s, errors.New("schema 1 carries schema 2 state")
	}
	if decodePreserving(s.Legacy["slots"], &s.Slots) != nil || s.Slots == nil {
		return s, errors.New("invalid slots")
	}
	if validateSlots(s.Slots) != nil || validateGenerations(s.IdentityGenerations, s.Slots) != nil || validateRecovery(s.IdentityRecovery) != nil || validateIdentityLinks(&s) != nil {
		return s, errors.New("invalid state")
	}
	return s, nil
}
func saveRecoveryState(dir string, s *State) error {
	if s.Slots == nil || s.Legacy == nil {
		return errors.New("missing slots")
	}
	b, e := json.Marshal(s.Slots)
	if e != nil {
		return e
	}
	s.Legacy["slots"] = b
	s.Schema = stateSchema
	s.Plugin = pluginID
	return atomicJSON(filepath.Join(dir, "state.json"), s)
}
