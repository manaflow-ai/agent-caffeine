package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A lease is one file per agent session. The hooks of the agent CLI create and
// refresh it; its mtime is the last sign of work. Nothing scans the process
// table. Three independent things end a lease, so a crash cannot leak it:
//   - the Stop / SessionEnd hook removes it,
//   - the owning pid disappears,
//   - the mtime passes the TTL.
type Lease struct {
	ID    string
	PID   int
	Label string
	Age   time.Duration
}

func leaseDir() string { return filepath.Join(stateDir(), "leases") }

// safeID keeps a session id usable as one file name.
func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}

// resolveLease reads the identity from flags, then from the environment that
// the agent CLI gives its hook children, then falls back to the parent pid.
func resolveLease(args []string) (id string, pid int, labelOut string) {
	label := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--id":
			if i+1 < len(args) {
				id = args[i+1]
				i++
			}
		case "--pid":
			if i+1 < len(args) {
				pid, _ = strconv.Atoi(args[i+1])
				i++
			}
		case "--label":
			if i+1 < len(args) {
				label = args[i+1]
				i++
			}
		}
	}
	if id == "" {
		for _, key := range []string{
			"CLAUDE_SESSION_ID", "CODEX_SESSION_ID", "CODEX_THREAD_ID",
			"CURSOR_SESSION_ID", "AGENT_SESSION_ID",
		} {
			if v := os.Getenv(key); v != "" {
				id = v
				if label == "" {
					label = strings.ToLower(strings.TrimSuffix(key, "_SESSION_ID"))
				}
				break
			}
		}
	}
	if pid == 0 {
		for _, key := range []string{"CLAUDE_PID", "CODEX_PID"} {
			if v := os.Getenv(key); v != "" {
				pid, _ = strconv.Atoi(v)
				break
			}
		}
	}
	if pid == 0 {
		pid = os.Getppid()
	}
	if id == "" {
		id = "pid-" + strconv.Itoa(pid)
	}
	if label == "" {
		label = "agent"
	}
	return safeID(id), pid, label
}

// touchLease creates or refreshes one lease. Hooks call this on every event
// that means "work is happening", so it must stay cheap: one small write.
func touchLease(args []string) int {
	id, pid, label := resolveLease(args)
	if err := os.MkdirAll(leaseDir(), 0o755); err != nil {
		fmt.Println("{}")
		return 0 // never fail the hook, and never block the agent
	}
	body := fmt.Sprintf("%d\n%s\n", pid, label)
	_ = os.WriteFile(filepath.Join(leaseDir(), id), []byte(body), 0o644)
	fmt.Println("{}")
	return 0
}

func releaseLease(args []string) int {
	id, _, _ := resolveLease(args)
	_ = os.Remove(filepath.Join(leaseDir(), id))
	fmt.Println("{}")
	return 0
}

// activeLeases returns the live leases and deletes the dead ones. It reads one
// directory; the only process call is kill(pid, 0) on a pid a lease named.
func activeLeases(ttl time.Duration) []Lease {
	entries, err := os.ReadDir(leaseDir())
	if err != nil {
		return nil
	}
	var out []Lease
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(leaseDir(), e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		age := time.Since(info.ModTime())
		pid, label := 0, "agent"
		if raw, err := os.ReadFile(path); err == nil {
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) > 0 {
				pid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
			}
			if len(lines) > 1 {
				label = strings.TrimSpace(lines[1])
			}
		}
		if age > ttl {
			_ = os.Remove(path)
			logf("lease expired: %s (age %s)", e.Name(), age.Round(time.Second))
			continue
		}
		if pid > 0 && syscall.Kill(pid, 0) != nil {
			_ = os.Remove(path)
			logf("lease dropped: %s (pid %d gone)", e.Name(), pid)
			continue
		}
		out = append(out, Lease{ID: e.Name(), PID: pid, Label: label, Age: age})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Age < out[j].Age })
	return out
}
