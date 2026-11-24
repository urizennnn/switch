# Switch — System Architecture & Language Choice

Status: Draft for build  
Date: 2026-11-02  
Scope: v1 — git repositories, one lead + N workers, OpenCode / Codex / Claude first

Sections 1–15 record the design; section 18 describes the system as built and
wins where they differ.

---

## 0. Locked decisions

| Decision | Choice |
|---|---|
| V1 scope | Coding repositories with git |
| Lead behavior | Plan and review; implements only a task handed off to it |
| First vendors | OpenCode, Codex, Claude Code |
| Lead | Exactly one per run; the agent the user converses with (`@<name>` asks a teammate directly) |
| Isolation | One git worktree per task, branched from the run integration branch |
| Integration | Serialized merge queue into a run integration branch |
| Rules | Global, project, project-local, role, task scopes |
| Language | Go 1.26.2+ (Go 1.27 toolchain used locally) |

Non-goals for v1: direct model APIs, PTY/screen scraping, remote/cloud execution, autonomous push/PR creation, embedding-based memory, Windows.

---

## 1. Language choice: Go

**Decision: Go for the entire Switch core, orchestration engine, CLI, MCP bridge, and TUI. One static binary.** The module declares Go 1.26.2 for compatibility; the development machine currently uses Go 1.27.3.

The core job of Switch is supervising long-lived subprocesses, streaming their output, cancelling them, and keeping durable state while several agents run concurrently. That is Go's strongest area.

| Requirement | Why Go fits |
|---|---|
| Spawn and supervise 3–6 persistent agent processes | `os/exec`, pipes, `context`, goroutines, clean signal handling |
| Stream concurrent agent output without blocking the TUI | One goroutine per session event pump; channels and `select` |
| Terminal UI | Bubble Tea v2 + Lip Gloss are already in the repo and are the most mature Go TUI stack |
| Single-binary distribution | `go build` produces one binary; no Node/Python runtime for the core |
| Local control socket / JSON-RPC / MCP | Standard library is enough; no framework needed |
| Cross-platform builds | macOS/Linux now, Windows later, from one codebase |
| Iteration speed | Fast compile, simple concurrency, easy testing with interfaces |

### Trade-offs accepted

- **Go's type system has no sum types.** ACP unions are handled by the SDK's generated union wrappers, not hand-rolled tagged structs.
- **Codex and Claude adapters are Node processes.** They are external subprocesses, not linked into Switch. OpenCode and OpenClaw are native binaries. Switch itself has no Node dependency.
- **The ACP SDK is v0.x.** Pin it, wrap it behind a driver interface, and keep a conformance suite. Do not let SDK types leak into the orchestrator.

### Why not the alternatives

| Language | Verdict |
|---|---|
| TypeScript/Node | Best ACP SDK support, but a TUI plus process supervisor plus single-binary distribution is weaker. You would ship a Node runtime for the core while Node adapters already run separately. |
| Rust | Excellent correctness and process control, but slower iteration for a state-heavy orchestrator, and the TUI ecosystem is less integrated than Bubble Tea. Better fit for a later performance-critical component than the whole product. |
| Python | Good for prototyping, poor fit for a distributable terminal product with many concurrent subprocesses and a polished TUI. |

**Language boundary:**

| Layer | Language |
|---|---|
| Core engine, TUI, CLI, control server, MCP bridge | Go |
| OpenCode / OpenClaw agent processes | Native vendor binaries |
| Codex / Claude ACP adapters | Vendor-provided Node packages |
| Project code the agents edit | Whatever that project uses |

---

## 2. ACP integration: use the Go SDK, do not hand-roll

### Correction

An earlier draft said Go had no ACP SDK. That is outdated. There is a credible, active Go SDK:

- **`github.com/coder/acp-go-sdk`**
- Version checked: **v0.13.5**
- Apache-2.0, Go 1.21+, no external dependencies
- ~248 stars, active as of October 2026, used by other Go modules
- Implements **both** ACP sides: `ClientSideConnection` and `AgentSideConnection`
- Includes typed requests/responses, union wrappers, generated schema types, extension methods, terminal methods, session config options, and examples for a Claude Code bridge and a Gemini CLI bridge

**Decision: use `coder/acp-go-sdk` for the ACP protocol layer.** Do not write JSON-RPC framing, request correlation, cancellation, or ACP wire types ourselves.

### What the SDK provides

- NDJSON JSON-RPC transport over any `io.Reader` / `io.Writer`
- Request IDs, response correlation, timeouts via `context.Context`
- Cancellation (`$/cancel_request`, ACP `session/cancel`)
- `ClientSideConnection` methods:
  - `Initialize`, `Authenticate`, `NewSession`, `LoadSession`, `ResumeSession`
  - `Prompt`, `Cancel`, `CloseSession`, `ListSessions`
  - `SetSessionMode`, `SetSessionConfigOption`
  - `CallExtension` / `NotifyExtension`
- A `Client` interface for inbound agent requests:
  - `SessionUpdate`
  - `RequestPermission`
  - `ReadTextFile`, `WriteTextFile`
  - terminal methods
- Generated ACP types and convenience builders:
  - `acp.TextBlock`, `acp.ToolDiffContent`, `acp.UpdateAgentMessageText`, `acp.StartToolCall`, etc.
- Union unmarshalling for `SessionUpdate`, `ContentBlock`, `ToolCallContent`, permissions
- Diagnostics through `slog`
- `Done()` channel for peer disconnect detection

### What Switch still owns

The SDK is a protocol library, not an agent runtime. These remain ours:

| Concern | Why it is ours |
|---|---|
| Process lifecycle | Spawn adapter, capture stderr, restart, kill, backoff, orphan cleanup |
| Session persistence | Store ACP session IDs; choose load vs resume; replay handling |
| Permission policy | Map ACP permission requests to role policy and the TUI modal |
| Event fan-out | Never block `SessionUpdate`; buffer, persist, coalesce display deltas |
| Capability matrix | Per-adapter feature detection, fallbacks, known quirks |
| Adapter configuration | Model, mode, MCP servers, env, auth method selection |
| Orchestration | Plans, DAGs, scheduling, review, merges, budgets |
| Workspace isolation | Git worktrees, branches, setup hooks, merge queue |
| Verification | Run project commands; capture evidence; never trust worker summaries |
| TUI / CLI / MCP bridge | User and agent control surfaces |

