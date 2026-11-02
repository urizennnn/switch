# Switch — System Architecture & Language Choice

Status: Draft for build  
Date: 2026-11-02  
Scope: v1 — git repositories, one lead + N workers, OpenCode / Codex / Claude first

---

## 0. Locked decisions

| Decision | Choice |
|---|---|
| V1 scope | Coding repositories with git |
| Lead behavior | Plan and review only; does not implement |
| First vendors | OpenCode, Codex, Claude Code |
| Lead | Exactly one per run; the only agent the user converses with |
| Isolation | One git worktree per agent/task |
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

### Adapter installation (deferred)

Adapters are external executables or npm packages, so installation is a security
boundary. V1 does not install anything automatically and does not run installers
inside a run. `switch probe --list` only reports what is present and prints
remediation hints.

A later `switch doctor` / first-run flow will offer an explicit installation UI:

- show the exact command, package, pinned version, source URL, and install scope
- require one confirmation per package, with a visible "do not install" default
- never install while a run is active
- verify the resulting binary/version and re-probe ACP capabilities
- record what was installed in the run/audit log
- support manual and custom adapter commands for air-gapped setups

---

## 3. Architectural principles

1. **Modular monolith, not microservices.** One binary, clear package boundaries, no broker, no HTTP service for local control.
2. **Headless engine first.** The TUI is a client of the engine. `switch run` executes the same run without a TUI.
3. **Protocol-first, vendor-agnostic.** The orchestrator knows ACP, not OpenCode or Codex. Vendor differences live in the driver layer.
4. **Durable state is the source of truth.** The TUI is a projection of SQLite + live events.
5. **One actor per run.** All task/agent state transitions for a run are serialized through a single goroutine.
6. **Safety invariants live in code.** "No push", "no merge without approval", "one lead", and "workers cannot delegate" are enforced by the control plane, not by prompt text.
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
│  │ switch CLI   │   │ MCP bridge   │   │ Session manager                   │  │
│  │ task/plan/…  │   │ (stdio)      │   │ agent registry + role policy      │  │
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
| **TUI** | Rendering, keyboard input, approval modals, view state | Write task state directly, spawn agents, call git |
| **Control server** | Unix socket API, per-agent auth tokens, request validation | Contain orchestration logic |
| **Run actor** | Plan validation, task transitions, scheduling decisions, review/merge sequencing | Call models, render UI |
| **Session manager** | Agent lifecycle, role policy, session refs, resume | Decide which task to run |
| **ACP driver** | Subprocess, SDK connection, capability negotiation, event normalization, cancellation | Interpret tasks or business rules |
| **Rules compiler** | Discover global/project/role rules, resolve precedence, produce prompt blocks | Enforce safety invariants |
| **Workspace manager** | Worktree creation, branches, setup hooks, diffs, merges | Decide when to merge |
| **Verification runner** | Execute configured commands, capture exit codes/output/artifacts | Trust worker summaries |
| **Store** | Durable runs, tasks, agents, events, usage, approvals | Contain business logic |
| **Doctor** | Probe CLIs, adapter versions, auth, ACP capabilities | Auto-fix without consent |

### Package layout

```
main.go                         CLI entry point (TUI, probe, version)
internal/driver/acpdriver/      adapter registry, ACP client bridge, probe
internal/probe/                 probe CLI flags and lifecycle
internal/version/               build version
internal/orchestrator/          run actor, scheduler, review, merge (Phase 1+)
internal/agent/                 agent instance and session state (Phase 1+)
internal/worktree/              git worktree and merge operations
internal/rules/                 discovery, frontmatter, compiler
internal/store/                 SQLite migrations and queries
internal/control/               Unix socket, CLI handlers, MCP bridge
internal/tui/                   app shell, chat, fleet, board, diff, logs
internal/verify/                command runner and evidence capture
internal/config/                TOML load/merge/validation
internal/doctor/
```

The driver and probe packages exist now. The rest are created in Phase 1 and
later; the existing top-level `tui/` package remains the current Bubble Tea
scaffold until it is moved under `internal/tui/`.

---

## 6. Core interfaces

The orchestrator depends on these, not on `acp` directly.

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

### One run actor

Each active run has a single goroutine consuming two streams:

- **Commands** from the TUI/control plane: `SendUserMessage`, `SubmitPlan`, `TaskDone`, `ApproveTask`, `CancelTask`, `RespondPermission`, `InterruptAgent`.
- **Events** from agent sessions: message chunks, tool calls, permission requests, turn completion, usage updates, process exits.

Only the run actor mutates task state. The TUI and control server are thin.

### Agent process model

