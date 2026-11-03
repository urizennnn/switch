# Switch

> **⚠️ Early development — the v1 execution loop works end to end (plan → run → verify → review → approve → merge). Expect rough edges.**

Switch is a terminal orchestrator for a team of existing coding agents. It does not call models itself. It launches vendor CLIs as subprocesses, gives each one a role and an isolated workspace, and lets exactly one agent — the lead — plan, delegate, review, and talk to you. You converse only with the lead.

The design is vendor-agnostic and built on the [Agent Client Protocol](https://agentclientprotocol.com). OpenCode, Codex, Claude Code, and OpenClaw are first-class targets; any future ACP agent can be added as a configuration entry.

## Status

| Layer | State |
|---|---|
| ACP driver + probe | Working across OpenCode, Codex, and Claude; OpenClaw untested |
| TUI chat with the lead | Streaming, history, permissions, resume, lead switching |
| Config, rules, SQLite state | Working; global/project/local plus role and task rules |
| Planning | Lead-generated or imported plans become runs with an integration branch |
| Scheduler | Ready tasks run in parallel up to `max_concurrency`, one per worker |
| Verification | Task `verify` commands run in the task worktree; evidence is stored |
| Lead review + approval | The lead reviews each verified diff; you approve or reject |
| Merge queue | Serialized merges into the integration branch, then integration verification |
| Control plane | Per-run Unix socket, agent CLI, and MCP bridge offered to every agent |
| Reliability | Crash recovery, orphan adapter cleanup, audit log, token usage, budgets |
| Headless | `switch __control run|status|stop|tasks|approve|reject|retry|diff|log` |
| Detached runs | `/run --detach` or `__control run --detach`; survives the TUI; one process per run (lock) |
| Packaging | GoReleaser config and GitHub Actions CI/release workflows |

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

To run `switch` from any directory without building by hand, link the
launcher onto your PATH. It rebuilds this checkout when its sources change,
then runs Switch in the directory you are in:

```bash
ln -s "$PWD/scripts/switch" ~/.local/bin/switch
```

`switch` is the primary interface. It auto-creates `.switch/config.toml` and
`.switch/rules.md` on first run in a project.

### TUI commands

| Command | Purpose |
|---|---|
| `/plan <goal>` | Ask the lead to plan and create a run |
| `/plan --file <path>` | Import a plan JSON file |
| `/run` | Start or resume the run; workers stream into the run log |
| `/run --detach [--allow]` | Run in a background process that outlives the TUI; worker permissions are deny (or allow) |
| `/stop` | Stop the run; running tasks become `interrupted` and `/run` resumes them |
| `/tasks` | Task list: enter for details, `a` approve, `r` reject, `d` diff, `t` retry |
| `/review` | The task list, opened at the first task waiting for approval |
| `/approve <task>` | Merge a reviewed task into the integration branch |
| `/reject <task> <reason>` | Send a task back to its worker with your reason |
| `/retry <task>` | Requeue a blocked or interrupted task with a fresh attempt budget |
| `/diff <task>` | Show a task's change against the integration branch |
| `/agents` | Run agents, what each is doing, last errors, and token usage |
| `/log` | The run's audit log |
| `/doctor` | Check config, rules, adapters, and state |
| `/init` | Create `.switch` config and rules explicitly |
| `/rules` | Show effective global/project rules |
| `/session` | Show the current agent session |
| `/lead <agent>` | Choose which configured agent leads and persist it |
| `/adapters` | Install or uninstall ACP adapters (not during a run) |
| `/new` | Start a new session with an empty transcript |
| `/resume` | Pick a past session of this project and continue it with the agent that owned it |
| `/zen` | Hide or show the sidebar (also `ctrl+o`) |
| `/home` | Return to the dashboard |
| `/help` | Show commands |
| `/quit` | Exit |

### How a run works

1. `/plan` asks the lead for a JSON plan (or imports one) and creates a run
   with an integration branch `switch/<run>/integration` off your `HEAD`.
2. `/run` dispatches every ready task (dependencies merged, a free worker of
   the task's role) up to `max_concurrency`. Each task gets its own worktree,
   branched from the integration branch, so it sees its dependencies' work.
3. When the worker's turn ends, Switch runs the task's `verify` commands. A
   failure goes back to the same worker with the evidence, up to
   `max_attempts`; then the task is `blocked`.
4. Switch commits the change on the task branch, and the lead reviews the diff
   and evidence (`lead_review`). A lead REJECT sends it back like a failure.
5. The task waits in `review` for you. `/approve` merges it into the
   integration branch and re-runs verification there. A conflict blocks the
   task; a failed integration check undoes that task's merge and blocks it.
   Dependents start once their dependencies are merged.
6. When every task is merged, the run is `completed`. Switch never pushes and
   never merges into your branches; merge the integration branch yourself.

Chat with the lead stays available during a run. Run settings live under
`[run]` in `.switch/config.toml`; role and task rules in
`.switch/rules/roles/<role>.md` and `.switch/rules/tasks/<task>.md`.

The TUI uses one design system across the dashboard, conversation, modals, and
command palette: shared colors, panels, section labels, input, and status bars.
The dashboard is built from live state: project, lead, driver, model, session,
rules, configured agents, latest run, task counts, commands, and keys. Use
`/lead <agent>` to choose any configured agent as the lead; the choice persists across
restarts. Slash commands operate over the dashboard without clearing it;
`/home` returns to it after chatting.

The TUI opens on the dashboard immediately while the lead connects in the
background; a message sent before it is ready is delivered once it connects.
Lead replies render as markdown, tool calls show as one line each that
updates in place, and the agent's plan shows as a checklist.

Once the lead is connected, Switch starts the other configured agents in the
background, so `/lead` switches instantly and switching back keeps the old
session warm. The lead knows its team: each message carries a short team
note, and every agent session can call `switch_team` (who is on the team) and
`switch_ask` (consult a teammate and get its answer) through the Switch MCP
server, or `$SWITCH_BIN __control agent team|ask` from a shell. Adapter
installs run in the background while you keep chatting.

Type `/` to open the command palette. It filters as you type, shows
descriptions, and supports up/down, tab completion, and enter. Anything that
does not start with `/` is sent to the agent.

### Internal control

The TUI is the product surface. The unadvertised `__control` entry point is
for agents, automation, and tests; the run commands drive the same engine as
the TUI:

```bash
./switch __control plan --file plan.json
./switch __control run --permission allow   # until tasks wait for review
./switch __control run --detach --permission allow   # in the background
./switch __control status                   # who drives the run, task counts
./switch __control stop                     # stop a background run
./switch __control tasks
./switch __control diff <task>
./switch __control approve <task>           # or: reject <task> <reason>, retry <task>
./switch __control log
./switch __control doctor
./switch __control probe --list
```

Agents reach the run through the control plane: every agent session is
offered a `switch` MCP server (`switch_task`, `switch_progress`,
`switch_complete`, `switch_block`, and lead-only `switch_delegate`), and the
same calls work from a shell as `$SWITCH_BIN __control agent <method> <message>`.
Only you approve and merge; agents are refused.

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
3. **Phase 2 — Lead + workers** *(done)*: plan schema, control socket/CLI/MCP bridge, scheduler, worktrees, agent view.
4. **Phase 3 — Review + integration** *(done)*: verification, diff review, merge queue, retries, budgets.
5. **Phase 4 — Rules + context** *(done)*: global/project/local/role/task rules.
6. **Phase 5 — OpenClaw + ecosystem** *(done)*: `/adapters` install/uninstall verified with real npm installs; OpenClaw verified as a worker in a full run (bridge, gateway, model reply, tool call, verify, review, merge) with adapter quirks handled.
7. **Phase 6 — Reliability + distribution** *(done)*: crash recovery, orphan cleanup, run lock, detached runs, headless run, packaging.

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
- `internal/execution/` — run engine: scheduler, worker turns, verification, lead review, merge queue
- `internal/verify/` — verification command runner and evidence
- `internal/control/` — control socket, agent CLI, and MCP bridge
- `internal/runcli/` — headless run and task commands

The `tui/` package is wired to the chat engine. `switch` opens the TUI; the hidden `__control` commands use the same engine for automation.

## Known limitations

- One TUI chat with the lead per project; it spans lead switches, which are marked in the history.
- Headless and detached runs cannot ask for permissions: choose allow or deny up front.
- OpenClaw needs a running gateway and a model provider configured in OpenClaw itself; its bridge rejects per-session MCP servers, so OpenClaw workers report through `$SWITCH_BIN __control agent` only.
- Lead review needs a lead in the run; without one, review is left to you.
- OpenCode's default model may hit provider privacy settings; use `--model` to choose a permitted model.
- Claude adapter hooks/plugins can add stderr noise; use `--agent-logs` only when debugging.
- The ACP SDK is v0.x; it is pinned and wrapped behind the driver boundary.
- Windows is out of scope for v1.

## License

No license file yet.
