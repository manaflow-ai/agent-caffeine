// agent-caffeine keeps macOS awake while a CLI coding agent is working.
//
// It holds a caffeinate assertion while a matched agent process (claude, codex,
// ...) burns CPU, and drops it when the agent goes idle. It probes network
// reachability; on failure it retries and can cycle Wi-Fi. If the network stays
// down past a grace period it releases the assertion, so the machine may sleep
// instead of staying awake for work that cannot progress.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// State is the snapshot written every poll, for `agent-caffeine status`.
type State struct {
	Updated            string            `json:"updated"`
	Awake              bool              `json:"awake"`
	Detection          string            `json:"detection"`
	Leases             map[string]string `json:"leases"`
	Agents             map[string]string `json:"agents"`
	BusyAgents         map[string]string `json:"busy_agents"`
	PeakCPURatio       float64           `json:"peak_cpu_ratio"`
	Busy               bool              `json:"busy"`
	SecondsSinceBusy   int               `json:"seconds_since_busy"`
	NetworkOK          bool              `json:"network_ok"`
	NetworkDownSeconds int               `json:"network_down_seconds"`
	NetworkGaveUp      bool              `json:"network_gave_up"`
	WifiDevice         string            `json:"wifi_device"`
}

func writeState(s State) {
	_ = os.MkdirAll(stateDir(), 0o755)
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, statePath())
}

func daemonRunning() (int, bool) {
	raw, err := os.ReadFile(pidPath())
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return pid, false
	}
	return pid, true
}

// ---------------------------------------------------------------------- run

func run() int {
	cfg := loadConfig()
	_ = os.MkdirAll(stateDir(), 0o755)

	if pid, ok := daemonRunning(); ok {
		fmt.Fprintf(os.Stderr, "already running (pid %d)\n", pid)
		return 1
	}
	_ = os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())), 0o644)
	defer os.Remove(pidPath())

	m := newMatcher(cfg)
	caff := &Caffeinator{flags: cfg.CaffeinateFlags}
	defer caff.release("daemon stop")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	logf("daemon start pid=%d mode=%s poll=%ds", os.Getpid(), cfg.Mode, cfg.PollSeconds)

	var (
		prevCPU        = map[int]time.Duration{}
		prevSample     = time.Now()
		lastBusy       time.Time
		netDownSince   time.Time
		netFailures    int
		lastWifiToggle time.Time
		wifiDev        = wifiDevice()
		prevNetOK      = true
	)

	ticker := time.NewTicker(time.Duration(cfg.PollSeconds) * time.Second)
	defer ticker.Stop()

	for {
		now := time.Now()
		elapsed := now.Sub(prevSample).Seconds()
		if elapsed <= 0 {
			elapsed = 0.001
		}
		prevSample = now

		// --- busy detection
		var leases []Lease
		agents := map[int]Proc{}
		ratios := map[int]float64{}
		peak := 0.0
		leaseMode := cfg.Detection != "process"
		if leaseMode {
			leases = activeLeases(time.Duration(cfg.LeaseTTLSeconds) * time.Second)
		} else {
			agents = scanAgents(m)
		}
		// Per process, so one worker outweighs many idlers (process mode only).
		for pid, p := range agents {
			prev, seen := prevCPU[pid]
			if !seen {
				continue // first sighting: establish a baseline, judge next poll
			}
			r := (p.CPU - prev).Seconds() / elapsed
			if r < 0 {
				r = 0
			}
			ratios[pid] = r
			if r > peak {
				peak = r
			}
		}
		prevCPU = map[int]time.Duration{}
		for pid, p := range agents {
			prevCPU[pid] = p.CPU
		}

		var busyPIDs []int
		for pid, r := range ratios {
			if r >= cfg.CPUBusyRatio {
				busyPIDs = append(busyPIDs, pid)
			}
		}
		sort.Ints(busyPIDs)
		busy := len(busyPIDs) > 0
		switch {
		case leaseMode:
			busy = len(leases) > 0
		case cfg.Mode == "present":
			busy = len(agents) > 0
		}
		if busy {
			lastBusy = now
		}

		// --- network probe and recovery
		netOK := probeNetwork(cfg)
		if netOK {
			if !prevNetOK {
				logf("network back up")
			}
			netDownSince = time.Time{}
			netFailures = 0
		} else {
			netFailures++
			if netDownSince.IsZero() {
				netDownSince = now
				logf("network unreachable")
			}
			if cfg.WifiRecovery && netFailures >= cfg.WifiToggleAfterFailures &&
				now.Sub(lastWifiToggle) >= time.Duration(cfg.WifiToggleCooldownSeconds)*time.Second {
				if wifiDev == "" {
					wifiDev = wifiDevice()
				}
				switch {
				case wifiIsOn(wifiDev):
					cycleWifi(wifiDev)
					lastWifiToggle = now
				case wifiDev != "":
					logf("Wi-Fi is off by choice: no recovery attempt")
				}
			}
		}
		prevNetOK = netOK

		netDownFor := time.Duration(0)
		if !netDownSince.IsZero() {
			netDownFor = now.Sub(netDownSince)
		}
		netGaveUp := netDownFor >= time.Duration(cfg.NetDownGraceSeconds)*time.Second
		// In lease mode the TTL is the grace, so a Stop hook releases at once.
		recentlyBusy := busy
		if !leaseMode {
			recentlyBusy = !lastBusy.IsZero() &&
				now.Sub(lastBusy) <= time.Duration(cfg.IdleReleaseSeconds)*time.Second
		}

		reason := fmt.Sprintf("%d busy of %d, peak cpu=%.2f", len(busyPIDs), len(agents), peak)
		if leaseMode {
			reason = fmt.Sprintf("%d lease(s), newest %s", len(leases), leaseAge(leases))
		}

		// --- hold or drop the assertion
		switch {
		case recentlyBusy && !netGaveUp:
			caff.acquire(reason)
		case caff.held() && netGaveUp:
			caff.release("network down too long")
		case caff.held():
			if leaseMode {
				caff.release("no live lease")
			} else {
				caff.release("agents idle")
			}
		}

		st := State{
			Updated:            now.Format("2006-01-02 15:04:05"),
			Awake:              caff.held(),
			Detection:          cfg.Detection,
			Leases:             map[string]string{},
			Agents:             map[string]string{},
			BusyAgents:         map[string]string{},
			PeakCPURatio:       float64(int(peak*1000)) / 1000,
			Busy:               busy,
			SecondsSinceBusy:   -1,
			NetworkOK:          netOK,
			NetworkDownSeconds: int(netDownFor.Seconds()),
			NetworkGaveUp:      netGaveUp,
			WifiDevice:         wifiDev,
		}
		if !lastBusy.IsZero() {
			st.SecondsSinceBusy = int(now.Sub(lastBusy).Seconds())
		}
		for _, l := range leases {
			st.Leases[l.ID] = fmt.Sprintf("%s pid=%d age=%s", l.Label, l.PID, l.Age.Round(time.Second))
		}
		for pid, p := range agents {
			st.Agents[strconv.Itoa(pid)] = p.Cmdline
		}
		for _, pid := range busyPIDs {
			st.BusyAgents[strconv.Itoa(pid)] = agents[pid].Cmdline
		}
		writeState(st)

		select {
		case <-stop:
			caff.release("daemon stop")
			logf("daemon stop")
			return 0
		case <-ticker.C:
		}
	}
}

