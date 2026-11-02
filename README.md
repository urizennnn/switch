# Switch

> **⚠️ Early development — Phase 2 planning foundation complete.**

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
| TUI slash commands | `/plan`, `/doctor`, `/init`, `/rules`, `/session`, `/lead`, `/new` |
| Config, rules, SQLite state | Working (TUI `/init`, `/doctor`; auto-init on first run) |
| Plan generation + run worktrees | Working (TUI `/plan`, lead-generated or `--file`) |
| Task execution, scheduler, review/merge | In progress (Phase 2; planning foundation done) |

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
go build -o switch .
./switch
```

`switch` is the primary interface. It auto-creates `.switch/config.toml` and
`.switch/rules.md` on first run in a project.

### TUI commands

| Command | Purpose |
|---|---|
| `/plan <goal>` | Ask the lead to plan and create a run with worktrees |
| `/plan --file <path>` | Import a plan JSON file |
| `/doctor` | Check config, rules, adapters, and state |
| `/init` | Create `.switch` config and rules explicitly |
| `/rules` | Show effective global/project rules |
| `/session` | Show the current agent session |
| `/lead <agent>` | Choose which fleet agent leads and persist it |
| `/new` | Start a new agent session |
| `/home` | Return to the dashboard |
| `/help` | Show commands |
| `/quit` | Exit |

The TUI uses one design system across the dashboard, conversation, modals, and
command palette: shared colors, panels, section labels, input, and status bars.
The dashboard is built from live state: project, lead, driver, model, session,
rules, fleet, latest run, task counts, commands, and keys. Use `/lead <agent>`
to choose any configured fleet member as the lead; the choice persists across
restarts. Slash commands operate over the dashboard without clearing it;
`/home` returns to it after chatting.

Type `/` to open the command palette. It filters as you type, shows
descriptions, and supports up/down, tab completion, and enter. Anything that
does not start with `/` is sent to the agent.

### Internal control

There are no user-facing CLI subcommands. The TUI is the product surface.
The unadvertised `__control` entry point remains for agents, automation, and
tests until the Unix-socket control plane lands:

```bash
./switch __control doctor
./switch __control plan --file plan.json
./switch __control probe --list
```

Resume a probe session by passing the session ID printed by a previous run:

```bash
./switch __control probe --agent opencode --session <session-id> \
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
- `internal/plan/` — plan schema and DAG validation
- `internal/worktree/` — git worktree creation and cleanup
- `internal/plancli/` — internal `plan` command; the TUI calls `internal/planning` directly

The `tui/` package is wired to the chat engine. `switch` opens the TUI; the hidden `__control` commands use the same engine for automation.

## Known limitations

- The TUI supports one resumable agent chat.
- `/plan` creates a run, agent worktrees, and persisted tasks; task execution and review are the next Phase 2 step.
- Task execution currently stops after planning; no worker has been dispatched yet.
- OpenClaw is untested because it is not installed on this machine.
- OpenCode's default model may hit provider privacy settings; use `--model` to choose a permitted model.
- Claude adapter hooks/plugins can add stderr noise; use `--agent-logs` only when debugging.
- The ACP SDK is v0.x; it is pinned and wrapped behind the driver boundary.

## License

No license file yet.
