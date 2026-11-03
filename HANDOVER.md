# Switch — Handover Plan

**Updated:** 2026-11-03  
**Baseline commit:** `2f038f8 feat(tui): redesign shell and add lead picker`  
**Working tree:** dirty; all work listed below is local and uncommitted. Do not commit or push unless the user explicitly asks.  
**Product:** a terminal orchestrator for a team of existing coding-agent CLIs, with one user-facing lead, delegated workers, scoped rules, git worktrees, and durable state.

---

## 1. Executive summary

Phases 0 and 1 are complete, and the Phase 2 planning foundation is working. The TUI is now the only user-facing surface. The remaining critical path is:

1. dispatch a queued task to a worker,
2. verify the worker's changes,
3. let the lead review them,
4. merge approved work into the run integration branch,
5. expose the same operations to agents through the control plane.

**Update 2026-11-03:** Milestone A (single-task execution) is done: `/run` dispatches the next ready task to a worker in its agent worktree, streams output, persists state and the worker session, and supports cancel/resume. Items 5.1, 5.2, 5.4, and 5.7 are resolved. The biggest gap is now the **scheduler and verification runner** (Milestones B and C): finished tasks stop at `implemented`, so dependents stay blocked.

---

## 2. What is done

### 2.1 ACP driver and probe

- `github.com/coder/acp-go-sdk v0.13.5` is pinned and wrapped behind `internal/driver/acpdriver`.
- Process lifecycle, ACP initialize, session create/resume, prompt streaming, cancellation, permissions, capability reporting, and adapter logs exist.
- Probe verified against OpenCode, Codex, and Claude:
  - initialize
  - new session
  - streamed prompt
  - resume after process restart
  - permission bridge (Claude verified allow/deny; interactive ask path exists)
- OpenClaw is defined as an adapter target but is not installed/tested on this machine.

### 2.2 Config, rules, state, and chat

- Global/project TOML config, auto-init of `.switch/config.toml` and `.switch/rules.md`.
- Global/project/local rules loading and prompt injection.
- SQLite database in WAL mode at `~/.local/state/switch/switch.db`.
- Tables for chat sessions/messages, project settings, runs, run agents, and tasks.
- Resumable chat sessions and persisted chat messages.
- Internal `switch __control` commands for chat, plan, probe, doctor, and init.
- `switch doctor` and TUI `/doctor`.

### 2.3 TUI

- Bubble Tea v2 shell: header, workspace sidebar, main pane, input, status line.
- Shared design system in `tui/design.go`.
- Live dashboard built from project, lead, driver, model, session, rules, configured agents, latest run, task counts, commands, and keys.
- Streaming assistant output, tool status, reasoning/tool/text plan streaming.
- Command palette and slash commands:
  - `/plan`, `/doctor`, `/init`, `/rules`, `/session`, `/lead`, `/adapters`, `/new`, `/home`, `/help`, `/quit`
- True centered modals, including `/lead` and `/adapters`.
- Sidebar now distinguishes the active lead from idle configured agents.
- Lazy startup: the TUI renders immediately with a `CONNECTING` state; the lead connects in the background.
- Optimistic lead switching: header, sidebar, and overview update immediately; session shows `connecting…` until the ACP session is ready; failed switches revert.
- Lead choice is persisted immediately on selection, again on shutdown, and on successful switch. This fixes the case where quitting during a slow adapter start lost the choice.
- A TUI regression test covers immediate lead persistence (`tui/leadpersist_test.go`).

### 2.4 Lead and adapter management

- `/lead` opens a centered picker of **configured agents only**.
- `/adapters` opens a separate centered manager for npm-managed ACP adapters.
- Adapter installation is explicit and confirmation-based; OpenCode is treated as externally managed.
- Managed adapter code lives in `internal/driver/acpdriver/managed.go` with tests.
- Lead selection is stored per project in `project_settings.lead_agent`.
- Startup prefers the stored lead, then a stored chat session, then the configured default.

### 2.5 Planning foundation