This is the product's actual value. The ACP boilerplate is now a dependency, not a differentiator.

### Alternatives considered

| Option | Verdict |
|---|---|
| Hand-rolled ACP client | Feasible (~1–2k LOC) but unnecessary now; more maintenance and protocol drift risk |
| `acpx` subprocess | Excellent headless ACP CLI with 30 agent profiles and NDJSON output. Useful for probing and as a fallback. Not primary: it is pre-1.0, adds a Node dependency, and its CLI permission flow is less interactive than a native TUI modal. |
| TypeScript sidecar using the official TS SDK | Viable fallback if the Go SDK blocks us. Adds a Node sidecar, a second RPC boundary, and packaging complexity. |
| Rust core with the Rust ACP SDK | Good protocol support, but would reset the existing Go/Bubble Tea work and slow iteration. |

### Dependency policy

1. **Pin `coder/acp-go-sdk` to an exact version** in `go.mod`.
2. **Wrap it** in `internal/driver/acpdriver`; no SDK types in orchestrator/TUI packages.
3. **Conformance test** the wrapper against a fake ACP agent and live OpenCode, Codex, and Claude adapters.
4. **Upstream fixes** when an adapter exposes an SDK gap; the SDK is active and small.
5. **Keep an escape hatch**: the `Driver` interface means a raw protocol client, `acpx`, or a TS sidecar can replace the ACP driver without touching orchestration.
6. **Track ACP v1** now. ACP v2 is still draft; negotiate v1 and revisit when stable.

### Adapter installation

Adapters are external executables or npm packages, so installation is a security
boundary. Switch never installs anything automatically or inside a run.
`switch __control probe --list` reports what is present and prints remediation
hints.

The TUI `/adapters` modal installs or uninstalls npm-managed adapters (Codex,
Claude, OpenClaw) with `npm install -g` at a pinned version (OpenClaw tracks
`latest`), after a second confirming enter, and never while a run (detached
included) is active. An install fails if the adapter's binary is not on PATH
afterwards; an uninstall fails, naming the path, if the binary is still on
PATH because it was installed another way. OpenCode, and any adapter a
`[drivers]` entry in the global config replaces, is managed externally;
`[drivers]` also runs custom adapter commands. Doctor warns when a global
`codex-acp` or `claude-agent-acp` is not the pinned version, which Switch
still prefers to the pinned `npx` package. Still to do:

- show the exact command, package, pinned version, source URL, and install scope
- require one confirmation per package, with a visible "do not install" default
- show the installed version in `/adapters`, offer a reinstall at the pin, and re-probe ACP capabilities
- record what was installed in the run/audit log

---

## 3. Architectural principles

1. **Modular monolith, not microservices.** One binary, clear package boundaries, no broker, no HTTP service for local control.
2. **Headless engine first.** The TUI is a client of the engine. `switch __control run` executes the same run without a TUI.
3. **Protocol-first, vendor-agnostic.** The orchestrator knows ACP, not OpenCode or Codex. Vendor differences live in the driver layer.
4. **Durable state is the source of truth.** The TUI is a projection of SQLite + live events.
5. **Serialized transitions.** A run's task transitions are serialized by one mutex per engine, and a transition out of a status that other processes act on (dispatch, approve, reject, retry) is saved only if the task is still in it, so two processes cannot both win. Merges take a separate lock.
6. **Switch's own actions are fixed in code.** Switch never pushes, merges only what the user approves, and keeps one lead per run, and the control plane refuses each agent the calls its role lacks (delegate for all but the lead, approve and merge for all). Agents act through their adapters as the user, so this is not a sandbox (section 11).
7. **No LLM calls inside Switch.** Planning intelligence comes from the lead agent. Switch validates and executes.
8. **Never block the ACP connection on UI.** `SessionUpdate` pushes to a buffered queue and returns; persistence and rendering happen downstream.

---

## 4. System diagram

```
┌──────────────────────────────── Switch binary ────────────────────────────────┐
│                                                                               │
│  ┌──────────────┐      commands      ┌─────────────────────────────────────┐  │
│  │  TUI         │ ─────────────────► │  Run actor (one per run)            │  │
│  │  Bubble Tea  │ ◄───────────────── │  - plan validation                  │  │
│  └──────────────┘   events/projection│  - task state machine               │  │
│                                      │  - scheduler                        │  │
│  ┌──────────────┐      JSON-RPC      │  - review / merge coordination      │  │
│  │ Control      │ ◄────────────────► │  - budget + watchdog                │  │
│  │ Unix socket  │                    └──────────────┬──────────────────────┘  │
│  └──────┬───────┘                                   │                         │
│         │                                           │                         │
│  ┌──────┴───────┐   ┌──────────────┐   ┌────────────▼──────────────────────┐  │
│  │ __control CLI│   │ MCP bridge   │   │ Session manager                   │  │
│  │ agent/run/…  │   │ (stdio)      │   │ agent registry + role policy      │  │
│  └──────────────┘   └──────────────┘   └────────────┬──────────────────────┘  │
│                                                     │                         │
│  ┌──────────────┐   ┌──────────────┐   ┌────────────▼──────────────────────┐  │
│  │ Rules        │   │ Store        │   │ Driver layer                      │  │
│  │ compiler     │   │ SQLite WAL   │   │ - coder/acp-go-sdk wrapper        │  │
│  └──────────────┘   └──────────────┘   │ - process supervision             │  │
│                                        │ - capability negotiation          │  │
│  ┌──────────────┐   ┌──────────────┐   │ - permission bridge               │  │
│  │ Verification │   │ Git          │   │ - session resume                  │  │
│  │ runner       │   │ worktrees    │   └────────────┬──────────────────────┘  │
│  └──────────────┘   └──────────────┘                │                         │
└─────────────────────────────────────────────────────┼─────────────────────────┘
                                                      │ ACP over stdio
                          ┌───────────────────────────┼───────────────────────────┐
                          ▼                           ▼                           ▼
                   opencode acp                codex-acp                 claude-agent-acp
                   (lead)                      (builder)                 (builder)
                          │                           │                           │
                          └───────────────────────────┴───────────────────────────┘
                                                      │
                                              git worktrees
```

---

## 5. Components

