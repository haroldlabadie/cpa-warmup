package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata"
)

type Bootstrap struct {
	Store        yaml.Node `yaml:"store"`
	Version      string    `yaml:"version"`
	Priority     int       `yaml:"priority"`
	Enabled      bool      `yaml:"enabled"`
	Observe      bool      `yaml:"observe"`
	DataDir      string    `yaml:"data_dir"`
	CPAMPURL     string    `yaml:"cpamp_url"`
	CPAURL       string    `yaml:"cpa_url"`
	SecretFile   string    `yaml:"secret_file"`
	SettingsFile string    `yaml:"settings_file"`
}
type Settings struct {
	Version      int      `json:"version"`
	Enabled      bool     `json:"enabled"`
	SlotHours    []int    `json:"slot_hours"`
	GraceSeconds int      `json:"slot_grace_seconds"`
	CheckSeconds int      `json:"check_seconds"`
	Timezone     string   `json:"timezone"`
	BufferSeconds int     `json:"reset_buffer_seconds"`
	Targets      []Target `json:"targets"`
}

// Target keeps each warmup channel independent. Codex has one provider-wide
// window, while Antigravity exposes two independently metered model families.
type Target struct {
	Scope    string   `json:"scope"`
	Accounts []string `json:"accounts"`
	Model    string   `json:"model"`
}
type Secrets struct {
	ManagementKey string `json:"management_key"`
	PrewarmKey    string `json:"prewarm_key"`
}

func defaults() Settings {
	return Settings{Version: 2, Enabled: true, SlotHours: []int{5, 10, 15, 20}, GraceSeconds: 5400, CheckSeconds: 20, Timezone: defaultTimezone, BufferSeconds: 15, Targets: []Target{
		{Scope: "codex", Accounts: []string{}, Model: "gpt-6-luna"},
		{Scope: "gemini", Accounts: []string{}, Model: "gemini-3.8-flash-high"},
		{Scope: "third_party", Accounts: []string{}, Model: "claude-sonnet-4-6"},
	}}
}
func (s Settings) validate() error {
	if s.Version != 2 || s.GraceSeconds < 600 || s.GraceSeconds > 10800 || s.BufferSeconds > 300 || (s.Timezone != "" && !validTimezone(s.Timezone)) || (s.BufferSeconds != 0 && s.BufferSeconds < 5) || s.CheckSeconds < 5 || s.CheckSeconds > 300 || len(s.SlotHours) == 0 || len(s.SlotHours) > 24 {
		return errors.New("invalid schedule")
	}
	seen := map[int]bool{}
	prev := -1
	for _, h := range s.SlotHours {
		if h < 0 || h > 23 || seen[h] || h <= prev {
			return errors.New("slot hours must be sorted and unique")
		}
		seen[h] = true
		prev = h
	}
	if len(s.Targets) != 3 {
		return errors.New("three target scopes required")
	}
	seenScopes := map[string]bool{}
	for _, t := range s.Targets {
		if (t.Scope != "codex" && t.Scope != "gemini" && t.Scope != "third_party") || seenScopes[t.Scope] {
			return errors.New("invalid or duplicate target scope")
		}
		seenScopes[t.Scope] = true
		if t.Model == "" || strings.ContainsAny(t.Model, "/\r\n\t ") || len(t.Model) > 128 {
			return errors.New("invalid model")
		}
		names := map[string]bool{}
		for _, n := range t.Accounts {
			if n == "" || filepath.Base(n) != n || strings.ContainsAny(n, "\\/\r\n") || names[n] {
				return errors.New("invalid account allowlist")
			}
			names[n] = true
		}
	}
	return nil
}

const defaultTimezone = "Asia/Shanghai"

