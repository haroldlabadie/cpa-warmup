package main

import (
	"encoding/json"
	"errors"
)

const stateSchema = 2

func cloneRecord(r Record) Record {
	b, _ := json.Marshal(r)
	var out Record
	decodePreserving(b, &out)
	return out
}

// stateDigest is a stable summary over every persisted decision, not just a
// mtime. The recovery helper requires the operator to quote this value.
func stateDigest(s *State) (string, error) {
	if s == nil || s.Legacy == nil || s.Slots == nil {
		return "", errors.New("incomplete state")
	}
	b, e := json.Marshal(struct {
		Schema              int                        `json:"schema"`
		Plugin              string                     `json:"plugin"`
		Migration           json.RawMessage            `json:"migration,omitempty"`
		Legacy              map[string]json.RawMessage `json:"legacy"`
		LatestSuccess       map[string]int64           `json:"latest_success"`
		AttemptGates        map[string]Gate            `json:"attempt_gates"`
		Identities          map[string]string          `json:"identities"`
		IdentityGenerations map[string]*Generation     `json:"identity_generations,omitempty"`
		IdentityRecovery    []IdentityRecovery         `json:"identity_recovery,omitempty"`
		Slots               Slots                      `json:"slots"`
	}{s.Schema, s.Plugin, s.Migration, s.Legacy, s.LatestSuccess, s.AttemptGates, s.Identities, s.IdentityGenerations, s.IdentityRecovery, s.Slots})
	if e != nil {
		return "", e
	}
	return hash(string(b)), nil
}