| Component | Owns | Must not do |
|---|---|---|
| **TUI** | Rendering, keyboard input, permission prompts, view state, the chat lead's and teammates' sessions | Change task state except through the engine; run git commands that change the repository, except `/land`'s user-confirmed merge into the checkout's branch |
| **Control server** | Unix socket API, per-agent auth tokens, request validation | Contain orchestration logic |
| **Run actor** | Plan validation, task transitions, scheduling decisions, review/merge sequencing | Call models, render UI |
| **Session manager** | Agent lifecycle, role policy, session refs, resume | Decide which task to run |
| **ACP driver** | Subprocess, SDK connection, capability negotiation, event normalization, cancellation | Interpret tasks or business rules |
| **Rules compiler** | Discover global/project/role rules, resolve precedence, produce prompt blocks | Enforce safety invariants |
| **Workspace manager** | Worktree creation, branches, commits, diffs, merges, conflict checks | Decide when to merge |
| **Verification runner** | Execute configured commands, capture exit codes and output | Trust worker summaries |
| **Store** | Durable runs, tasks, agents, events, usage, verifications, sessions | Contain business logic |
| **Doctor** | Start each team adapter once (ACP initialize), report versions and drift from the pinned ones | Auto-fix without consent |

### Package layout

```
main.go                         entry point: TUI, --resume, hidden __control commands
tui/                            Bubble Tea TUI: chat, team, permission prompts, run views
internal/execution/             run engine: scheduler, turns, verification, review, merges, subagents, run lock
internal/planning/              asks the lead for a plan (or imports one) and creates the run
internal/plan/                  plan schema, parsing, validation
internal/control/               control socket, agent CLI, MCP bridge
internal/driver/acpdriver/      adapter registry, ACP sessions, permissions, limits, orphan cleanup, probe
internal/worktree/              git worktrees, commits, merges, conflict checks
internal/verify/                check and setup commands and evidence
internal/rules/                 discovery, scoping, trust, cleaning
internal/config/                TOML load/merge/validation, project root
internal/store/                 SQLite migrations and queries
internal/chat/                  the chat lead's session and transcript
internal/policy/                permission modes
internal/runcli/                headless run, status, stop, tasks, approve, reject, retry, diff, log, summary, prune
internal/plancli/ chatcli/ rulescli/   headless plan, chat, rules
internal/doctor/ initcmd/ probe/ version/
```

---

## 6. Core interfaces

The design called for interfaces like these between the orchestrator and
`acp`. As built, the engine runs each agent turn through `execution.TurnFunc`
(tests swap in a fake) over `acpdriver` sessions, and talks to git through
`internal/worktree` and to SQLite through `internal/store` directly.

```go
type Driver interface {
    Probe(ctx context.Context, cfg DriverConfig) (Capabilities, error)
    Open(ctx context.Context, spec AgentSpec) (Session, error)
    Resume(ctx context.Context, spec AgentSpec, ref SessionRef) (Session, error)
}

type Session interface {
    Ref() SessionRef
    Prompt(ctx context.Context, prompt Prompt) (TurnResult, error)
    Cancel(ctx context.Context) error
    RespondPermission(ctx context.Context, requestID string, d PermissionDecision) error
    Events() <-chan Update
    Close(ctx context.Context) error
}

type Workspace interface {
    Create(ctx context.Context, run RunID, agent AgentID, base Ref) (Worktree, error)
    Diff(ctx context.Context, wt Worktree, base Ref) (Diff, error)
    Commit(ctx context.Context, wt Worktree, message string) (Ref, error)
    Merge(ctx context.Context, into, from Ref) (MergeResult, error)
    Remove(ctx context.Context, wt Worktree) error
}

type Store interface {
    AppendEvent(ctx context.Context, event Event) error
    PutTask(ctx context.Context, task Task) error
    Task(ctx context.Context, id TaskID) (Task, error)
    TasksForRun(ctx context.Context, run RunID) ([]Task, error)
    RecordUsage(ctx context.Context, usage Usage) error
}
```

Inside the ACP driver, the SDK is used directly:

```go
conn := acp.NewClientSideConnection(clientBridge, agentStdin, agentStdout)

init, err := conn.Initialize(ctx, acp.InitializeRequest{
    ProtocolVersion: acp.ProtocolVersionNumber,
    ClientCapabilities: acp.ClientCapabilities{
        // fs/terminal disabled initially: agents use their own tools.
    },
    ClientInfo: &acp.Implementation{
        Name:    "switch",
        Version: version,
    },
})

sess, err := conn.NewSession(ctx, acp.NewSessionRequest{
    Cwd:        worktreePath,
    McpServers: switchMCPServers,
})

turn, err := conn.Prompt(ctx, acp.PromptRequest{
    SessionId: sess.SessionId,
    Prompt:    []acp.ContentBlock{acp.TextBlock(taskBrief)},
})
```

The `clientBridge` implements `acp.Client`:

- `SessionUpdate` → non-blocking push to the agent event channel
- `RequestPermission` → permission policy + TUI modal, with context cancellation
- `ReadTextFile` / `WriteTextFile` / terminal methods → initially return a clear "unsupported" error, because Switch advertises those capabilities as disabled. Implement them later only if a vendor needs them.

---

## 7. Runtime model

### One engine per run

The process driving a run (TUI, headless, or detached) runs one engine
(`internal/execution`). Its scheduling loop dispatches each ready task to a
goroutine that takes it through work, verification, and review; task
transitions are serialized by the engine's mutex, and approvals, rejections,
and retries from the TUI or `__control` go through an engine too. The control
server records only what an agent reports (`complete`'s summary, a block, a
delegated task) for the engine to act on.

### Agent process model

- One persistent ACP subprocess per agent instance.
- `stdin`/`stdout` pipes wired into `acp.NewClientSideConnection`.
- `stderr`: the last ~2,000 bytes are kept for error messages (`--agent-logs` streams all of it).
- `cmd.Wait` in a goroutine; exit is an event, not a panic.
- Each prompt is bound to a `context.Context`; cancellation sends ACP `session/cancel`, then escalates to process kill after a grace period.
- Permission requests block only that agent's turn, not the run.
- Reconnect: spawn a new adapter process, then `LoadSession` (replay) or `ResumeSession` (no replay) using the stored ACP session ID. Prefer resume where advertised; otherwise load and mark updates as replay.

### Non-blocking event rule

The SDK processes notifications sequentially. If `SessionUpdate` blocks, the agent stalls. Therefore:

