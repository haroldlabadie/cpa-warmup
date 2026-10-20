package main

import (
	"encoding/json"
	"errors"
)

// A Generation seals the old credential identity for one account name and
// starts a new one. Legacy attempts keep their own lack of identity; sealing
// never rewrites them. The new generation can only use slots strictly after
// the retirement floor, which covers every sealed unresolved request.
type Generation struct {
	ID            string   `json:"id"`
	Identity      string   `json:"identity"`
	CreatedMS     int64    `json:"created_ms"`
	Operator      string   `json:"operator"`
	Reason        string   `json:"reason"`
	PriorIdentity string   `json:"prior_identity,omitempty"`
	Sealed        []Sealed `json:"sealed,omitempty"`
	FloorMS       int64    `json:"floor_ms"`
	Generation    int      `json:"generation"`
}

// Sealed identifies one unresolved record kept as-is in the journal. Digest
// hashes the complete record so later tampering is visible.
type Sealed struct {
	Key    string `json:"key"`
	Slot   string `json:"slot"`
	Status string `json:"status"`
	SentMS int64  `json:"sent_ms"`
	Token  string `json:"attempt_token,omitempty"`
	Digest string `json:"digest"`
}

// IdentityRecovery is an append-only audit entry for an offline decision.
type IdentityRecovery struct {
	ID             string   `json:"id"`
	Action         string   `json:"action"`
	Account        string   `json:"account"`
	Operator       string   `json:"operator"`
	Reason         string   `json:"reason"`
	CreatedMS      int64    `json:"created_ms"`
	BeforeIdentity string   `json:"before_identity,omitempty"`
	AfterIdentity  string   `json:"after_identity"`
	StateDigest    string   `json:"state_sha256,omitempty"`
	Sealed         []Sealed `json:"sealed,omitempty"`
	FloorMS        int64    `json:"floor_ms,omitempty"`
}

// sealedFrom hashes the complete record so no account name, target identity,
// status, token, or unknown field can change after sealing.
func sealedFrom(key, slot string, r Record) Sealed {
	b, _ := json.Marshal(r)
	return Sealed{Key: key, Slot: slot, Status: str(r, "status"), SentMS: num(r, "sent_ms"), Token: str(r, "attempt_token"), Digest: hash(string(b))}
}

func validateGenerations(gens map[string]*Generation, slots Slots) error {
	for name, g := range gens {
		if g == nil || name == "" || len(name) > 256 || g.ID == "" || len(g.Identity) != 64 || g.Generation < 1 || g.CreatedMS <= 0 || g.FloorMS <= 0 || g.Operator == "" || g.Reason == "" {
			return errors.New("invalid identity generation")
		}
		if g.FloorMS <= g.CreatedMS || len(g.Sealed) == 0 {
			return errors.New("invalid identity generation floor")
		}
		for _, s := range g.Sealed {
			if s.Key == "" || s.Slot == "" || s.Digest == "" || s.SentMS <= 0 || s.SentMS > g.CreatedMS || s.SentMS+fiveHour+45000 > g.FloorMS {
				return errors.New("invalid sealed attempt")
			}
			// Evidence must remain intact after sealing.
			if rows := slots[s.Slot]; rows == nil || rows[s.Key] == nil || sealedFrom(s.Key, s.Slot, rows[s.Key]).Digest != s.Digest {
				return errors.New("sealed attempt evidence changed")
			}
		}
	}
	return nil
}