- One persistent ACP subprocess per agent instance.
- `stdin`/`stdout` pipes wired into `acp.NewClientSideConnection`.
- `stderr` captured to a per-agent log file.
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
                     worker claims done
                              │
                     Switch runs verification
                              │
                     diff capture → lead review
                              │
                     approve → merge queue → integration verify
                              │
                     all done → user confirms final merge
```

Task lifecycle:

```
queued → running → verifying → review → approved → merged → done
                    ↘ failed → retry / reassign
                    ↘ blocked (dependency, conflict, permission, budget)
```

---

## 9. Failure behaviour

| Failure | Behaviour |
|---|---|
| Agent process exits | Mark agent failed; requeue task; resume session if supported; exponential backoff |
| Agent produces no events | Watchdog warns, then cancels and retries |
| SDK detects disconnect | `Done()` closes; session manager reconnects or fails the task |
| Permission denied | Block task, notify lead/user with the exact request |
| Verification fails | Return failure output to the same worker; retry budget then reassign |
| Merge conflict | Block task; notify lead with conflicting files; never auto-resolve |
| Budget exhausted | Interrupt agent, pause run, require user decision |
| Switch crashes | Restart; mark `running` tasks `interrupted`; resume sessions and worktrees from SQLite |
| Control socket drops | CLI returns an error; the run actor continues unaffected |

---

## 10. Storage

**SQLite in WAL mode**, using `modernc.org/sqlite` (pure Go, no CGO). No ORM; hand-written queries and embedded migrations.

| Table | Purpose |
|---|---|
| `projects` | Repo path, name, config hash |
| `runs` | Goal, base commit, integration branch, status, budget |
| `agents` | Vendor, role, model, ACP session ref, worktree, status |
| `tasks` | Plan task, role, deps, scope paths, state, attempts, result |
| `events` | Append-only audit/event log |
| `messages` | Lead/user conversation and worker reports |
| `approvals` | Permission requests and decisions |
| `usage` | Tokens, cost, duration per agent/task |
| `artifacts` | Test output, diffs, logs, reports |

Rules, config, and context are files, not database rows. Runtime state lives in `~/.local/state/switch`; project config lives in `.switch/`.

**Git operations shell out to the `git` binary.** Not `go-git`. Worktrees, merge strategies, hooks, LFS, submodules, and user git config need real git behaviour.

---

## 11. Control plane and security boundaries

| Boundary | Mechanism |
|---|---|
| TUI ↔ engine | In-process commands/events |
| Agent ↔ engine | Unix socket at `~/.local/state/switch/runs/<id>/control.sock`, mode `0600` |
| Agent authentication | Per-agent bearer token in a `.switch-run` file in its worktree, mode `0600` |
| MCP-capable agents | `switch mcp` stdio bridge, same handlers |
| Shell-only agents (OpenClaw) | `switch task ...` CLI uses the same socket |
| Agents ↔ filesystem | Vendor sandbox, confined to their worktree; lead is read-only outside `.switch/context/**` |
| Agents ↔ git | Agents commit only in their own worktree; Switch owns merges; no agent can push |
| Secrets | Stay in vendor stores/env; never included in prompts or the event log |

The socket is a local Unix socket, not a TCP listener. Remote control would be a later, explicitly authenticated transport.

---

## 12. Revised Phase 0

The earlier Phase 0 assumed we had to build an ACP client. Revised:

**Goal:** prove the ACP spine through the SDK across all three vendors.

1. Add `github.com/coder/acp-go-sdk@v0.13.5`.
2. Build `internal/driver/acp` around `ClientSideConnection`.
3. Spawn `opencode acp`, `codex-acp`, and `claude-agent-acp` as subprocesses.
4. Implement a minimal `acp.Client` bridge: stream updates, capture usage, route one permission request.
5. Add `switch probe --agent <name>` to print the capability matrix and run one prompt.
6. Verify session resume: kill the process, respawn, `LoadSession` or `ResumeSession`, continue.

**Acceptance:** one prompt streams through each of the three vendors, a tool permission request reaches the TUI, and a session survives a process restart.

**Effort:** 2–3 days instead of 3–4, because the protocol layer is a dependency.

---

## 13. Risks

| Risk | Mitigation |
|---|---|
| ACP SDK is v0.x and may change | Pin exact version; wrap it; conformance tests; budget a small upgrade each cycle |
| Adapter capability differences | Probe at startup; feature flags; per-adapter fallbacks; `switch doctor` |
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
go run . probe --list
go run . probe --agent <opencode|codex|claude> [--model <value>] [--session <id>]
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
- OpenClaw is not installed here; its ACP bridge remains untested.
- Session IDs printed by the probe can be passed back with `--session` for resume.

Phase 0 exit criteria are met for OpenCode, Codex, and Claude. The next milestone is the engine/TUI shell (Phase 1), not more ACP plumbing.