1. `SessionUpdate` decodes/normalizes minimally and pushes to a buffered channel.
2. If the buffer is full, persist the event to a fallback queue and drop only intermediate display deltas.
3. The run actor handles persistence, state transitions, and TUI fan-out.
4. `RequestPermission` is allowed to block, because it is a request-response call that genuinely needs a user decision; enforce a timeout and cancellation.

---

## 8. Task flow

```
user → TUI → run actor → lead session prompt
                              │
                              ▼
                     plan submit (MCP/CLI)
                              │
                     validate DAG + scopes
                              │
                              ▼
                     scheduler → worker prompt
                              │
                     stream updates + usage
                              │
                     worker turn ends
                              │
                     Switch commits, runs verification
                              │
                     conflict check → lead review
                              │
                     approve → merge lock → integration verify → branch moves
                              │
                     all merged → run checks → user lands it with /land (or merges the integration branch)
```

Task lifecycle: `queued → running → verifying → reviewing → review →
approved → merged`, with `blocked` and `interrupted` on the side; section 18
has every transition.

---

## 9. Failure behaviour

| Failure | Behaviour |
|---|---|
| Agent process exits | The turn fails with the exit status or signal and the tail of its stderr; Switch stops what it left in its process group; the task goes back with feedback (an attempt) |
| Usage, rate, or context limit | The task is handed off to the worker's `handoff` agent or the lead (an attempt) |
| Worker produces no events | After `idle_timeout`, Switch stops the turn and retries it (an attempt); a pending permission prompt pauses the watchdog |
| SDK detects disconnect | The session ends as exited and the adapter is stopped; the turn fails |
| Permission denied | The adapter gets the reject answer; what it does next is up to it |
| Verification fails | A failing command reruns once from a clean checkout; if it fails again, the output goes back to the same worker; at `max_attempts` the task escalates or is blocked |
| Merge conflict | Before review: the integration branch is merged into the task's worktree with conflict markers and the task goes back to its worker. At approval: the same, and the task is blocked for `/retry`. Switch never resolves conflicts |
| Budget exhausted | No task starts; running worker and subagent turns are interrupted and resume in a later run |
| Switch crashes | Restart; mark `running` tasks `interrupted` without using an attempt; resume sessions and worktrees from SQLite |
| Control socket drops | CLI returns an error; the run continues unaffected |

---

## 10. Storage

**SQLite in WAL mode**, using `modernc.org/sqlite` (pure Go, no CGO). No ORM; hand-written queries and embedded migrations.

| Table | Purpose |
|---|---|
| `runs` | Goal, base commit, integration branch and worktree, status |
| `run_agents` | Each run's agents: name, role (`lead`, `standby`, or a worker role), driver, model |
| `tasks` | Plan task, role, deps, scope paths, status, assignee, attempts, worktree, commit, feedback, review, summary |
| `verifications` | Each check's command, exit code, output, and duration, per attempt and scope (`task` or `integration`) |
| `events` | Append-only audit/event log |
| `usage` | Tokens per agent turn |
| `subagents` | Workers' subagents, their file claims and results |
| `chat_sessions`, `session_history`, `chat_messages` | The TUI's sessions and transcripts |
| `project_settings` | Per project: the chosen lead and the hash of the accepted project rules |

Rules, config, and context are files, not database rows. Runtime state lives in `~/.local/state/switch` (mode `0700`, `switch.db` `0600`); project config lives in `.switch/`. Several Switch processes can open the database at once: each waits up to 5 seconds for another's lock instead of failing.

**Git operations shell out to the `git` binary.** Not `go-git`. Worktrees, merge strategies, hooks, LFS, submodules, and user git config need real git behaviour.

---

## 11. Control plane and security boundaries