// ------------------------------------------------------------- subcommands

func status() int {
	raw, err := os.ReadFile(statePath())
	if err != nil {
		fmt.Println("no state yet — is the daemon running? (agent-caffeine install)")
		return 1
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		fmt.Fprintf(os.Stderr, "unreadable state: %v\n", err)
		return 1
	}
	fmt.Printf("updated       %s\n", s.Updated)
	fmt.Printf("awake held    %v\n", s.Awake)
	net := "true"
	if !s.NetworkOK {
		net = fmt.Sprintf("false  (down %ds", s.NetworkDownSeconds)
		if s.NetworkGaveUp {
			net += ", gave up — sleep allowed)"
		} else {
			net += ")"
		}
	}
	fmt.Printf("network ok    %s\n", net)
	fmt.Printf("detection     %s\n", s.Detection)
	if s.Detection == "process" {
		fmt.Printf("peak cpu      %.3f cores  busy=%v  idle_for=%ds\n",
			s.PeakCPURatio, s.Busy, s.SecondsSinceBusy)
		fmt.Printf("agents        %d matched, %d working\n", len(s.Agents), len(s.BusyAgents))
		for pid, argv := range s.BusyAgents {
			fmt.Printf("  %7s  %s\n", pid, trim(argv, 110))
		}
		return 0
	}
	fmt.Printf("leases        %d live\n", len(s.Leases))
	ids := make([]string, 0, len(s.Leases))
	for id := range s.Leases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("  %-40s %s\n", trim(id, 40), s.Leases[id])
	}
	return 0
}