- Plan schema and validation in `internal/plan`:
  - task IDs, roles, briefs, dependencies, acceptance criteria
  - dependency existence, cycles, duplicate IDs, max task count
- `internal/planning` can:
  - ask the lead for a JSON plan, or import a plan file
  - validate the plan
  - create a run and run integration branch
  - create one worktree per configured agent
  - persist run agents and queued tasks with mapped dependencies
  - stream plan text/tool/reasoning output to the TUI
- `internal/worktree` creates run integration and agent worktrees, and can remove them.
- SQLite persists runs, run agents, and tasks.

### 2.6 Verification state

The following pass locally:

```bash
go build ./...
go test ./...
go vet ./...
```

Manual TUI smoke tests covered:

- instant startup with a background connection,
- `/lead` true modal,
- lead switch and immediate sidebar/header/overview update,
- quit during a lead switch and restart with the new lead,
- `/adapters` modal and confirmation flow,
- prompt streaming, permission modal, and quit.

---

## 3. Uncommitted handoff state

Last committed work is `2f038f8`. The following files are modified or new and must be reviewed before the next commit:

| File | Purpose |
|---|---|
| `main.go` | Lazy TUI startup; resolve stored lead |
| `tui/model.go` | Background connection, connection context, lead fields, shutdown persistence |
| `tui/update.go` | Lazy connection, optimistic lead switch, immediate persistence, adapter actions |
| `tui/view.go` | Connecting state, lead-aware overview/status, new sidebar states |
| `tui/design.go` | Shared palette, panels, modal styling |
| `tui/commands.go` | Command palette entries |
| `tui/leadpicker.go` | Configured-agent lead picker |
| `tui/adapterpicker.go` | Adapter management modal |
| `tui/leadpersist_test.go` | Immediate lead persistence test |
| `internal/driver/acpdriver/managed.go` | Managed adapter install/uninstall |
| `internal/driver/acpdriver/managed_test.go` | Adapter manager tests |
| `internal/planning/planning.go` | Streaming callback for plan output |
| `README.md` | TUI commands and current status |
| `docs/architecture.md` | Phase 2 status updates, adapter manager, execution model |
| `internal/execution/` | Milestone A: `Next`, `Recover`, `RunNext`, `Run`, worker prompt, tests |
| `internal/driver/acpdriver/turn.go` | Shared `Session.RunTurn` and `PermissionRequest.Resolve` used by planning and execution |
| `internal/chat/chat.go` | `SwitchAgent`/`NewSession` return a new `*Chat` instead of mutating from a goroutine |
| `tui/job.go` | Shared background-job stream for `/plan` and `/run` (esc cancels, permission modal) |
| `tui/job_test.go`, `main_test.go` | `/run` and worker-permission routing; configured-agent-only lead |

Also present but untracked: `.switch/` (project config and rules generated during development). Decide whether this belongs in the repository, should be ignored, or should remain local-only.

---

## 4. Architecture snapshot

```text
TUI
  -> chat.Chat (one live user-facing ACP session)
  -> planning.Execute (lead prompt -> validated plan -> run/worktrees/tasks)
  -> SQLite (sessions, messages, project settings, runs, agents, tasks)

acpdriver
  -> OpenCode / Codex / Claude / OpenClaw adapter process
  -> ACP session, events, permissions, resume
```

Current package map:

| Package | Responsibility |
|---|---|
| `internal/driver/acpdriver` | Adapter registry, ACP client, probe, managed installation |
| `internal/chat` | One resumable chat session |
| `internal/chatcli` | Hidden chat control command |
| `internal/config` | TOML config, fleet, adapter selection |
| `internal/rules` | Rule discovery and prompt compilation |
| `internal/store` | SQLite migrations and queries |
| `internal/plan` | Plan schema and validation |
| `internal/planning` | Plan generation/import and run provisioning |
| `internal/worktree` | Git worktree creation/removal |
| `internal/doctor` | Readiness diagnostics |
| `tui` | User-facing Bubble Tea application |

**Deliberate constraints**

