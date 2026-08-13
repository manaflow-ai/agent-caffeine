package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ------------------------------------------------------------------ logging

func logf(format string, args ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().Format("2006-01-02 15:04:05"),
		fmt.Sprintf(format, args...))
	_ = os.MkdirAll(stateDir(), 0o755)
	if fh, err := os.OpenFile(logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		_, _ = fh.WriteString(line)
		_ = fh.Close()
	}
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		fmt.Print(line)
	}
}

// ---------------------------------------------------------------- processes

// Proc is one matched agent process.
type Proc struct {
	PID     int
	CPU     time.Duration // total CPU time consumed so far
	Cmdline string
}

// parseCPUTime reads the ps TIME column: [[HH:]MM:]SS[.ss].
func parseCPUTime(raw string) time.Duration {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	mult := []float64{1, 60, 3600}
	var total float64
	for i := 0; i < len(parts) && i < 3; i++ {
		v, err := strconv.ParseFloat(parts[len(parts)-1-i], 64)
		if err != nil {
			return 0
		}
		total += v * mult[i]
	}
	return time.Duration(total * float64(time.Second))
}

type matcher struct {
	patterns []string
	args     []*regexp.Regexp
	excludes []*regexp.Regexp
}

func newMatcher(cfg Config) matcher {
	compile := func(list []string) []*regexp.Regexp {
		out := make([]*regexp.Regexp, 0, len(list))
		for _, s := range list {
			if rx, err := regexp.Compile(s); err == nil {
				out = append(out, rx)
			} else {
				logf("bad regex %q: %v", s, err)
			}
		}
		return out
	}
	return matcher{patterns: cfg.Patterns, args: compile(cfg.ArgPatterns), excludes: compile(cfg.ExcludeRegexes)}
}

// interpreters hide the real name in the first argument: gemini runs as
// "node .../@google/gemini-cli/bundle/gemini.js".
var interpreters = map[string]bool{
	"node": true, "bun": true, "deno": true, "tsx": true, "npx": true,
	"python": true, "python3": true, "ruby": true, "perl": true, "uv": true,
	"sh": true, "bash": true, "zsh": true,
}

var scriptExts = []string{".js", ".mjs", ".cjs", ".ts", ".py", ".rb", ".sh"}

// pathMatches tests one path against one pattern, by basename and by path
// component, because a launcher can hide the name: Claude Code runs as
// ~/.local/share/claude/versions/2.1.229, where only the component says
// "claude"; gemini's package directory is "gemini-cli".
func pathMatches(path, pat string) bool {
	base := filepath.Base(path)
	for _, ext := range scriptExts {
		base = strings.TrimSuffix(base, ext)
	}
	if base == pat || strings.HasPrefix(base, pat+"-") {
		return true
	}
	for _, c := range strings.Split(path, string(os.PathSeparator)) {
		if c == pat || strings.HasPrefix(c, pat+"-") {
			return true
		}
	}
	return false
}

func (m matcher) match(cmdline string) bool {
	for _, rx := range m.excludes {
		if rx.MatchString(cmdline) {
			return false
		}
	}
	fields := strings.Fields(cmdline)
	if len(fields) == 0 {
		return false
	}
	candidates := []string{fields[0]}
	// Only an interpreter earns a look at its script argument. A wider scan of
	// every argument would wake the Mac for "grep claude" or a file in ~/.claude.
	if interpreters[filepath.Base(fields[0])] {
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") {
				continue
			}
			candidates = append(candidates, f)
			break
		}
	}
	for _, pat := range m.patterns {
		for _, c := range candidates {
			if pathMatches(c, pat) {
				return true
			}
		}
	}
	for _, rx := range m.args {
		if rx.MatchString(cmdline) {
			return true
		}
	}
	return false
}

func scanAgents(m matcher) map[int]Proc {
	out, err := exec.Command("ps", "-axo", "pid=,time=,args=").Output()
	if err != nil {
		logf("ps failed: %v", err)
		return nil
	}
	self := os.Getpid()
	found := make(map[int]Proc)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		cmdline := strings.TrimSpace(line)
		// Drop the leading "pid time " prefix without splitting the arguments.
		if i := strings.Index(cmdline, fields[1]); i >= 0 {
			cmdline = strings.TrimSpace(cmdline[i+len(fields[1]):])
		}
		if pid == self || strings.Contains(cmdline, "agent-caffeine") {
			continue
		}
		if m.match(cmdline) {
			found[pid] = Proc{PID: pid, CPU: parseCPUTime(fields[1]), Cmdline: cmdline}
		}
	}
	return found
}

// ------------------------------------------------------------------ network

// probeNetwork completes a TLS handshake, not just a TCP connect. On this
// machine a local interceptor answers TCP for arbitrary addresses (203.0.113.1
// "connects" in 1ms), so a bare connect reports reachability that does not
// exist. A verified handshake proves a real path to the real host.
func probeNetwork(cfg Config) bool {
	timeout := time.Duration(cfg.NetProbeTimeoutSeconds * float64(time.Second))
	for _, target := range cfg.NetProbeHosts {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			host, target = target, target+":443"
		}
		conn, err := tls.DialWithDialer(
			&net.Dialer{Timeout: timeout}, "tcp", target,
			&tls.Config{ServerName: host},
		)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

func wifiDevice() string {
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(string(out), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "Wi-Fi") && !strings.Contains(line, "AirPort") {
			continue
		}
		for j := i + 1; j < len(lines) && j <= i+2; j++ {
			if strings.HasPrefix(lines[j], "Device:") {
				return strings.TrimSpace(strings.TrimPrefix(lines[j], "Device:"))
			}
		}
	}
	return ""
}

func wifiIsOn(device string) bool {
	if device == "" {
		return false
	}
	out, err := exec.Command("networksetup", "-getairportpower", device).Output()
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.TrimSpace(string(out)), "On")
}

func cycleWifi(device string) {
	logf("network down: cycling Wi-Fi on %s", device)
	for _, state := range []string{"off", "on"} {
		if err := exec.Command("networksetup", "-setairportpower", device, state).Run(); err != nil {
			logf("Wi-Fi %s failed: %v", state, err)
			return
		}
		if state == "off" {
			time.Sleep(3 * time.Second)
		}
	}
}

// ---------------------------------------------------------------- assertion

// Caffeinator owns at most one caffeinate child process.
type Caffeinator struct {
	flags []string
	cmd   *exec.Cmd
}

func (c *Caffeinator) held() bool {
	return c.cmd != nil && c.cmd.Process != nil && c.cmd.ProcessState == nil
}

func (c *Caffeinator) acquire(reason string) {
	if c.held() {
		return
	}
	cmd := exec.Command("caffeinate", c.flags...)
	if err := cmd.Start(); err != nil {
		logf("caffeinate failed to start: %v", err)
		return
	}
	c.cmd = cmd
	go func() { _ = cmd.Wait() }() // reap, so ProcessState is set on exit
	logf("awake  ON  (%s) pid=%d flags=%s", reason, cmd.Process.Pid, strings.Join(c.flags, " "))
}

func (c *Caffeinator) release(reason string) {
	if !c.held() {
		c.cmd = nil
		return
	}
	_ = c.cmd.Process.Kill()
	c.cmd = nil
	logf("awake  OFF (%s)", reason)
}