| Boundary | Mechanism |
|---|---|
| TUI ↔ engine | In-process calls and events |
| Agent ↔ engine | Unix socket at `~/.local/state/switch/runs/<id8>/control.sock` (the chat's under a hash of the project path), mode `0600` in a `0700` directory; one JSON request per connection |
| Agent authentication | A random token per agent turn in a run, revoked when the turn ends, or per chat session; passed in the adapter's environment (`SWITCH_TOKEN`) and the MCP server's, held only in memory |
| MCP-capable agents | `switch __control mcp` stdio bridge, same handlers; it lists only the tools the agent's role may call |
| Shell agents (OpenClaw) | `$SWITCH_BIN __control agent <method> [message or JSON arguments]` on the same socket |
| Lead reviews | No Switch MCP server, no `SWITCH_*` environment; every permission request is denied |
| Agents ↔ filesystem | Not confined: agents run as the user with their adapter's own tools; Switch answers only the permission requests an adapter sends |
| Permission answers | `build` allows only reads and searches whose every path, including paths the call's earlier tool-call updates named, is inside the agent's own directory, and asks about the rest, run workers' edits in their worktrees included, since verify runs what a worker wrote before approval. Headless and detached runs give what `build` would ask about the run's `--permission` answer (deny unless `--allow`). Paths through `.git`, `.claude`, `.codex`, `.opencode`, `.mcp.json` or `opencode.json(c)`, matched with Unicode case folding, never count as inside |
| Agents ↔ git | Each task has its own worktree and branch; Switch makes the task commits and merges and moves the integration branch; Switch never pushes |
| User decisions | The socket refuses `approve` and `merge` to every agent; `__control approve`, `reject`, `retry` and `prune` refuse to run with `SWITCH_TOKEN` set |
| Secrets | Stay in vendor stores/env; never included in prompts or the event log |

These boundaries hold for agents that follow their instructions, not against
a hostile one. An agent runs as the user, so it can unset `SWITCH_TOKEN` and
run `__control approve`, edit the state database, or run `git push` itself:
the `SWITCH_TOKEN` refusal is a speed bump, not a sandbox. Confining agents
would take an OS sandbox around each adapter.

The socket is a local Unix socket, not a TCP listener. Remote control would be a later, explicitly authenticated transport.

---

## 12. Revised Phase 0

The earlier Phase 0 assumed we had to build an ACP client. Revised:

**Goal:** prove the ACP spine through the SDK across all three vendors.

1. Add `github.com/coder/acp-go-sdk@v0.13.5`.
2. Build `internal/driver/acpdriver` around `ClientSideConnection`.
3. Spawn `opencode acp`, `codex-acp`, and `claude-agent-acp` as subprocesses.
4. Implement a minimal `acp.Client` bridge: stream updates, capture usage, route one permission request.
5. Add `switch __control probe --agent <name>` to print the capability matrix and run one prompt.
6. Verify session resume: kill the process, respawn, `LoadSession` or `ResumeSession`, continue.

**Acceptance:** one prompt streams through each of the three vendors, a tool permission request reaches the TUI, and a session survives a process restart.

**Effort:** 2–3 days instead of 3–4, because the protocol layer is a dependency.

---

## 13. Risks

| Risk | Mitigation |
|---|---|
| ACP SDK is v0.x and may change | Pin exact version; wrap it; conformance tests; budget a small upgrade each cycle |
| Adapter capability differences | Probe at startup; feature flags; per-adapter fallbacks; `/doctor` |
| SDK notification handler blocks and stalls an agent | Non-blocking `SessionUpdate`; buffered queue; test with a slow consumer |
| Session resume behaves differently per vendor | Store session IDs; prefer `ResumeSession`; handle `LoadSession` replay explicitly |
| Lead ignores the delegation protocol | Strong role contract, structured tools, validation, manual TUI fallback |
| Merge conflicts | File scopes, DAG serialization, worktrees, serialized merge queue |
| Cost/runaway loops | Per-task and per-run budgets, max attempts, no recursive delegation |
| Vendor auth/quota | Doctor checks, per-agent model config, graceful degradation |
| SDK project abandoned | Escape hatch: raw ACP client, `acpx` subprocess, or TypeScript sidecar behind the same `Driver` interface |

---

## 14. References

- ACP overview: https://agentclientprotocol.com/protocol/v1/overview
- ACP initialization: https://agentclientprotocol.com/protocol/v1/initialization
- ACP session setup: https://agentclientprotocol.com/protocol/v1/session-setup
- ACP transports: https://agentclientprotocol.com/protocol/v1/transports
- Go SDK: https://github.com/coder/acp-go-sdk
- Go SDK docs: https://pkg.go.dev/github.com/coder/acp-go-sdk
- Codex ACP adapter: https://github.com/agentclientprotocol/codex-acp
- Claude Agent ACP adapter: https://github.com/agentclientprotocol/claude-agent-acp
- OpenClaw ACP bridge: https://docs.openclaw.ai/cli/acp
- acpx (fallback / probe tool): https://github.com/openclaw/acpx

---

## 15. Phase 0 verification (2026-11-02)

The Phase 0 spike is implemented under `internal/driver/acpdriver` and exposed as:

```bash
go run . __control probe --list
go run . __control probe --agent <opencode|codex|claude> [--model <value>] [--session <id>]
```

Verified on this machine:

| Check | OpenCode 2.1.3 | Codex via `codex-acp` 2.5.0 | Claude via `claude-agent-acp` 0.100.0 |
|---|---|---|---|
| ACP initialize | pass | pass | pass |
| Capability report | pass | pass | pass |
| New session | pass | pass | pass |
| Prompt + streamed reply | pass | pass | pass |
| Session resume after process restart | pass | pass | pass |
| Permission allow/deny bridge | adapter did not request | adapter did not request | pass (allow wrote file, deny blocked it); interactive `ask` path implemented, not exercised in non-TTY tests |

Notes:

- OpenCode's default model hit a provider privacy error. `--model` selects a session config value before prompting; `opencode/deepseek-v4.1-flash-free` was used for verification.
- Codex and Claude adapters were launched through `npx` with pinned versions.
- Claude adapter stderr is hidden by default; pass `--agent-logs` to stream it.
- At Phase 0, OpenClaw was not installed here, so its ACP bridge was untested; it has since been verified (section 18, Verified adapters).
- Session IDs printed by the probe can be passed back with `--session` for resume.

Phase 0 exit criteria are met for OpenCode, Codex, and Claude. The next milestone is the engine/TUI shell (Phase 1), not more ACP plumbing.

---

## 16. Phase 1 status (2026-11-02)

Implemented:

- Global/project TOML configuration with `/init` and automatic project initialization on first `switch` run
- Global, project, and project-local rule loading with prompt injection
- SQLite-backed chat session and message state
- Reusable ACP `Session` API with event and permission channels
- Resumable TUI chat, plus an internal `__control chat` REPL
- Bubble Tea TUI wired to the chat engine: streaming messages, history,
  permission choices, session resume, and `/rules`
- `/doctor` diagnostics

Not yet implemented (Phase 2):

- Task plans, scheduler, and lead/worker delegation
- Git worktrees, verification runner, review, and merge queue
- Control socket, agent-facing CLI, and MCP bridge
- Role-scoped rule compilation

Superseded by section 18.

---

## 17. Phase 2 progress (2026-11-02)

Implemented:

- Plan schema with dependency/cycle validation (`internal/plan`)
- Durable runs, run agents, and tasks in SQLite
- Git worktree manager with per-run integration and agent worktrees
- TUI `/plan` and the internal `__control plan` (headless):
  - ask the configured lead for a JSON plan, or import `--file`
  - create the run integration branch and a worktree per configured agent
  - persist the plan as queued tasks with mapped dependencies
  - report run, agent, and task summaries
- TUI slash commands: `/plan`, `/doctor`, `/init`, `/rules`, `/session`,
  `/lead`, `/new`, `/home`, `/help`, `/quit`
- User-selectable lead from the configured agents, persisted per project
- One TUI design system shared by the dashboard, conversation, modals, input,
  status bar, and command palette

Configured agents default to one lead plus two builders, all on the default
agent. The `.switch/config.toml` written on first run has every setting and
the example team commented out, so a new project uses the global config,
then these defaults.

Superseded by section 18.

---

## 18. Execution engine (2026-11-03)

Phases 2–6 are implemented. Packages:

| Package | Responsibility |
|---|---|
| `internal/execution` | Run engine: scheduler, worker turns, verification, lead review, merges, handoffs, subagents |
| `internal/verify` | Runs check and setup commands (`sh -c`, per-command timeout, process-group kill, one rerun of a failure) |
| `internal/worktree` | Worktrees plus commit, clean, checkout, merge (abort on conflict), advance (fast-forward), update (merge leaving conflict markers), conflicts (`git merge-tree`), merged, diff, merge-base |
| `internal/control` | Per-run Unix socket, agent CLI, MCP stdio bridge |
| `internal/runcli` | Headless `__control run\|status\|stop\|tasks\|approve\|reject\|retry\|diff\|log\|summary\|prune` |

### Task lifecycle

| From | Event | To |
|---|---|---|
| `queued`, `interrupted` | dispatched to a worker (an interrupted task keeps its attempt) | `running` |
| `running` | `[run] setup` fails or is stopped (it runs before the first attempt, again after `/retry`, and when an interrupted first attempt resumes) | `blocked` |
| `running` | turn ends | `verifying` |
| `running` | worker called `block`, or the user stopped it | `blocked` |
| `running` | worker handed off, or a limit stopped it | `queued` for the new agent, or as below at `max_attempts` |
| `running` | worker error, turn or idle timeout | `queued` with feedback, or as below at `max_attempts` |
| `verifying` | committed, checks pass, a change, no conflict with the integration branch | `reviewing` (or `review` without lead review) |
| `verifying` | checks fail, no change, or a conflict (integration branch merged into the worktree) | `queued` with feedback, or as below at `max_attempts` |
| `reviewing` | lead APPROVE, no verdict, or the lead worked on the task | `review` |
| `reviewing` | lead REJECT | `queued` with feedback, or as below at `max_attempts` |
| `review` | user `/approve` | `approved` → `merged` |
| `review` | user `/reject <reason>` | `queued` with feedback, or as below at `max_attempts` |
| `approved` | merge conflict or integration check fails | `blocked`; nothing merged |
| `approved` | approval interrupted, or a restart finds the merge lock free | `review` |
| any send-back at `max_attempts` | the worker has an `escalate` agent | `queued` for that agent, once |
| any send-back at `max_attempts` | no escalation left | `blocked` (`attempt limit N reached: <reason>`) |
| (lead proposal) | `switch_delegate` from the chat or a run lead's task turn | `blocked` (`proposed by …`); it does not hold run completion back |
| `blocked`, `interrupted` | user `/retry` | `queued` (attempts reset, fresh session, any worker of its role; a proposal starts with no feedback) |
| `blocked`, `queued`, `interrupted`, `review` | user `/fix`, then exits the shell | `verifying` → `review`, or `blocked` if checks fail or nothing changed (no attempt used) |
| `running` | run budget used up | `interrupted` |
| any active state | run stopped, quit, or crash | `interrupted` |

- **Readiness:** a task runs when every dependency is `merged`, no started
  and unmerged task (running through approved) has a file scope that
  overlaps its own (a glob stands for its directory; tasks without `files`
  overlap nothing), and an agent may take it: a task sent back goes to the
  worker that attempted it, and one pinned by the plan's `agent`, handed off,
  or escalated goes only to that agent, the lead included; others go to any
  free worker of their role. Each worker runs one task at a time.
  `__control status` and the run's outcome log say why each unfinished task
  waits.
- **Worktrees:** each task gets `switch/<run>/task-<id>`, branched from the
  integration branch at first dispatch, so dependents see merged work and a
  rejected change never reaches another task. This replaces the earlier
  one-worktree-per-agent model. Merged task worktrees are removed; branches
  stay. A worktree a crash left before it was recorded is unlocked, reset,
  and reused. When the project is a subfolder of the repository, agents and
  checks work in that subfolder of each worktree.
- **Transitions** are serialized by one mutex in each engine; worker turns,
  verification, reviews, and merges run concurrently. A transition out of a
  status other processes act on (dispatch, approve, reject, retry) is saved
  only while the task is still in it, so the loser of two such decisions gets
  `task X is no longer <status>; it changed meanwhile`.
- **Verification:** Switch commits the worker's change (`git add -A`, so new
  files count even with `status.showUntrackedFiles=no`), runs the task's
  checks on that commit, and cleans the worktree afterwards (`git reset
  --hard`, `git clean -fd`), so nothing a check writes is committed. A
  failing command reruns once after the same clean; a pass then logs a
  `flaky` event and the evidence says "passed on a rerun (flaky)", and a
  second failure reports both. A timeout is not rerun. Workers, checks, and
  setup get `SWITCH_SLOT` (unique among running tasks; 0 for integration
  checks), `SWITCH_TASK`, and `SWITCH_RUN`. Results are stored per attempt
  (`verifications`), both in the task worktree and after merge
  (`scope = integration`). A task with no change goes back to its worker, as
  does one whose worktree still holds conflict markers, before anything is
  committed. A `switch_block` wins over a limit or error ending the same turn.
  Before review, `git merge-tree` (git 2.38+) checks the commit against the
  integration branch.
