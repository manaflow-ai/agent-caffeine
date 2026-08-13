package main

import "testing"

func TestMatch(t *testing.T) {
	m := newMatcher(defaultConfig())

	// Real command lines seen on this machine, plus the interpreter-launched
	// forms that a bare basename check would miss.
	want := []string{
		"/Users/lawrence/.local/bin/claude --session-id abc --settings {}",
		"/Users/lawrence/.local/share/claude/versions/2.1.229 --resume",
		"/var/folders/rr/T/cmux-cli-shims/ABC/claude",
		"claude --dangerously-skip-permissions",
		"/Users/lawrence/.local/bin/codex --enable hooks -c hooks.SessionStart=[]",
		"/Applications/ChatGPT.app/Contents/Resources/codex app-server --listen stdio://",
		"codex --yolo",
		"/Users/lawrence/.local/bin/cursor-agent --print",
		"/Users/lawrence/.local/share/cursor-agent/versions/2026.08.04/node index.js",
		"node /Users/lawrence/.bun/install/global/node_modules/@google/gemini-cli/bundle/gemini.js --yolo",
		"/Users/lawrence/.bun/bin/opencode run",
		"/Users/lawrence/.local/bin/amp -x 'do the thing'",
		"bun /Users/lawrence/src/opencode/index.ts",
	}
	for _, cmd := range want {
		if !m.match(cmd) {
			t.Errorf("should match but did not:\n  %s", cmd)
		}
	}

	// These must never hold the Mac awake.
	reject := []string{
		"tail -f /Users/lawrence/.claude/debug/latest.txt",
		"grep -r claude /Users/lawrence/src",
		"/usr/bin/vim /Users/lawrence/.claude/settings.json",
		"node /Users/lawrence/src/webapp/server.js",
		"/Applications/Safari.app/Contents/MacOS/Safari",
		"ps -axo pid=,time=,args=",
		"/opt/homebrew/bin/rg codex",
	}
	for _, cmd := range reject {
		if m.match(cmd) {
			t.Errorf("should NOT match but did:\n  %s", cmd)
		}
	}
}

func TestExcludeRegexes(t *testing.T) {
	cfg := defaultConfig()
	cfg.ExcludeRegexes = []string{`codex sandbox`}
	m := newMatcher(cfg)
	if m.match(`/Applications/ChatGPT.app/Contents/Resources/codex sandbox -c policy="all"`) {
		t.Error("exclude_regexes did not drop the sandbox helper")
	}
	if !m.match("/Users/lawrence/.local/bin/codex --yolo") {
		t.Error("exclude_regexes dropped too much")
	}
}

func TestParseCPUTime(t *testing.T) {
	cases := map[string]float64{
		"0:05.12":  5.12,
		"1:02.00":  62,
		"10:00.00": 600,
		"1:00:00":  3600,
		"":         0,
	}
	for raw, want := range cases {
		if got := parseCPUTime(raw).Seconds(); got != want {
			t.Errorf("parseCPUTime(%q) = %v, want %v", raw, got, want)
		}
	}
}
