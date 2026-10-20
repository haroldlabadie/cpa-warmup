package main

import (
	"bytes"
	"errors"
	"gopkg.in/yaml.v3"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strings"
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
	Version      int               `json:"version"`
	Enabled      bool              `json:"enabled"`
	SlotHours    []int             `json:"slot_hours"`
	GraceSeconds int               `json:"slot_grace_seconds"`
	CheckSeconds int               `json:"check_seconds"`
	Accounts     []string          `json:"accounts"`
	Models       map[string]string `json:"models"`
}
type Secrets struct {
	ManagementKey string `json:"management_key"`
	PrewarmKey    string `json:"prewarm_key"`
}

func defaults() Settings {
	return Settings{1, true, []int{5, 10, 15, 20}, 5400, 20, []string{}, map[string]string{"codex": "gpt-6-luna", "gemini": "gemini-3.8-flash-high", "third_party": "claude-sonnet-4-6"}}
}
func (s Settings) validate() error {
	if s.Version != 1 || s.GraceSeconds != 5400 || s.CheckSeconds < 5 || s.CheckSeconds > 300 || len(s.SlotHours) == 0 || len(s.SlotHours) > 24 {
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
	if len(s.Models) != 3 {
		return errors.New("three model scopes required")
	}
	for _, scope := range []string{"codex", "gemini", "third_party"} {
		m := s.Models[scope]
		if m == "" || strings.ContainsAny(m, "/\r\n\t ") || len(m) > 128 {
			return errors.New("invalid model")
		}
	}
	names := map[string]bool{}
	for _, n := range s.Accounts {
		if n == "" || filepath.Base(n) != n || strings.ContainsAny(n, "\\/\r\n") || names[n] {
			return errors.New("invalid account allowlist")
		}
		names[n] = true
	}
	return nil
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
