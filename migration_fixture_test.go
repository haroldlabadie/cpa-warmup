package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMigratorFixtureSurvivesEngineOpenAndSave(t *testing.T) {
	source, err := os.ReadFile("testdata/migration-state.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "state.json"), source, 0600); err != nil {
		t.Fatal(err)
	}
	var before State
	if strict(source, &before) != nil {
		t.Fatal("invalid fixture")
	}
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.saveLocked(); err != nil {
		t.Fatal(err)
	}
	s.close()
	saved, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var after State
	if strict(saved, &after) != nil {
		t.Fatal("invalid output")
	}
	var a, b any
	beforeRaw, _ := json.Marshal(before.Legacy)
	afterRaw, _ := json.Marshal(after.Legacy)
	decodePreserving(beforeRaw, &a)
	decodePreserving(afterRaw, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("legacy migration fields changed during engine persistence")
	}
	if !reflect.DeepEqual(before.Identities, after.Identities) {
		t.Fatal("identity proofs changed")
	}
}
