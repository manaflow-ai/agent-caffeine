package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

const label = "com.local.agent-caffeine"

// Config controls process matching, the busy heuristic, and network recovery.
type Config struct {
	// Process name / path-component patterns that identify an agent CLI.
	Patterns []string `json:"patterns"`
	// Extra regexes matched against the full command line.
	ArgPatterns []string `json:"arg_patterns"`
	// Regexes that drop a match again (helper processes you never wake for).
	ExcludeRegexes []string `json:"exclude_regexes"`
	// "lease"   trusts hook-written lease files only; never reads the process table.
	// "process" scans processes and judges by CPU (no hooks needed).
	Detection string `json:"detection"`
	// A lease counts as live until its mtime is this old. It covers a long model
	// turn that fires no hook, and it heals a session that died without a Stop.
	LeaseTTLSeconds int `json:"lease_ttl_seconds"`

	// Used by detection "process" only.
	// "busy" keeps the Mac awake only while an agent uses CPU (recommended).
	// "present" keeps it awake whenever an agent process exists.
	Mode        string `json:"mode"`
	PollSeconds int    `json:"poll_seconds"`
	// Fraction of one core, for a SINGLE process, that counts as working.
	// Measured on this machine: idle session <= 0.003, working session 0.06-0.13.
	CPUBusyRatio float64 `json:"cpu_busy_ratio"`
	// Stay awake this long after the last busy sample.
	IdleReleaseSeconds int `json:"idle_release_seconds"`

	NetProbeHosts          []string `json:"net_probe_hosts"`
	NetProbeTimeoutSeconds float64  `json:"net_probe_timeout_seconds"`
	// Release the assertion (allow sleep) after the network is down this long.
	NetDownGraceSeconds int `json:"net_down_grace_seconds"`

	// Wi-Fi recovery acts only when Wi-Fi power is already on.
	WifiRecovery              bool `json:"wifi_recovery"`
	WifiToggleAfterFailures   int  `json:"wifi_toggle_after_failures"`
	WifiToggleCooldownSeconds int  `json:"wifi_toggle_cooldown_seconds"`

	// -i idle sleep, -m disk, -s system sleep (AC only). Add "-d" to hold the display on.
	CaffeinateFlags []string `json:"caffeinate_flags"`
}

func defaultConfig() Config {
	return Config{
		Patterns: []string{
			"claude", "codex", "cursor-agent", "aider", "gemini",
			"opencode", "goose", "amp", "crush",
		},
		ArgPatterns:               []string{},
		ExcludeRegexes:            []string{},
		Detection:                 "lease",
		LeaseTTLSeconds:           600,
		Mode:                      "busy",
		PollSeconds:               15,
		CPUBusyRatio:              0.02,
		IdleReleaseSeconds:        300,
		NetProbeHosts:             []string{"api.anthropic.com:443", "api.openai.com:443", "1.1.1.1:443"},
		NetProbeTimeoutSeconds:    4,
		NetDownGraceSeconds:       600,
		WifiRecovery:              true,
		WifiToggleAfterFailures:   3,
		WifiToggleCooldownSeconds: 300,
		CaffeinateFlags:           []string{"-i", "-m", "-s"},
	}
}

func home() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return os.Getenv("HOME")
	}
	return dir
}

func configPath() string { return filepath.Join(home(), ".config", "agent-caffeine", "config.json") }
func stateDir() string   { return filepath.Join(home(), ".local", "state", "agent-caffeine") }
func statePath() string  { return filepath.Join(stateDir(), "state.json") }
func logPath() string    { return filepath.Join(stateDir(), "agent-caffeine.log") }
func pidPath() string    { return filepath.Join(stateDir(), "daemon.pid") }
func plistPath() string {
	return filepath.Join(home(), "Library", "LaunchAgents", label+".plist")
}

// loadConfig starts from the defaults and overlays the config file. Absent keys
// keep their default, so a partial config file is valid.
func loadConfig() Config {
	cfg := defaultConfig()
	raw, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		logf("config error, using defaults: %v", err)
		return defaultConfig()
	}
	if cfg.PollSeconds < 1 {
		cfg.PollSeconds = 1
	}
	return cfg
}

func writeDefaultConfig() (bool, error) {
	if _, err := os.Stat(configPath()); err == nil {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o755); err != nil {
		return false, err
	}
	raw, err := json.MarshalIndent(defaultConfig(), "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(configPath(), append(raw, '\n'), 0o644)
}