func validTimezone(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func (s Settings) loc() *time.Location {
	if l, err := time.LoadLocation(s.Timezone); err == nil && s.Timezone != "" {
		return l
	}
	return tz
}

func (s Settings) bufferMS() int64 {
	if s.BufferSeconds <= 0 {
		return 15000
	}
	return int64(s.BufferSeconds) * 1000
}

// withDefaults fills fields added after the first release so older saved
// files keep their original behavior (Shanghai, 15 second buffer).
func (s Settings) withDefaults() Settings {
	if s.Timezone == "" {
		s.Timezone = defaultTimezone
	}
	if s.BufferSeconds == 0 {
		s.BufferSeconds = 15
	}
	return s
}

// legacySettings accepts version 1 files so an upgrade never drops a live
// plan. A v1 account list applies to every scope, matching its former global
// semantics; an empty list keeps the former "all supported accounts" meaning.
type legacySettings struct {
	Version      int               `json:"version"`
	Enabled      bool              `json:"enabled"`
	SlotHours    []int             `json:"slot_hours"`
	GraceSeconds int               `json:"slot_grace_seconds"`
	CheckSeconds int               `json:"check_seconds"`
	Accounts     []string          `json:"accounts"`
	Models       map[string]string `json:"models"`
}

func (l legacySettings) migrate() Settings {
	targets := []Target{}
	for _, scope := range []string{"codex", "gemini", "third_party"} {
		targets = append(targets, Target{Scope: scope, Accounts: append([]string(nil), l.Accounts...), Model: l.Models[scope]})
	}
	return Settings{Version: 2, Enabled: l.Enabled, SlotHours: l.SlotHours, GraceSeconds: l.GraceSeconds, CheckSeconds: l.CheckSeconds, Timezone: defaultTimezone, BufferSeconds: 15, Targets: targets}
}

func decodeSettings(b []byte) (Settings, error) {
	return decodeSettingsVersion(b, true)
}

// decodeSettingsForWrite accepts only the current schema. Reading a v1 file is
// a one-way migration for upgrades; allowing a stale v1 page to write it back
// would flatten the three independent channel targets into one global list.
func decodeSettingsForWrite(b []byte) (Settings, error) {
	return decodeSettingsVersion(b, false)
}

func decodeSettingsVersion(b []byte, allowLegacy bool) (Settings, error) {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return Settings{}, errors.New("settings decode failed")
	}
	if probe.Version == 1 && !allowLegacy {
		return Settings{}, errors.New("legacy settings are read-only")
	}
	if probe.Version == 1 {
		var legacy legacySettings
		if err := strict(b, &legacy); err != nil || len(legacy.Models) != 3 {
			return Settings{}, errors.New("settings decode failed")
		}
		s := legacy.migrate()
		if err := s.validate(); err != nil {
			return Settings{}, err
		}
		return s, nil
	}
	var s Settings
	if err := strict(b, &s); err != nil {
		return Settings{}, errors.New("settings decode failed")
	}
	return s.withDefaults(), nil
}
func decodeBootstrap(b []byte) (Bootstrap, error) {
	var c Bootstrap
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if e := d.Decode(&c); e != nil {
		return c, errors.New("invalid bootstrap YAML")
	}
	var x any
	if d.Decode(&x) != io.EOF {
		return c, errors.New("extra YAML document")
	}
	if !filepath.IsAbs(c.DataDir) || !filepath.IsAbs(c.SecretFile) {
		return c, errors.New("absolute data_dir and secret_file required")
	}
	if c.SettingsFile == "" {
		c.SettingsFile = filepath.Join(c.DataDir, "settings.json")
	}
	if !filepath.IsAbs(c.SettingsFile) {
		return c, errors.New("settings_file must be absolute")
	}
	if e := validateURL(c.CPAURL, true); e != nil {
		return c, e
	}
	if e := validateURL(c.CPAMPURL, false); e != nil {
		return c, e
	}
	return c, nil
}
func validateURL(raw string, loopback bool) error {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Hostname() == "" {
		return errors.New("invalid service URL")
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	if loopback {
		if h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("CPA URL must be loopback")
		}
		return nil
	}
	if ip != nil {
		if !ip.IsLoopback() && !ip.IsPrivate() {
			return errors.New("CPAMP must be private network")
		}
		return nil
	}
	// Compose service names are single-label DNS names. Arbitrary external
	// domains (including lookalikes) are rejected instead of trusted by DNS.
	if len(h) > 63 || strings.Contains(h, ".") || strings.HasPrefix(h, "-") || strings.HasSuffix(h, "-") {
		return errors.New("CPAMP must use private IP or internal service name")
	}
	for _, r := range h {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			return errors.New("CPAMP must use private IP or internal service name")
		}
	}
	return nil
}