func sameSealed(a, b []Sealed) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateIdentityLinks(s *State) error {
	for name, g := range s.IdentityGenerations {
		if s.Identities[name] != g.Identity {
			return errors.New("identity generation is not bound")
		}
		matched := false
		for _, r := range s.IdentityRecovery {
			if r.ID == g.ID && r.Account == name && r.Action == "seal-and-recover" {
				if r.AfterIdentity != g.Identity || r.BeforeIdentity != g.PriorIdentity || r.FloorMS != g.FloorMS || r.CreatedMS != g.CreatedMS || r.Operator != g.Operator || r.Reason != g.Reason || !sameSealed(r.Sealed, g.Sealed) {
					return errors.New("identity generation audit mismatch")
				}
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("identity generation audit missing")
		}
	}
	return nil
}

func validateRecovery(entries []IdentityRecovery) error {
	seen := map[string]bool{}
	for _, r := range entries {
		if r.ID == "" || r.Account == "" || len(r.AfterIdentity) != 64 || r.Operator == "" || r.Reason == "" || r.CreatedMS <= 0 || (r.Action != "bind" && r.Action != "seal-and-recover") {
			return errors.New("invalid recovery audit")
		}
		// Older audit rows may omit the prior summary; every new apply writes
		// it. A present value must still be a full SHA256 digest.
		if r.StateDigest != "" && len(r.StateDigest) != 64 {
			return errors.New("invalid recovery state digest")
		}
		if seen[r.ID] {
			return errors.New("duplicate recovery operation")
		}
		seen[r.ID] = true
	}
	return nil
}

// identityStatus decides one account without binding anything implicitly.
// A new installation (no identity and no unresolved history) may bind its
// first inventory. Migration-marked or mismatched accounts stay blocked.
func (e *Engine) boundIdentity(name, id string) (string, string) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if old, exists := e.store.state.Identities[name]; exists {
		if old == "migration-unproven" || old == "" {
			return "unproven", "migration-identity-unproven"
		}
		if old != id {
			return "mismatch", "credential-identity-mismatch"
		}
		return "ok", ""
	}
	if g := e.store.state.IdentityGenerations[name]; g != nil {
		if g.Identity == id {
			return "ok", ""
		}
		return "mismatch", "identity-generation-mismatch"
	}
	return "unbound", "identity-not-bound"
}

func (e *Engine) identityStatus(name, id string) (string, string) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	st := &e.store.state
	if g := st.IdentityGenerations[name]; g != nil {
		if g.Identity == id {
			return "ok", ""
		}
		return "mismatch", "identity-generation-mismatch"
	}
	if old, exists := st.Identities[name]; exists {
		if old == "migration-unproven" || old == "" {
			return "unproven", "migration-identity-unproven"
		}
		if old != id {
			return "mismatch", "credential-identity-mismatch"
		}
		return "ok", ""
	}
	for _, rs := range st.Slots {
		for key, r := range rs {
			if accountName(key) == name && unresolved(r) {
				return "unproven", "unresolved-history-without-identity"
			}
		}
	}
	st.Identities[name] = id
	if e.store.saveLocked() != nil {
		e.fail("identity persistence failed")
		return "stopped", "identity-persistence-failed"
	}
	return "ok", ""
}

// sealedAttempt identifies a sealed record by its unique attempt UUID.
func (e *Engine) sealedAttempt(name string, r Record) bool {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	g := e.store.state.IdentityGenerations[name]
	if g == nil {
		return false
	}
	token := str(r, "attempt_token")
	if token == "" {
		return false
	}
	for _, s := range g.Sealed {
		if s.Token == token {
			return true
		}
	}
	return false
}

func accountName(key string) string {
	for i := 0; i+2 < len(key); i++ {
		if key[i] == ':' && key[i+1] == ':' {
			return key[:i]
		}
	}
	return key
}

// slotAdmitted applies the sealed generation floor. Slots at or before the
// floor are surfaced as blocked forever; they are never rewritten or replayed.
func (e *Engine) slotAdmitted(name string, start int64) (bool, string) {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	g := e.store.state.IdentityGenerations[name]
	if g == nil {
		return true, ""
	}
	// A recovered generation begins with future slots only. The current
	// already-started slot is not a backfill target even if it began after the
	// retirement floor.
	if start <= g.FloorMS || start <= g.CreatedMS {
		return false, "sealed-generation-floor"
	}
	return true, ""
}

// HasUnrepresentableFailure reports state the old v2 scheduler cannot express.
// The export path uses it to refuse a lossy rollback.
func HasUnrepresentableFailure(s *State) bool {
	if len(s.IdentityGenerations) > 0 || len(s.IdentityRecovery) > 0 {
		return true
	}
	for _, id := range s.Identities {
		if id == "migration-unproven" {
			return true
		}
	}
	return false
}