- TUI is the only user-facing surface; automation stays behind `switch __control` until the control plane exists.
- Switch does not call models directly.
- Agents are external ACP subprocesses; no PTY/screen scraping.
- Work happens in git worktrees.
- No auto-commit, auto-push, or merge without approval.
- SQLite and the filesystem are the source of truth; the TUI is a projection.

---

## 5. Important gaps to fix before or during the next phase

These are not just roadmap items; some are correctness issues in the current state.

### 5.1 Enforce configured-agent-only lead selection everywhere — DONE

`/lead <name>` and startup (`resolveLead` in `main.go`) only accept configured agents; stale adapter names are ignored. Tests: `TestLeadCommandRejectsBareAdapterName`, `TestResolveLeadIgnoresBareAdapterName`.

The `/lead` picker is configured-only, but the `/lead <name>` fallback in `tui/update.go` still searches ACP adapters. Startup also has an adapter fallback for stored lead names. The requirement is that the lead is a configured agent, not a bare adapter.

- Remove the adapter fallback from the slash command.
- Remove or explicitly migrate adapter names stored by older builds.
- Add a test that a bare adapter name cannot become the lead.

### 5.2 Decide worktree ownership for tasks — DECIDED: serialize per agent

Each run agent keeps its one worktree; `execution.Next` never gives an agent a second task while one is `running`. Revisit if one agent needs parallel tasks.

`/plan` currently creates one worktree per configured agent. Tasks have `branch` and `worktree` columns, but assignment is not implemented. Two concurrent tasks assigned to the same agent would share a worktree and collide.

Choose one:

- **serialize per agent** and reuse the agent worktree, or
- create one worktree per task for isolation.

The architecture currently says "one worktree per agent/task", so this needs to be settled before the scheduler.

### 5.3 Chat history is not scoped by agent

`chat_sessions` is keyed only by project, and `chat_messages` has no agent column. Switching leads overwrites the single session and the conversation view may show messages from a previous agent. Decide whether to:

- add an agent column and scope sessions/messages by agent, or
- intentionally keep one conversation and clearly label agent changes.

### 5.4 Startup connection failure has no retry — DONE (enter retries)

If the lead adapter is unavailable, the TUI shows an error but the input path is effectively blocked until `ctrl+c`. Add a retry key or a `/reconnect` path.

### 5.5 Cold-start latency remains

Lead switching is visually immediate, but a new ACP process still starts in the background. `/adapters` can install Codex/Claude globally to avoid `npx`; a future improvement is prewarming one process per adapter or a session/agent pool.

### 5.6 Legacy code cleanup

These tracked packages are not imported by the current product path:

- `api/`
- `config/`
- `memory/`
- `minds/`
- `msgs/`
- `tui/conversation/`
- `tui/input/`
- `tui/minds/`
- `tui/statusbar/`

They appear to be prototype leftovers. Review and delete or quarantine before the codebase grows further.

### 5.7 Documentation drift — DONE

`docs/architecture.md` says adapter installation is deferred, but `/adapters` and managed npm installation now exist. Reconcile the architecture doc and README before handoff.

---

## 6. What is left to build

### Milestone A — Execute one task end to end — DONE (2026-11-03)

Verified live with an OpenCode worker in a throwaway repo: file created in the agent worktree, task `implemented` with session ID persisted, no orphan adapter after completion or cancellation, and `/run` after a cancel resumed the `interrupted` task. Worker permissions use the TUI modal. No `__control run` headless path yet.

**Goal:** prove the execution spine before adding concurrency.

Tasks:

1. Add an execution package (for example `internal/execution` or `internal/orchestrator`).
2. Assign one queued task to one configured worker.
3. Build the worker prompt from task brief, acceptance criteria, verification commands, and effective rules.
4. Start an ACP session in the worker worktree.
5. Stream worker events to the TUI.
6. Persist task state transitions and the worker session ID.
7. Support cancellation and process-exit handling.
8. Add a TUI command to start/resume a run (for example `/run`).

**Acceptance:**