- **Lead review** gets the diff against the merge base, the evidence, the
  worker's name and driver, and its `switch_complete` summary (or else its
  final reply). The verdict is read from the first line only (APPROVE or
  REJECT, also `Verdict: APPROVE`, bold or not); only REJECT is acted on, and
  the user always makes the merge decision. The review turn is read-only: no
  Switch MCP server, no `SWITCH_*` environment, every permission request
  denied. A task the run's lead ever worked on skips review and waits for the
  user with a note saying so.
- **Merging:** an approval takes `merge.lock` next to the integration
  worktree, so approvals from any engine or process merge one at a time; the
  engine mutex is not held, so the run goes on meanwhile. Switch checks out
  the integration branch's tip on a detached HEAD, merges the task's
  reviewed commit there with `git merge --no-ff`, runs `[run] setup`, then
  the task's checks plus `[run] verify`, and fast-forwards the branch only
  when they pass, so the branch never holds an unverified merge. Between
  approvals the integration worktree has the branch checked out, so git
  refuses to check it out elsewhere; if the user does so mid-approval, the
  fast-forward fails and the task is blocked. A conflict aborts the merge, merges the integration branch
  into the task's worktree with conflict markers, and blocks the task, so
  `/retry` has a worker resolve them, skipping `[run] setup` until no markers
  remain; a resolution that keeps one side and is
  staged is committed too. Switch's own commits, merges, resets and checkouts
  run as `Switch <switch@localhost>` with per-command `-c
  core.hooksPath=/dev/null`, `commit.gpgsign=false`, `merge.ff=true`, and
  `merge.verifySignatures=false`; the user's git config is never edited.
  Switch never pushes and never touches the user's branches.
