# agent-caffeine

Keep macOS awake while a CLI coding agent is working — and let it sleep when the work cannot progress.

Long agent runs die when the Mac sleeps mid-stream. The API connection goes quiet, the client watchdog fires, and the agent stops with something like `API Error: Response stalled mid-stream`. `caffeinate` alone fixes that by never letting the machine sleep, which costs battery for every idle session you leave open.

`agent-caffeine` holds the assertion only while an agent actually works, and drops it when the network has been down long enough that staying awake is pointless.

```
$ agent-caffeine status
updated       2026-08-13 10:14:02
awake held    true
network ok    true
detection     lease
leases        2 live
  1a6d741a-7775-4753-a10e-e6c7d27e9b7e     claude pid=58707 age=6s
  01998c4f-0e5f-7a91-b0d2-3f1a2c7d8e44     codex  pid=72076 age=41s
```

## How it decides

A **lease** is one file per agent session under `~/.local/state/agent-caffeine/leases/`. Your agent's own hooks create and refresh it. The daemon reads that one directory — it never scans the process table.

A lease dies three ways, so a crashed session cannot leak the assertion and pin your Mac awake:

1. the `Stop` / `SessionEnd` hook removes it,
2. the owning pid disappears,
3. its mtime passes the TTL (default 600 s), which also covers a long model turn that fires no hook.

Every 15 s the daemon reads the leases, probes the network, and decides:

| Condition | Action |
|---|---|
| A live lease exists | Hold `caffeinate -i -m -s` |
| No live lease | Release — the Mac may sleep |
| TLS probe fails 3 times | Cycle Wi-Fi, at most once per 5 min, only if Wi-Fi power is on |
| Network down for 10 min | Release — do not burn battery on work that cannot progress |
| Network returns | Hold again, if a lease is live |

## Install

```sh
go build -o ~/.local/bin/agent-caffeine .
agent-caffeine install      # writes the config and the launchd agent, then starts it
```

`install` is idempotent. It writes `~/.config/agent-caffeine/config.json` (only if absent) and
`~/Library/LaunchAgents/com.local.agent-caffeine.plist`, then bootstraps the job.

## Wire your agent

Any tool that can run a command on session events works. The command must be cheap — it writes one small file.

```
touch    on SessionStart, UserPromptSubmit, PreToolUse, PostToolUse
release  on Stop, SessionEnd
```

**Claude Code** — `~/.claude/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      { "hooks": [ { "type": "command", "timeout": 2,
        "command": "agent-caffeine touch --label claude 2>/dev/null||echo {}" } ] }
    ],
    "Stop": [
      { "hooks": [ { "type": "command", "timeout": 2,
        "command": "agent-caffeine release --label claude 2>/dev/null||echo {}" } ] }
    ]
  }
}
```

**Codex** — the same shape in `~/.codex/hooks.json`.

`touch` and `release` take `--id`, `--pid` and `--label`. Without them they read `CLAUDE_SESSION_ID` /
`CODEX_SESSION_ID` and fall back to the parent pid, so a tool that exports neither still gets one lease
per process.

## Commands

| Command | Purpose |
|---|---|
| `touch` | take or refresh this session's lease |
| `release` | drop this session's lease |
| `status` | what the daemon decided at the last poll |
| `doctor` | one-shot check: leases, Wi-Fi, network, daemon |
| `install` / `uninstall` | manage the launchd agent |
| `run` | the poll loop itself (launchd runs this) |

## Configuration

`~/.config/agent-caffeine/config.json`. Absent keys keep their default.

| Key | Default | Meaning |
|---|---|---|
| `detection` | `lease` | `lease`, or `process` to judge by CPU instead (see below) |
| `lease_ttl_seconds` | `600` | a lease counts as live until its mtime is this old |
| `poll_seconds` | `15` | loop interval |
| `net_probe_hosts` | Anthropic, OpenAI, `1.1.1.1` | probed in order, first success wins |
| `net_down_grace_seconds` | `600` | release the assertion after the network is down this long |
| `wifi_recovery` | `true` | cycle Wi-Fi while the network is down |
| `caffeinate_flags` | `["-i","-m","-s"]` | add `-d` to hold the display on too |

### Process detection

Set `"detection": "process"` if you do not want to wire hooks. The daemon then runs one `ps` per poll,
matches agent CLIs by name, and treats a process as working when it uses at least `cpu_busy_ratio` of one
core (default 0.02).

Two details this mode gets right, both measured rather than guessed:

- The CPU test is **per process**, not summed. An idle agent session sits at or below 0.003 cores while a
  working one runs 0.06–0.13. Summing across many open sessions produces false positives.
- Matching reads path components, not just the basename, because launchers hide the name: Claude Code runs
  as `~/.local/share/claude/versions/2.1.229`, and gemini runs as `node .../gemini-cli/bundle/gemini.js`.
  For a known interpreter the matcher also reads the first non-flag argument — and only that one, so
  `grep claude` or a file open in `~/.claude` never wakes the machine.

## Limits

- `caffeinate` cannot prevent clamshell sleep. Closing the lid on battery still sleeps the Mac.
- The network probe completes a **TLS handshake**, not a bare TCP connect. Some machines run a local
  interceptor that accepts TCP for any address, which makes a connect-only probe report reachability that
  does not exist.

## License

MIT