- A plan file can create a run.
- One worker executes one task in its worktree.
- Output streams in the TUI.
- Task status and session ID survive restart.
- Cancellation stops the worker without leaving an orphan process.

---

### Milestone B — DAG scheduler and concurrency

**Goal:** run multiple tasks safely.

Tasks:

1. Implement readiness checks from task dependencies.
2. Match tasks to workers by role and availability.
3. Enforce a configurable maximum concurrency.
4. Serialize all task transitions through one run actor.
5. Add retry/attempt handling.
6. Mark `running` tasks as `interrupted` on startup after a crash.
7. Decide and implement the worktree-per-agent vs worktree-per-task model from 5.2.
8. Add queue/scheduler tests, including dependency order and cycle-free plans.

**Acceptance:**

- Independent tasks run in parallel within the configured limit.
- Dependent tasks wait for their prerequisites.
- Two tasks never write to the same worktree concurrently.
- A restart does not silently leave tasks stuck in `running`.

---

### Milestone C — Verification runner

**Goal:** never trust a worker's claim of completion.

Tasks:

1. Add `internal/verify`.
2. Execute each task's `verify` commands in the task worktree.
3. Capture exit code, stdout, stderr, duration, and relevant artifacts.
4. Persist evidence in SQLite or the filesystem.
5. Send verification failures back to the same worker.
6. Enforce an attempt limit, then block or reassign.
7. Add tests for pass, fail, timeout, and missing commands.

**Acceptance:**

- A task cannot reach `review` without passing verification.
- Failure evidence is visible to the worker and the lead.
- Retry limits are enforced.

---

### Milestone D — Lead review, merge queue, and integration verification

**Goal:** safely integrate approved work.

Tasks:

1. Extend `internal/worktree` with diff, commit, merge, and cleanup operations.
2. Capture the worker diff and send it to the lead for review.
3. Add a TUI approval modal for approve/reject.
4. Implement a serialized merge queue into the run integration branch.
5. Run integration verification after each merge.
6. Handle merge conflicts by blocking and reporting, never auto-resolving.
7. Remove merged worktrees safely.
8. Enforce "no push" and "no merge without approval" in code.

**Acceptance:**

- Approved tasks merge one at a time.
- Conflicts block the task and notify the lead.
- Integration checks run after merge.
- No push occurs without an explicit user action.

---

### Milestone E — Control plane, agent CLI, and MCP bridge

**Goal:** let agents and automation interact with the engine without using the TUI.

Tasks:

1. Add a Unix socket at `~/.local/state/switch/runs/<id>/control.sock`, mode `0600`.
2. Add per-agent authentication tokens in `.switch-run` files.
3. Implement handlers for claim, progress, complete, block, and permission responses.
4. Add the internal agent-facing CLI path.
5. Add an MCP stdio bridge that uses the same handlers.
6. Keep `switch __control` as a compatibility wrapper until clients migrate.
7. Add role authorization: workers cannot delegate or approve their own work.

**Acceptance:**

- A worker can report status through the control plane.
- The TUI sees the same state transition.
- Unauthorized agents are rejected.
- A dropped socket does not stop the run actor.

---

### Milestone F — Run, task, and agent TUI views

**Goal:** make execution observable.

Tasks:

1. Expand the dashboard into run/task/agent views.
2. Show queued, running, verifying, review, and merged tasks.
3. Show per-agent session state, worktree, current task, and last error.
4. Add diff and verification evidence views.
5. Add approval modals for review and merge.
6. Keep the existing design system and true centered modals.
7. Add a command to start or resume a run.

**Acceptance:**

- The user can see what the lead and every worker are doing.
- Every transition is visible without leaving the TUI.
- Approval and merge actions are explicit.

---

### Milestone G — Rules and context scopes

**Goal:** apply the right instructions to the right role and task.

Tasks:

1. Add role and task rule discovery.
2. Define precedence: global → project → local → role → task.
3. Compile role/task rules into worker prompts.
4. Add an effective-rules view.
5. Add tests for precedence and missing files.

**Acceptance:**

- A builder receives builder/task rules, not lead-only rules.
- The effective-rules view matches the prompt actually sent.

