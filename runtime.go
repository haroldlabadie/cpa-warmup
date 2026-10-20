package main

import "errors"

// ensureIdentity returns "ok" or a precise reason. Missing proof never causes
// an automatic binding when unresolved history exists for the same account.
func (e *Engine) ensureIdentity(a Account) (string, string) {
	return e.identityStatus(a.Name, a.identity())
}

func identityGeneration(e *Engine, name string) string {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if g := e.store.state.IdentityGenerations[name]; g != nil {
		return g.ID
	}
	return ""
}

func identityGenerationNumber(e *Engine, name string) int {
	e.store.mu.Lock()
	defer e.store.mu.Unlock()
	if g := e.store.state.IdentityGenerations[name]; g != nil {
		return g.Generation
	}
	return 0
}

type recoveryPlan struct {
	Action    string
	Account   Account
	Identity  string
	Operator  string
	Reason    string
	Operation string
	Now       int64
	Sealed    []Sealed
	FloorMS   int64
}

// planRecovery performs every decision once, against a clone. Both dry-run and
// apply call it, so a dry-run success cannot still fail later for a decision
// reason (duplicate operation, missing/future sent_ms, unsafe bind proof).
func planRecovery(s *State, req recoverRequest) (recoveryPlan, error) {
	var plan recoveryPlan
	now := nowMillis()
	if req.Account.Name == "" || (req.Account.Provider != "codex" && req.Account.Provider != "antigravity") {
		return plan, errors.New("invalid account")
	}
	if len(req.Identity) != 64 || req.Operator == "" || req.Reason == "" || req.OperationID == "" {
		return plan, errors.New("recovery metadata incomplete")
	}
	if req.Account.identity() != req.Identity {
		return plan, errors.New("current identity metadata mismatch")
	}
	if req.Action != "bind" && req.Action != "seal-and-recover" {
		return plan, errors.New("unsupported recovery action")
	}
	if _, exists := s.Identities[req.Account.Name]; !exists {
		return plan, errors.New("account has no identity state")
	}
	for _, r := range s.IdentityRecovery {
		if r.ID == req.OperationID {
			return plan, errors.New("recovery operation already applied")
		}
	}
	old := s.Identities[req.Account.Name]
	if req.Action == "bind" && old == req.Identity {
		return plan, errors.New("identity already bound")
	}
	if req.Action == "bind" && s.IdentityGenerations[req.Account.Name] != nil {
		return plan, errors.New("sealed generation requires seal-and-recover")
	}
	sealed, floor, err := sealedUnresolved(s, req.Account.Name, now, req.Action == "seal-and-recover")
	if err != nil {
		return plan, err
	}
	if req.Action == "bind" {
		if err := provesBinding(s, req.Account.Name, req.Identity); err != nil {
			return plan, err
		}
	}
	if req.Action == "seal-and-recover" {
		if len(sealed) == 0 {
			return plan, errors.New("no unresolved records to seal")
		}
		if floor <= now {
			floor = now + 45000
		}
	}
	return recoveryPlan{Action: req.Action, Account: req.Account, Identity: req.Identity,
		Operator: req.Operator, Reason: req.Reason, Operation: req.OperationID,
		Now: now, Sealed: sealed, FloorMS: floor}, nil
}

// sealedUnresolved gathers evidence without changing any record. requireProof
// rejects missing or future send times for sealing; bind does not use it.
func sealedUnresolved(s *State, name string, now int64, requireProof bool) ([]Sealed, int64, error) {
	sealed := []Sealed{}
	floor := int64(0)
	for sid, rows := range s.Slots {
		for key, r := range rows {
			if accountName(key) != name || !unresolved(r) {
				continue
			}
			sent := num(r, "sent_ms")
			if sent <= 0 || sent > now {
				if requireProof {
					return nil, 0, errors.New("unresolved request has missing or future sent_ms")
				}
				continue
			}
			sealed = append(sealed, sealedFrom(key, sid, r))
			if v := sent + fiveHour + 45000; v > floor {
				floor = v
			}
		}
	}
	return sealed, floor, nil
}

// provesBinding accepts only machine evidence already stored for the same
// identity. Saved migration identities or names alone never prove an account.
func provesBinding(s *State, name, identity string) error {
	seen := 0
	for sid, rows := range s.Slots {
		for key, r := range rows {
			if accountName(key) != name || !unresolved(r) {
				continue
			}
			seen++
			if id := str(r, "identity"); id != "" {
				if id != identity {
					return errors.New("unresolved record identity conflicts with binding")
				}
				continue
			}
			token := str(r, "attempt_token")
			gate, ok := s.AttemptGates[token]
			if !ok || token == "" || gate.Identity != identity || gate.AuthIndex == "" || gate.AuthIndex != str(r, "target_auth_index") || gate.AuthID != str(r, "target_auth_id") {
				return errors.New("unresolved record lacks machine identity proof")
			}
			_ = sid
		}
	}
	if seen == 0 {
		return errors.New("no unresolved record to bind")
	}
	return nil
}

func applyPlan(s *State, plan recoveryPlan, stateSHA string) error {
	if s.Identities == nil {
		s.Identities = map[string]string{}
	}
	if s.AttemptGates == nil {
		s.AttemptGates = map[string]Gate{}
	}
	if s.IdentityGenerations == nil {
		s.IdentityGenerations = map[string]*Generation{}
	}
	if s.IdentityRecovery == nil {
		s.IdentityRecovery = []IdentityRecovery{}
	}
	prior := s.Identities[plan.Account.Name]
	entry := IdentityRecovery{
		ID: plan.Operation, Action: plan.Action, Account: plan.Account.Name, Operator: plan.Operator,
		Reason: plan.Reason, CreatedMS: plan.Now, BeforeIdentity: prior, AfterIdentity: plan.Identity,
		StateDigest: stateSHA, Sealed: plan.Sealed, FloorMS: plan.FloorMS,
	}
	s.Identities[plan.Account.Name] = plan.Identity
	if plan.Action == "seal-and-recover" {
		gen := 1
		if g := s.IdentityGenerations[plan.Account.Name]; g != nil {
			gen = g.Generation + 1
		}
		s.IdentityGenerations[plan.Account.Name] = &Generation{
			ID: plan.Operation, Identity: plan.Identity, CreatedMS: plan.Now, Operator: plan.Operator,
			Reason: plan.Reason, PriorIdentity: prior, Sealed: plan.Sealed, FloorMS: plan.FloorMS, Generation: gen,
		}
	}
	s.IdentityRecovery = append(s.IdentityRecovery, entry)
	return nil
}
