# Switch

> **⚠️ Early development — Phase 1 chat shell complete.**

Switch is a terminal orchestrator for a fleet of existing coding agents. It does not call models itself. It launches vendor CLIs as subprocesses, gives each one a role and an isolated workspace, and lets exactly one agent — the lead — plan, delegate, review, and talk to you. You converse only with the lead.

The design is vendor-agnostic and built on the [Agent Client Protocol](https://agentclientprotocol.com). OpenCode, Codex, Claude Code, and OpenClaw are first-class targets; any future ACP agent can be added as a configuration entry.

## Status

| Layer | State |
|---|---|
| ACP driver + probe | Working across OpenCode, Codex, and Claude |
| Session create / prompt / resume | Working |
| Permission allow / deny bridge | Working (verified with Claude; interactive ask path implemented) |
| Capability report | Working |
| TUI chat shell | Wired to the engine with streaming, history, permissions, and resume |
| Config, rules, SQLite state | Working (`switch init`, `switch chat`, `switch doctor`) |
| Task engine, worktrees, lead/worker flow | Planned (Phase 2+) |

See [`docs/architecture.md`](docs/architecture.md) for the full architecture, language choice, phases, and risks.

## Requirements

- Go 1.26.2+ (the repo declares `go 1.26.2`; Go 1.27 is installed on the development machine)
- At least one ACP-capable agent:
  - **OpenCode** — `opencode` (native `opencode acp`)
  - **Codex** — `codex-acp` or `npx` with the pinned `@agentclientprotocol/codex-acp` package
  - **Claude Code** — `claude-agent-acp` or `npx` with the pinned `@agentclientprotocol/claude-agent-acp` package
  - **OpenClaw** — `openclaw` with a running gateway (`openclaw acp`)

The Codex and Claude adapters are Node processes. `npx` is used as a fallback; install the adapters globally for faster startup.

## Quick start

```bash
# Build
go build -o switch .

# Create project config and starter rules
./switch init

# Start the TUI (one resumable agent chat)
./switch

# Or use the line-based REPL
./switch chat

# Check config, rules, state, and adapters
./switch doctor

# See which adapters are available locally
./switch probe --list

# Verify the ACP connection without prompting
./switch probe --agent opencode --no-prompt

# Run one prompt through each vendor
./switch probe --agent opencode \
  --model opencode/deepseek-v4.1-flash-free \
  --prompt "Reply with exactly: switch probe ok"

./switch probe --agent codex --prompt "Reply with exactly: switch probe ok"
./switch probe --agent claude --prompt "Reply with exactly: switch probe ok"
```

Resume a session by passing the session ID printed by a previous run:

```bash
./switch probe --agent opencode --session <session-id> \
  --prompt "What did you just say? One short sentence."
```

### Probe flags

| Flag | Purpose |
|---|---|
| `--agent` | `opencode`, `codex`, `claude`, `openclaw`, or `custom` |
| `--command` | Custom adapter command for `--agent custom` (simple space-separated args) |
| `--cwd` | Working directory for the session |
| `--prompt` | Prompt to send |
| `--model` | Select a model session config value before prompting |
| `--session` | Resume an existing ACP session ID |
| `--permission` | `ask` (TTY prompt), `allow`, or `deny` |
| `--json` | Emit normalized JSONL events |
| `--verbose` | Show thoughts, tool status updates, plans, and available commands |
| `--agent-logs` | Stream adapter stderr to the terminal |
| `--no-prompt` | Connect and report capabilities only |
| `--timeout` | Prompt timeout (default 5m) |
| `--list` | List adapters and local availability |

### JSON events

`--json` emits one JSON object per line:

```json
{"type":"capabilities","agent":"opencode","protocol":1,...}
{"type":"session","id":"ses_...","resumed":false,"configOptions":[...]}
{"type":"agent_message","text":"switch probe ok"}
{"type":"tool_call","id":"tool-1","title":"Read file","status":"in_progress"}
{"type":"permission","title":"Write file","options":[...]}
{"type":"permission_decision","tool":"tool-1","decision":"deny"}
{"type":"usage","used":11208,"size":200000}
{"type":"turn","stopReason":"end_turn","usage":{...}}
```

This is the event shape the TUI consumes.

## Design in one paragraph

Switch runs a modular Go monolith. The engine owns runs, tasks, agents, rules, worktrees, verification, and review; the TUI is a client of that engine. Agents are persistent ACP subprocesses, one per agent instance, wrapped by a thin driver around [`coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk). The lead is the only agent you talk to and the only one allowed to delegate. Workers get isolated git worktrees and report structured results; Switch verifies them before the lead reviews and approves a merge.

## Roadmap

1. **Phase 0 — ACP spine** *(done)*: probe OpenCode, Codex, Claude; capability matrix; permissions; resume.
2. **Phase 1 — Engine + chat shell** *(done)*: config, SQLite state, session manager, TUI chat, `/rules`, doctor.
3. **Phase 2 — Lead + workers**: plan schema, control socket/CLI/MCP bridge, scheduler, worktrees, fleet view.
4. **Phase 3 — Review + integration**: verification, diff review, merge queue, retries, budgets.
5. **Phase 4 — Rules + context**: global/project/role rules, effective-rules view, product context docs.
6. **Phase 5 — OpenClaw + ecosystem**: ACP bridge, adapter install/doctor, capability fallbacks.
7. **Phase 6 — Reliability + distribution**: crash recovery, headless `run`, daemon/detach, packaging.

## Development

```bash
go build ./...
go test ./...
go vet ./...
```

The probe implementation lives in:

- `internal/driver/acpdriver/adapter.go` — adapter registry and local availability
- `internal/driver/acpdriver/client.go` — `acp.Client` bridge and permission policy
- `internal/driver/acpdriver/probe.go` — lifecycle, capabilities, prompt, resume
- `internal/probe/probe.go` — CLI flags

The `tui/` package is now wired to the chat engine. `switch` opens the TUI; `switch chat` provides the same engine as a line-based REPL.

## Known limitations

- The TUI supports one resumable agent chat. Lead/worker orchestration, task state, and worktrees are Phase 2 work.
- OpenClaw is untested because it is not installed on this machine.
- OpenCode's default model may hit provider privacy settings; use `--model` to choose a permitted model.
- Claude adapter hooks/plugins can add stderr noise; use `--agent-logs` only when debugging.
- The ACP SDK is v0.x; it is pinned and wrapped behind the driver boundary.

## License

No license file yet.