- **Run checks:** once every task is merged, `[run] setup` and `[run] verify`
  run once more on the integration branch; a failure ends the run `blocked`
  with the output instead of `completed`.
- **Budgets:** `max_turns` counts every agent turn of the run (worker turns,
  lead reviews, subagent turns, and turns whose adapter failed to start);
  `max_tokens` counts reported tokens. Running out stops dispatching and
  interrupts the worker and subagent turns still running; work whose turn had
  ended is still verified and reviewed (that review may use one turn more).
  `max_attempts` bounds each task; interruptions (stop, quit, crash, budget)
  do not use attempts. `idle_timeout` stops a worker turn after that long
  without an event from the worker or its subagents, paused while one of them
  waits on a permission prompt.
- **Subagents** (`internal/execution/subagents.go`, table `subagents`): a
  worker's `switch_spawn` starts an ACP turn as `<worker>/<name>` with role
  `subagent` in the worker's worktree. Limits: `[run.subagents]
  max_per_builder` running per worker, optional `drivers` allow-list,
  disjoint file claims among running siblings, the run budget, and one level
  only (the control plane refuses subagents). Only the agent working on a
  running task may use the subagent tools: its worker, or the lead on a task
  handed to it. Follow-ups queue until the current turn ends or restart a
  finished subagent in its session, within the budget, the per-worker limit,
  and file claims; ones an earlier process ran (`interrupted`) can be resumed
  the same way. `switch_wait` returns within 45 seconds with any still
  running. `switch_complete` is refused while any run; `process` stops
  leftovers after the worker's turn, before verification. Recovery marks
  running ones `interrupted`.
- **Recovery:** on start, `running`/`verifying`/`reviewing` tasks become
  `interrupted`, and an `approved` task returns to `review` unless an
  approval still holds the merge lock (the integration branch never took its
  unverified merge); such a task, and a `verifying` one left the same way,
  is recovered in the same run once the lock is free. Adapter process groups are recorded under
  `<state>/pids`; a later start kills those whose owning Switch process is
  gone, `npx`-started adapters included, after checking that the PID still
  runs the recorded command. When an adapter exits, Switch also stops what it
  left running in its process group.

### Control plane

- Socket: `<state>/runs/<id8>/control.sock`, mode `0600` in a `0700`
  directory; a short temp path is used when that exceeds the Unix socket
  limit. The chat's server (teammates) keys its directory by a hash of the
  project path.
- Each agent turn in a run gets a fresh random token, revoked when the turn
  ends; a chat session's token lasts as long as the session. Tokens are held
  in memory only (no `.switch-run` file, so nothing can be committed by
  accident) and map to the agent, its role, and its task.
- Chat sessions and run turns, except lead reviews, are offered the MCP bridge
  through ACP `mcpServers` (OpenClaw's bridge refuses it), and `SWITCH_SOCKET`,
  `SWITCH_TOKEN`, and `SWITCH_BIN` are set in the adapter's environment for
  `$SWITCH_BIN __control agent <method> [message | JSON object]`. A JSON
  object that names one of the tool's arguments is sent as the arguments;
  other text is the message. `switch __control agent` lists the methods.
- Methods, by caller role (`lead`, `subagent`, or `worker` for any other
  role), and by the kind of server, a run's or the chat's; the MCP bridge
  lists only the tools the agent may call, and `whoami` lists the methods:

  | Method | Who | Server |
  |---|---|---|
  | `whoami`, `team`, `progress`, `limits` | anyone (`progress` not from the chat lead, which says it in its reply) | both |
  | `task`/`claim` | anyone | run |
  | `complete`, `block` | the agent working on the running task: its worker, or the lead on a task handed to it | run |
  | `handoff` | lead or worker: in a run, its running task; in the chat, only the lead | both |
  | `spawn`, `wait`, `message`, `stop`, `subagents` | lead or worker: in a run, on its running task; in the chat, a teammate answering a question | both |
  | `ask` | lead or worker, not one answering a question | chat |
  | `report` | subagents | both |
  | `delegate` | lead; needs a title, brief, acceptance criteria, and a role a worker in the run has; the task is a proposal, blocked until the user's `/retry` | run |
  | `approve`, `merge` | nobody | — |

- Refusals read `forbidden: <method> is for <roles>, not <role>`.
  `switch_complete` saves the worker's summary, which wins over its final
  reply; `switch_block` blocks the task instead of verifying it.
- `switch_wait` and `switch_ask` return within 45 seconds and say to call
  again; a teammate keeps answering meanwhile, and the same question asked
  again collects the answer (one nobody collects within 45 seconds of
  arriving is dropped). The bridge handles calls in parallel and honours
  `notifications/cancelled`.
- Each connection carries one request and is handled independently; a
  dropped connection never affects the run. Closing the server ends the
  requests in flight.
- `__control approve`, `reject`, `retry` and `prune` refuse to run when
  `SWITCH_TOKEN` is set, which marks an agent's process. An agent can unset
  it, so this is a speed bump, not a sandbox (section 11).

### Rules

Precedence is global → project → local → role → agent → task; within the
role and agent levels the global file (`~/.config/switch/rules/`) comes before
the project one (`.switch/rules/`). Lead-only guidance goes in `roles/lead.md`;
the chat lead is scoped as role `lead` whatever its configured role.
`rules.Scoped` rereads the files on every call, so edits apply to the next
prompt, and rejects names that would escape the rules directory. HTML comments
are stripped, and so are terminal escapes, control characters, and invisible
Unicode (bidi overrides, zero-width and tag characters, variation selectors),
in the review and in what agents receive. `ignore_global` in `[rules]` turns
the global scope off. `[rules]` paths: `~` is the user's home; relative
`project` and `local` paths are relative to the project folder, and
`global`, which only the user's own config may set, to `~/.config/switch`.

Chat sends the lead's rules once per session and again when their hash
changes (with a note that they replace the earlier ones, or that they no
longer apply). Worker and review prompts carry their rules on every turn.
Chat teammates answering `switch_ask` get the rules `/rules <agent>` shows;
chat subagents get their teammate's rules plus `roles/subagent.md`.

Project rules are untrusted input. They are every rule file of the project:
`.switch/rules.md`, the local file, and the project's `roles/`, `agents/`,
and `tasks/` files. `rules.Set.Trust` sends them only while their hash
matches the one the user accepted (`project_settings.trusted_rules`); until
then every prompt leaves them out, even after a mid-session change. The TUI
shows unaccepted ones at launch and within about 2 seconds of a change, and
enter accepts exactly the text shown; if the stored answer cannot be read,
the review is shown rather than skipped. Declining drops all of them from
chat, planning, and runs (detached ones too) for that session; nothing is
read from the working directory instead. An edit through `/rules` counts as
accepting only if the editor succeeded, the rules were accepted before, and
no other rule file changed while it was open; `/remember` keeps accepted
rules accepted. Headless `__control run|chat|plan` leave out unaccepted
project rules with a note; `__control run --no-project-rules` leaves out all
of them.

### State added

`verifications`, `events` (audit log), and `usage` (tokens per turn; the
adapter-reported session cost goes to the audit log because it is
cumulative). `tasks` gained `session_id`, `commit_sha`, `feedback`, `review`,
`last_error`, `summary`; `runs` gained `integration_path`. Columns are added
in place for databases created by older builds.

### Run ownership and detached runs

- The process driving a run holds an exclusive `flock` on
  `<state>/runs/<id8>/owner.lock`, which records its PID, followed by
  `headless` for a headless or detached owner. The kernel drops the lock
  when that process dies, so a crash never leaves it stale. A second driver
  (TUI, headless, or detached) is refused with the owner's PID, and the
  control server refuses to replace a socket that still accepts connections.
- `__control run --detach` (TUI: `/run --detach [--allow]`) starts
  `__control run --wait` in its own session, logging to
  `<state>/runs/<id8>/run.log`, and returns once the child holds the lock. A
  decline of the project rules in the TUI is passed on as
  `--no-project-rules`.
- A run's engine, in the TUI or with `--wait`, polls the store every 2s, so
  approvals, rejections, and retries made by another process (a TUI or
  `__control approve`) reach it, and dependent tasks start, even while other
  workers run. With `--wait` the run stays alive while tasks wait for review
  and exits on completed, blocked, budget, or stopped.
- `__control status` shows the owner and what each unfinished task waits
  for. `__control stop` (or `/stop` in a TUI that does not drive the run)
  sends SIGINT to a headless owner and waits for the lock to be released;
  running tasks become `interrupted`. A run a TUI drives is refused: stop it
  there with `/stop`.
- The TUI refreshes the dashboard every 2s, so it shows a detached run's
  progress and approves its tasks. While any run is active, detached ones
  included, it refuses `/plan`, `/run`, and `/adapters`.
- A completed headless or detached run, and a `__control approve` that merges
  the last task, write `<state>/runs/<id8>/summary.md`, the PR description
  `__control summary` prints: goal, integration branch, each task's summary,
  review notes and latest check results, and token use.
- `__control prune` removes the worktrees and `switch/<run>/` branches of
  completed runs and of older runs a newer plan replaced, holding each run's
  lock and merge lock meanwhile; it skips a run a process drives, one an
  approval is merging in, and the current unfinished run, and removes a
  pruned run's folder whole. Without `--all` it keeps worktrees with
  uncommitted changes (untracked files count whatever git's config), task
  branches not merged into their integration branch, and integration branches
  that no branch outside `switch/` contains. `__control run`, `approve`,
  `reject` and `retry` refuse a pruned run.
- `[run] notify` is read only from the global config; a project config that
  sets it, like `[drivers]`, fails to load.
- The TUI reloads `[run]` from the project config each time it builds an
  engine (`/run`, approvals, task actions); `max_concurrency` keeps the launch
  or `/parallel` value.

### Session lifetime

Only ephemeral sessions (planning, lead reviews) are closed with
`session/close`. Chat and worker sessions are left open on the agent so they
can be resumed: some adapters delete a session on close. The probe keeps
sessions unless `--close` is passed. ACP errors now include the adapter's
`data` field, which carries the real cause.

Starting a session (new, resume, or load, and model selection) fails after 2
minutes. When an adapter exits, the error gives its exit status or signal and
the last ~2,000 bytes of its stderr, and Switch stops whatever it left in its
process group (dev servers, watchers, MCP servers). A dropped ACP connection
ends the session as exited. Codex's context-window and rate-limit stops count
as limits; an adapter crash never does. Adapter text (tool titles, commands,
output, plans, thoughts, permission prompts) is shown with control characters
and escape sequences removed.

### Adapter quirks

`Adapter.NoSessionMCP` drops per-session MCP servers for adapters that reject
them, and `Adapter.SessionKeyPrefix` sends `_meta.sessionKey` (with
`requireExisting`) when resuming. Both are set for OpenClaw, whose ACP bridge
refuses session MCP servers and, after a bridge restart, looks sessions up by
bare ID instead of the `acp-bridge:<id>` key it created them under. When the
Codex or Claude adapter falls back to `npx`, it runs from the system temp
directory, so a project's `node_modules` cannot supply the adapter.

### Verified adapters

| Adapter | Verified |
|---|---|
| OpenCode | Full runs: workers, lead review, MCP control plane, resume |
| Codex | Probe, resume (Phase 0); `/adapters` install → detected → ACP connect → uninstall (scratch npm prefix), and a real global install through `/adapters`, kept on the development machine |
| Claude | Probe, permissions, resume (Phase 0) |
| OpenClaw 2026.10.10 | `/adapters` install; gateway (loopback, dev); initialize, sessions, resume after bridge restart; a model reply; and a full run as a worker: the model called OpenClaw's `exec` tool, the file landed in the Switch task worktree, verification passed, the OpenCode lead approved, and the merge completed, both headless and in the TUI (approved from `/review`). The model was a scripted OpenAI-compatible stand-in on loopback (`models.providers` custom provider), since no provider key was available. Real hosted-model replies were then verified by pointing the same custom provider at a loopback relay to OpenCode's free hosted model (`opencode run`): a probe prompt and a TUI chat with OpenClaw as the lead both returned the model's correct answers. The user accepted this as the OpenClaw verification; a built-in OpenClaw provider only changes OpenClaw's own config. |

### Not in v1

- Several Switch processes cooperating on one run (one drives; others may
  approve, reject, and retry).
- Prewarming run workers' adapters to hide cold-start latency (the chat warms
  teammates only).
- Creating the integration worktree at the first `/run` instead of at
  `/plan`, and removing runs a newer plan replaced without `__control prune`.
- Writing the PR description from a TUI-driven run.