func leaseAge(leases []Lease) string {
	if len(leases) == 0 {
		return "none"
	}
	return leases[0].Age.Round(time.Second).String()
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func install() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find own path: %v\n", err)
		return 1
	}
	self, _ = filepath.EvalSymlinks(self)
	_ = os.MkdirAll(stateDir(), 0o755)
	if wrote, err := writeDefaultConfig(); err != nil {
		fmt.Fprintf(os.Stderr, "config write failed: %v\n", err)
	} else if wrote {
		fmt.Printf("wrote default config %s\n", configPath())
	}

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, label, self, filepath.Join(stateDir(), "launchd.out.log"), filepath.Join(stateDir(), "launchd.err.log"))

	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if err := os.WriteFile(plistPath(), []byte(plist), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	// bootout returns before the service is gone. An immediate bootstrap then
	// fails with "5: Input/output error", so wait for the teardown and retry.
	for i := 0; i < 20; i++ {
		if err := exec.Command("launchctl", "print", domain+"/"+label).Run(); err != nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	var out []byte
	for i := 0; i < 5; i++ {
		out, err = exec.Command("launchctl", "bootstrap", domain, plistPath()).CombinedOutput()
		if err == nil {
			fmt.Printf("installed and started %s\n", label)
			return 0
		}
		time.Sleep(time.Second)
	}
	fmt.Fprintf(os.Stderr, "bootstrap failed: %v %s\n", err, strings.TrimSpace(string(out)))
	return 1
}

func uninstall() int {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	_ = os.Remove(plistPath())
	fmt.Printf("removed %s\n", label)
	return 0
}

func doctor() int {
	cfg := loadConfig()
	src := configPath()
	if _, err := os.Stat(src); err != nil {
		src = "(defaults)"
	}
	fmt.Printf("config        %s\n", src)
	fmt.Printf("detection     %s\n", cfg.Detection)
	if cfg.Detection == "process" {
		m := newMatcher(cfg)
		agents := scanAgents(m)
		fmt.Printf("patterns      %s\n", strings.Join(cfg.Patterns, ", "))
		fmt.Printf("matched       %d process(es)\n", len(agents))
		pids := make([]int, 0, len(agents))
		for pid := range agents {
			pids = append(pids, pid)
		}
		sort.Ints(pids)
		for _, pid := range pids {
			fmt.Printf("  %7d  cpu=%8.1fs  %s\n", pid, agents[pid].CPU.Seconds(), trim(agents[pid].Cmdline, 100))
		}
	} else {
		fmt.Printf("lease dir     %s (ttl %ds)\n", leaseDir(), cfg.LeaseTTLSeconds)
		leases := activeLeases(time.Duration(cfg.LeaseTTLSeconds) * time.Second)
		fmt.Printf("leases        %d live\n", len(leases))
		for _, l := range leases {
			fmt.Printf("  %-40s %s pid=%d age=%s\n", trim(l.ID, 40), l.Label, l.PID, l.Age.Round(time.Second))
		}
	}
	dev := wifiDevice()
	fmt.Printf("wifi device   %s (power on: %v)\n", dev, wifiIsOn(dev))
	t0 := time.Now()
	ok := probeNetwork(cfg)
	label := "reachable"
	if !ok {
		label = "UNREACHABLE"
	}
	fmt.Printf("network       %s in %.2fs\n", label, time.Since(t0).Seconds())
	pid, running := daemonRunning()
	if running {
		fmt.Printf("daemon        running (pid %d)\n", pid)
	} else {
		fmt.Printf("daemon        not running\n")
	}
	return 0
}

func usage() int {
	fmt.Println(`agent-caffeine — keep macOS awake while a CLI coding agent works

  touch       take or refresh this session's lease (agent hooks call this)
  release     drop this session's lease
  run         poll loop (launchd runs this)
  status      what the daemon decided at the last poll
  doctor      one-shot check: leases or processes, Wi-Fi, network, daemon
  install     write the config plus the launchd agent, then start it
  uninstall   stop and remove the launchd agent

touch and release take --id, --pid and --label. Without them they read
CLAUDE_SESSION_ID / CODEX_SESSION_ID and the parent pid.`)
	return 2
}

func main() {
	cmd := "status"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "touch":
		os.Exit(touchLease(os.Args[2:]))
	case "release":
		os.Exit(releaseLease(os.Args[2:]))
	case "run":
		os.Exit(run())
	case "status":
		os.Exit(status())
	case "doctor":
		os.Exit(doctor())
	case "install":
		os.Exit(install())
	case "uninstall":
		os.Exit(uninstall())
	default:
		os.Exit(usage())
	}
}