---

### Milestone H — Reliability, headless operation, and distribution

**Goal:** make Switch usable beyond a single interactive session.

Tasks:

1. Crash recovery and process/orphan cleanup.
2. Event/audit log and usage/cost persistence.
3. Budget limits and runaway-loop protection.
4. Headless `switch run` using the same engine.
5. Daemon/detach mode if still desired.
6. Packaging and release workflow.
7. OpenClaw adapter verification.
8. Windows is out of scope for v1.

**Acceptance:**

- A run survives a TUI restart.
- Interrupted tasks are recoverable and visible.
- Headless execution produces the same state as TUI execution.

---

## 7. Recommended handoff sequence

### First day

1. Review the entire uncommitted diff.
2. Run `go build ./...`, `go test ./...`, and `go vet ./...`.
3. Run the TUI in a real terminal and verify:
   - startup shows `CONNECTING` immediately,
   - `/lead` switches and persists,
   - quitting during a lead switch preserves the choice,
   - `/adapters` opens and confirms correctly,
   - `/plan --file` creates a run and tasks.
4. Decide the `.switch/` tracking question.
5. Commit the current TUI/lead/adapter work **only if the user asks**.
6. Reconcile `docs/architecture.md` with the implemented adapter manager.

### First sprint

1. Fix 5.1 (configured-agent-only lead).
2. Decide 5.2 (worktree ownership) and write it down.
3. Implement Milestone A: one task end to end.
4. Add the first execution test.

### Next sprint

1. Milestone B: scheduler and dependency handling.
2. Milestone C: verification runner.
3. Start Milestone D: diff and lead review.

Do not start the control plane before single-task execution and verification work, because the control plane needs stable task transitions to expose.

---

## 8. Testing checklist for the next engineer

Run before every handoff:

```bash
go build ./...
go test ./...
go vet ./...
```

Manual TUI smoke test:

1. Start `./switch` in a git repository.
2. Confirm the shell appears before the lead connects.
3. Send a prompt and confirm streaming plus final status.
4. Open `/lead`, switch to another configured agent, and confirm the header/sidebar update immediately.
5. Quit during the switch; restart and confirm the new lead is still selected.
6. Open `/adapters`; confirm the modal, confirmation text, and external-management behavior.
7. Run `/plan --file <plan.json>` and confirm run, agent worktrees, and queued tasks.
8. Confirm no worker has been dispatched yet (current limitation).
9. Quit and confirm no orphan adapter processes remain.

Useful state checks:

```bash
sqlite3 ~/.local/state/switch/switch.db \
  "select project_path, lead_agent from project_settings;"

sqlite3 ~/.local/state/switch/switch.db \
  "select id, status, goal from runs order by created_at desc limit 5;"

sqlite3 ~/.local/state/switch/switch.db \
  "select id, title, status, assignee from tasks order by rowid desc limit 20;"
```

---

## 9. Handoff checklist

- [ ] Current diff reviewed and intentionally committed or left local.
- [ ] `.switch/` tracking decision made.
- [ ] `go build ./...`, `go test ./...`, `go vet ./...` pass.
- [ ] TUI smoke test passes in a real terminal.
- [ ] Lead persistence verified across restart.
- [ ] Adapter install/uninstall flow verified.
- [ ] Architecture docs reconciled with implementation.
- [ ] Worktree ownership decision recorded.
- [ ] Next milestone (single-task execution) has an owner and acceptance tests.
- [ ] No auto-push or auto-merge behavior was introduced.

---

## 10. Open decisions

1. **Worktree model:** decided: one per agent, tasks serialized (see 5.2).
2. **Lead persistence scope:** per project (current) or global fallback?
3. **Conversation model:** one shared project transcript or per-agent transcripts?
4. **Run start:** explicit `/run` (current); revisit with the scheduler.
5. **Control plane timing:** before or after verification/review is stable?
6. **OpenClaw:** support now or defer until after the core execution loop?
7. **Legacy packages:** delete now or keep for reference until v1?
