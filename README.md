# Switch

> **⚠️ Early development — the v1 execution loop works end to end (plan → run → verify → review → approve → merge). Expect rough edges.**

Switch is a terminal orchestrator for a team of existing coding agents. It does not call models itself. It launches vendor CLIs as subprocesses, gives each one a role and an isolated workspace, and lets exactly one agent — the lead — plan, delegate, review, and talk to you. You converse with the lead; `@<name>` asks a teammate directly.

The design is vendor-agnostic and built on the [Agent Client Protocol](https://agentclientprotocol.com). OpenCode, Codex, Claude Code, and OpenClaw are first-class targets; any other ACP agent is added as a `[drivers]` entry in your config.

## Status

| Layer | State |
|---|---|
| ACP driver + probe | Working across OpenCode, Codex, and Claude; OpenClaw verified as a worker; custom ACP agents through `[drivers]` |
| TUI chat with the lead | Streaming, history, permissions, resume, lead switching, queued messages, `@` questions to teammates |
| Config, rules, SQLite state | Working; global/project/local plus role and task rules; project rules are sent only as you accepted them |
| Planning | Lead-generated or imported plans become runs with an integration branch; a rejected lead plan gets one fix-up turn |
| Scheduler | Ready tasks run in parallel up to `max_concurrency`, one per worker; tasks with overlapping file scopes wait |
| Verification | `[run] setup`, task `verify` and `[run] verify` commands; a failing command is rerun once; evidence is stored |
| Lead review + approval | The lead reviews each verified diff; you approve (one task or all) or reject |
| Merge queue | Approvals merge one at a time; the integration branch moves only once the integration checks pass |
| Control plane | Per-run Unix socket, agent CLI, and MCP bridge offering each agent the tools of its role |
| Reliability | Crash recovery, orphan adapter cleanup, audit log, token usage, budgets, idle-turn watchdog |
| Headless | `switch __control run\|status\|stop\|tasks\|approve\|reject\|retry\|diff\|log\|summary\|prune` |
| Detached runs | `/run --detach` or `__control run --detach`; survives the TUI; one process per run (lock) |
| Packaging | GoReleaser config and GitHub Actions CI/release workflows |

See [`docs/architecture.md`](docs/architecture.md) for the full architecture, language choice, phases, and risks.

## Requirements

- Go 1.26.2+ (the repo declares `go 1.26.2`; Go 1.27 is installed on the development machine)
- git; 2.38+ for the conflict check before lead review
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
`.switch/rules.md` on first run in a project; every setting in that config,
the example team included, is commented out, so the project uses your global
config, then the defaults. The project is the nearest
folder with a `.switch` folder, up to the git root, or else the git root, so
`switch` started in a subfolder opens the same project; the `__control` run,
task, plan, chat and rules commands do the same. Outside a git repository,
the project is the folder you start in. When the project is a
subfolder of its repository (a package in a monorepo), run agents and checks
work in that subfolder of each worktree.

### TUI commands

| Command | Purpose |
|---|---|
| `/plan [goal]` | Ask the lead to plan and create a run; with no goal, it plans from the conversation so far |
| `/plan --file <path>` | Import a plan file (see [Plan files](#plan-files)) |
| `/run` | Start or resume the run; workers stream into the run log. On a completed run with nothing left to do it says so and points to `/land` |
| `/mode [build\|plan\|full]` | Show or change the current session's tool-request policy |
| `/parallel [count]` | Show or set the foreground run's simultaneous worker-task limit |
| `/run --detach [--allow]` | Run in a background process that outlives the TUI; worker requests that `build` mode would ask about, edits included, are denied (or allowed); only reads inside the worker's directory are always allowed |
| `/stop` | Stop the run; running tasks become `interrupted` and `/run` resumes them |
| `/tasks` | Task list: enter for details, `a` approve, `r` reject, `d` diff, `t` retry |
| `/review` | The task list, opened at the first task waiting for approval |
| `/approve <task\|all>` | Merge a reviewed task into the integration branch; `all` merges every waiting task in plan order and stops at the first that fails |
| `/reject <task> <reason>` | Send a task back to its worker with your reason |
| `/retry <task>` | Requeue a blocked or interrupted task with a fresh attempt budget and session, for any worker of its role |
| `/fix <task>` | Finish a blocked, queued, interrupted or review task by hand in a shell in its worktree (its first line says to exit when done); when you exit, Switch commits, re-runs its checks at no attempt cost, and puts it in review or blocks it |
| `/diff <task>` | Show a task's change against the integration branch, in color; for a merged task, what its merge brought in |
| `/why` | Show why each unfinished task waits |
| `/land` | On a completed run, preview and then run `git merge --no-ff` of the integration branch into your current branch; nothing is pushed. The preview names the branch's commit and shows `git diff --stat HEAD...<branch>`, what the merge brings in |
| `/agent <name>` | Follow a worker, subagent, or standby lead live |
| `/steer <agent> <text>` | Redirect a worker or teammate mid-turn, or at its next turn if its adapter cannot take it mid-turn |
| `/agents` | Run agents, what each is doing, last errors, and token usage |
| `/log` | While `/run` runs here, its output since it started (once past 64 KB, only the last 32–64 KB); otherwise the run's latest 40 events |
| `/copy` | Copy the last reply in view to the clipboard (OSC 52); `/copy code` copies the newest code block |
| `/doctor` | Check config, rules, and state, and start each team agent's adapter once |
| `/init` | Create `.switch` config and rules explicitly |
| `/rules` | Manage rule files; `/rules <agent>` shows what that agent gets |
| `/remember <rule>` | Append `- <rule>` to this project's local rules file |
| `/session` | Show this project's sessions, current first: enter switches, n starts a new one |
| `/lead <agent>` | Choose which configured agent leads and persist it; it gets a recap of the recent conversation |
| `/adapters` | Install or uninstall ACP adapters (not during a run) |
| `/new` | Start a new session with an empty transcript |
| `/resume` | Same as `/session`; a past session continues with the agent that owned it |
| `/zen` | Hide or show the sidebar (also `ctrl+o`) |
| `/home` | Return to the dashboard |
| `/help` | Show commands |
| `/quit` | Exit (also `/exit` or Ctrl+C) |
| `@<teammate> <message>` | Ask a teammate directly, without the lead or its context; tab completes the name |

Each launch starts a new session. When you quit, Switch prints the command
that reopens the one you were in (`switch --resume <id>`, run from the same
folder); if the agent cannot reopen it, Switch says so and starts a new one.

Messages sent while the lead works, connects, plans, or changes session wait
as `you · queued` and go when it is free. Esc stops the lead's turn and puts
queued messages back in the input. Slash commands run while the lead works,
or say why they must wait and stay in the input. Paste (the terminal's, or
Ctrl+V) goes into the input, with newlines turned into spaces. An `@` question
shows in the conversation, and the teammate's answer joins it under its name;
tab follows the answer live.

Use Page Up/Page Down (half a page) or Shift+Up/Down (a line) to move through
output and Ctrl+Home/Ctrl+End to jump to the beginning or latest output;
Up/Down also scroll a line when no list is open.
Fenced code in agent replies is highlighted and framed; a block still arriving
is marked “streaming”. Command output is collapsed by default; press Ctrl+E to
show or hide it, and Ctrl+T for thinking. File reads and edits remain visible.

The mouse wheel scrolls the conversation, agent view, or open reading panel,
including inside tmux with mouse mode enabled. When mouse reporting is active,
use your terminal's selection modifier (often Shift) to select text.

While the terminal is in the background, Switch rings the bell and sends a
desktop notification (OSC 9) when a permission prompt opens, a task becomes
ready for review, the run ends, or a lead turn longer than 30 seconds
finishes. This needs focus reporting (tmux: `set -g focus-events on`) and a
terminal that shows OSC 9. The window title shows what waits on you:
`▲ permission · switch`, `● 2 to review · switch`, or `switch`. `/copy` needs a
terminal that allows OSC 52 (tmux: `set -g set-clipboard on`).

An agent's question (`switch_question`) shows in the same queue as permission
prompts: pick an option by number or type an answer, and esc skips it.
Questions always wait for you, whatever the mode.

### How a run works

1. `/plan` asks the lead for a JSON plan (or imports one) and creates a run
   with an integration branch `switch/<run>/integration` off your `HEAD`.
   Workers start from the last commit, so the plan summary warns about
   uncommitted changes. The lead that plans the run leads it: it reviews and
   takes default handoffs, and other `role = "lead"` agents are on standby. A
   local branch named `switch` blocks planning; rename it.
2. `/run` dispatches every ready task (dependencies merged, a free worker of
   the task's role, and no started, unmerged task whose file scope overlaps
   it) up to `max_concurrency`. Each task gets its own worktree, branched from
   the integration branch, so it sees its dependencies' work. `[run] setup`
   runs there before the task's first attempt, again after `/retry`, and when
   an interrupted first attempt resumes; a failure blocks the task.
3. When the worker's turn ends, Switch commits its change on the task branch,
   runs the task's `verify` commands on that commit, and removes what they
   wrote. A failing command runs once more from a clean checkout before it
   counts. A failure, a task that changed nothing, or a conflict with work
   merged since the task started (Switch merges the integration branch into
   the worktree, leaving the conflict markers) goes back to the same worker
   with the evidence, as does a worktree that still holds conflict markers,
   with the files named; Switch never commits the markers. After `max_attempts` the task goes to its worker's
   `escalate` agent for one more attempt, or is `blocked`.
4. The lead reviews the diff and evidence (`lead_review`); the first line of
   its reply is the verdict. A REJECT sends the task back like a failure. A
   task the lead worked on itself skips review.
5. The task waits in `review` for you. `/approve` merges it in the integration
   worktree, runs `[run] setup`, the task's `verify` and `[run] verify` there,
   and moves the integration branch only if they pass; otherwise nothing is
   merged and the task is blocked. A conflict blocks the task and merges the
   integration branch into its worktree, so `/retry` has a worker resolve the
   markers; `[run] setup` is skipped until they are resolved. Approvals queue and merge one at a time. Dependents start once
   their dependencies are merged.
6. When every task is merged, `[run] verify` runs once more on the integration
   branch, and the run is `completed`, or `blocked` if it fails. When no
   process drives the run (such as with `__control approve`), the approval
   that merges the last task completes it on the spot. Tasks the chat lead
   proposed that still wait for `/retry` do not hold completion back. Switch
   never pushes, and it merges into your branch only when you run `/land` on
   a completed run: `/land` shows the merge first, and enter on an empty
   input or `/land` again runs it. Esc, any other key, or enter with text in
   the input cancels it, and the next `/land` previews again. It refuses a
   dirty checkout, a detached HEAD, or a branch that moved since the preview,
   and aborts on a conflict. You can also merge the integration branch yourself.

Stopping, quitting, a crash or running out of budget interrupts running
tasks without using an attempt; `/run` resumes them. Chat with the lead stays
available during a run, but `/plan`, `/run` and `/adapters` are refused
until it ends, detached runs included. Run settings live under `[run]` in
`.switch/config.toml` (see [Configuration](#configuration)). The TUI reads
them again for each `/run`, approval and other task action, so an edit such
as a raised `max_turns` applies without a restart; `max_concurrency` stays
the launch or `/parallel` value, and an invalid config makes those commands
fail with its error.

### Plan files

`/plan --file <path>` and `switch __control plan --file <path>` import a plan
instead of asking the lead. The file holds one JSON object; prose or a code
fence around it is ignored.

```json
{
  "goal": "Add account deletion",
  "tasks": [
    {"id": "schema", "title": "Add audit table", "brief": "Create the migration and store methods.", "role": "builder",
     "files": ["internal/store/**"], "acceptance": ["migration applies cleanly"], "verify": ["go test ./internal/store/..."]},
    {"id": "api", "title": "Add delete endpoint", "brief": "Implement the endpoint.", "role": "builder", "agent": "builder-2",
     "deps": ["schema"], "acceptance": ["204 on success"], "verify": ["go test ./internal/api/..."]}
  ]
}
```

- `goal` and 1 to 32 tasks are required.
- Each task needs `id`, `title`, `brief`, `role` and at least one `acceptance`
  criterion.
- Ids name git branches (`switch/<run>/task-<id>`): at most 40 letters,
  digits, `.`, `_` or `-`, starting and ending with a letter or digit, with no
  `..` and no `.lock` ending, and unique ignoring case.
- `role` must be one that a configured agent other than a lead has.
- Optional `agent` gives the task to that worker; `/retry` hands it to any
  worker of its role.
- `deps` name other tasks in the plan; cycles are rejected. `files` lists the
  paths or globs the task should touch. `verify` commands run in the task's
  worktree and again on the integration branch at its merge.
- Unknown fields are rejected.

A lead plan that fails these checks gets one turn to fix it. If it still
fails, both answers are saved in `~/.local/state/switch/plans/rejected-*.txt`
and the error names the file: fix either copy and load the file with `--file`.

### Configuration

Switch reads your `~/.config/switch/config.toml`, then the project's
`.switch/config.toml`, whose settings win; a project's `[[agents]]` list
replaces yours whole. `~/.config` and `~/.local/state` follow
`XDG_CONFIG_HOME` and `XDG_STATE_HOME` when they are set. Unknown keys, a
`handoff_at` outside 0–100, and agent names or roles with spaces, slashes or
control characters are errors.

The top-level `agent` (default `opencode`) and `model` (default: the
adapter's own) are the team's default driver and model; `SWITCH_AGENT` and
`SWITCH_MODEL` override them. Each `[[agents]]` entry is one team member;
with none in either file, the team is `lead`, `builder-1` and `builder-2`,
all on `agent`.

| `[[agents]]` key | Default | Purpose |
|---|---|---|
| `name` | required | Unique; names the agent in `/lead`, `@`, rule files and plans |
| `role` | `builder` | Tasks go to workers by role; agents with role `lead` take none and lead or stand by |
| `driver` | `agent` | A built-in adapter or a `[drivers]` entry |
| `model` | `model`, when `driver` is `agent` | The model the agent's sessions select |
| `handoff` | the lead | Who continues this agent's work (see [Limits and handoffs](#limits-and-handoffs)) |
| `escalate` | none | Who gets one more attempt when this agent's task runs out of attempts |

| `[run]` key | Default | Purpose |
|---|---|---|
| `max_concurrency` | 2 | Tasks running at once |
| `max_attempts` | 3 | Attempts per task before it is escalated or blocked |
| `lead_review` | true | The lead reviews each verified diff before you do |
| `verify_timeout` | 600 | Seconds per check or setup command |
| `turn_timeout` | 1800 | Seconds per agent turn in a run |
| `idle_timeout` | 600 | Seconds a worker turn may go with no activity from the worker or its subagents before it is stopped and retried; paused while one of them waits on a permission prompt |
| `max_turns` | 50 | Agent turns per run: worker turns, lead reviews, and subagent turns (0 = unlimited) |
| `max_tokens` | 0 | Tokens per run (0 = unlimited) |
| `handoff_at` | 80 | % of its context at which an agent is told to hand off (0 = never) |
| `setup` | none | Commands run in each task worktree before its first attempt, and in the integration worktree before its checks |
| `verify` | none | Commands run on the integration branch with every merge's checks, and once more before the run completes |
| `notify` | unset | Global config only; a project config that sets it fails to load. Shell command (`sh -c`) run when a detached or headless run needs you; `SWITCH_EVENT` is `review`, `blocked`, `completed` or `question`, with `SWITCH_RUN`, `SWITCH_TASK` and `SWITCH_MESSAGE` (first line) set. It gets 10 seconds and is not waited on. `SWITCH_MESSAGE` holds agents' words: pass it as an argument, never into script source. The TUI notifies on its own |

Workers, their subagents, checks and setup get `SWITCH_SLOT`, `SWITCH_TASK`
and `SWITCH_RUN`.
`SWITCH_SLOT` is unique among running tasks (integration checks use 0):
derive ports, test databases and container names from it. Before a failed
check reruns, Switch resets the worktree (`git reset --hard`,
`git clean -fd`), so files a check needs from setup or an earlier check must
be gitignored; Switch commits everything else in a task worktree, so
gitignore what setup writes. A timed-out command is not rerun; a pass on the
rerun logs a `flaky` event.

To run an ACP agent Switch has no adapter for, define it in your own config:

```toml
# ~/.config/switch/config.toml
[drivers.mine]
command = ["my-agent", "acp"]
env = { MY_AGENT_LOG = "warn" }   # optional
```

Then `driver = "mine"` works in any `[[agents]]` entry and in
`[run.subagents] drivers`: for the lead, teammates, workers, reviews and
subagents, headless and detached runs included, and
`__control probe --agent mine` tries it. A project config that sets
`[drivers]` is refused: a repository may not choose what Switch runs. A
driver named like a built-in (`opencode`, `codex`, `claude`, `openclaw`)
replaces how that adapter is launched and keeps its special handling;
`/adapters` then shows it as managed externally. Driver names ignore case,
and `claude-code` is `claude`.

### Permission modes and parallelism

Switch controls the requests an ACP adapter sends; it is not a sandbox and
cannot intercept operations the adapter performs without asking. Modes apply
to this interactive session and reset to `build` when Switch restarts:

- `build`: allow reads and searches whose paths are all inside the agent's
  own directory; ask about everything else, a run worker's edits inside its
  task worktree included, since verify runs what a worker wrote before you
  approve it.
- `plan`: allow those reads, deny everything else, and prevent starting
  worker runs. Use this to explore and plan; it cannot enforce read-only
  behavior against an adapter that does not ask permission.
- `full`: automatically allow reported tool requests. Select explicitly with
  `/mode full`; it applies for the rest of this session until changed.

Chat teammates and subagents, in the chat or a run, get the read allowance
only, and ask before edits even in `full` mode. A question from an agent
always waits for you.

Automatic decisions select one-shot options only. They never grant a
persistent adapter approval. User approval is still required to merge each
task. Detached/headless runs cannot present a prompt: what `build` would ask
about, worker edits included, gets their `--permission` or `--allow` answer
(denied by default). The TUI answers
the planning lead as in `plan` mode, and a lead review is denied every
request.

Prompts from the lead, workers and teammates queue and show one at a time,
with how many more wait; the other agents keep working meanwhile. A prompt
takes an option's digit or esc only once it has been on screen for half a
second with no typing; one pressed sooner is ignored, and other keys go to
the input. Esc picks the adapter's reject option. When a run stops, its workers' prompts are withdrawn.

`/parallel 4` changes the foreground run's maximum concurrent worker tasks for
the current session; it does not add agents. The number able to run is also
limited by configured workers and task dependencies. It does not affect a
detached run or the separate `[run.subagents] max_per_builder` limit. Change
the project configuration for persistent limits.

### Rules

Rules apply in three scopes; later ones win on conflict:

| Scope | Files | For |
|---|---|---|
| Global | `~/.config/switch/rules.md`, `rules/roles/<role>.md`, `rules/agents/<agent>.md` | Your conventions in every project |
| Project | `.switch/rules.md`, `.switch/rules/{roles,agents,tasks}/<name>.md` | The repository's rules, committed |
| Local | `.switch/rules.local.md` | Your private rules for this repository |

Each agent gets the base files plus its role, its own name, and (in a run) its
task file. Whoever is leading gets `roles/lead.md`. `/rules` lists the files by
scope and opens them in `$VISUAL`/`$EDITOR`; `/rules <agent>` shows exactly what
that agent is given. `/remember <rule>` appends `- <rule>` to the local file;
agents get it with their next message. HTML comments are notes and are never
sent; terminal escapes, control characters and invisible Unicode are removed.
Set `ignore_global = true` under `[rules]` to leave your global rules out of a
project. `project`, `local` and `global` under `[rules]` move the files: `~` is
your home, and a relative path is relative to the project folder (for
`global`, to `~/.config/switch`). Only your own config sets `global`; a
project's is ignored. Headless:
`switch __control rules [list|show [agent]|path <file>|edit <file>]`; `show`
leaves out project rules you have not accepted, as agents get them.

The lead is sent its rules with the first message of a session and again only
when the files change. Project rules (every rule file of the project, local
ones included) come from the repository, so Switch sends them only as you
last accepted them; until then every prompt leaves them out. The TUI shows
rules you have not accepted at launch, and within about 2 seconds of a change
(a `git pull`, an agent editing them); enter accepts exactly the text shown,
and esc leaves them out for that session, detached runs included. An edit
through `/rules` counts as accepting them only if the editor succeeded, they
were accepted before, and no other rule file changed meanwhile; `/remember`
keeps them accepted. Headless `__control run`, `chat` and `plan` leave out
project rules you have not accepted, with a note;
`__control run --no-project-rules` leaves them all out.

The TUI uses one design system across the dashboard, conversation, modals, and
command palette: shared colors, panels, section labels, input, and status bars.
The dashboard is built from live state: project, lead, driver, model, session,
rules, configured agents, latest run, task counts, commands, and keys. Use
`/lead <agent>` to choose any configured agent as the lead; the choice
persists across restarts. Each lead has its own session, so the transcript
starts fresh, and Switch sends the new lead a recap of the recent
conversation as its first turn. Slash commands operate over the dashboard
without clearing it; `/home` returns to it after chatting.

The TUI opens on the dashboard immediately while the lead connects in the
background; a message sent before it is ready is delivered once it connects.
Lead replies render as markdown, tool calls show as one line each that
updates in place, and the agent's plan shows as a checklist.

Once the lead is connected, Switch starts the other configured agents in the
background, so `/lead` switches instantly and switching back keeps the old
session warm; a teammate whose adapter exited starts again when needed. The
lead knows its team: each message carries a short team note, and the lead
can call `switch_team` (who is on the team) and `switch_ask`
(consult a teammate) through the Switch MCP server, or
`$SWITCH_BIN __control agent team|ask` from a shell. `switch_ask` returns
within 45 seconds; while the teammate is still answering it says so, and the
same question asked again collects the answer; it refuses a teammate while
that teammate works on a run task. Adapter installs run in the background
while you keep chatting.

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
./switch __control status                   # who drives the run, what each task waits for
./switch __control stop                     # stop a background run
./switch __control tasks
./switch __control diff <task>
./switch __control approve <task>           # or: reject <task> <reason>, retry <task>
./switch __control summary                  # the run as a Markdown PR description
./switch __control prune                    # remove finished runs' worktrees and branches
./switch __control log
./switch __control doctor                   # exits 1 if a team agent's adapter does not start
./switch __control probe --list
```

- Run and task commands act on the project's latest run; `--run <id>` picks
  another of the project's runs, and a unique prefix works, such as the 8 characters `plan` prints
  or the 12 `status` shows.
- `run`, `status` and `tasks` take `--json` and print
  `{run, status, goal, integration, owner, tasks: [{id, title, status, assignee, attempts, last_error}]}`;
  with `run --json`, everything else goes to stderr.
- `run` exits 0 when the run completed, 3 when tasks wait for review, 4 when
  it is blocked, 5 when its budget ran out, and 130 when stopped (1 on an
  error, 2 on bad flags). Like `status`, it ends by saying what each
  unfinished task waits for.
- A headless or detached run that completes, and an `approve` that merges the
  last task, write the PR description to `<state>/runs/<id8>/summary.md`
  (`<state>` is `~/.local/state/switch`) and print its path.
- `prune` removes the worktrees and `switch/<run>/` branches of completed runs
  and of older runs that a newer plan replaced, never those of a run a
  process drives, of one an approval is merging in, or of the current
  unfinished run. Without `--all` it keeps worktrees with uncommitted changes
  (new untracked files count), task branches not merged into their
  integration branch, and integration branches that no branch outside
  `switch/` contains. `run`, `approve`, `reject` and `retry` then refuse a
  pruned run: plan a new one.
- `stop` interrupts a headless or detached run; a run that a TUI drives is
  refused and is stopped with `/stop` in that TUI.

Agents reach Switch through the control plane. Chat sessions and run turns,
except lead reviews, which are read-only, are offered a `switch` MCP server
listing the tools the agent's role may call there. In a run: `switch_team`,
`switch_task`, `switch_progress`, `switch_complete`, `switch_block`,
`switch_limits`, `switch_handoff`, the subagent tools, `switch_question`
(workers ask you and wait without ending the turn; with nobody watching, it
says so and `[run] notify` fires), lead-only `switch_delegate` (it needs
acceptance criteria and a role that a worker has, and only proposes a task
that stays blocked until you accept it with `/retry <id>`), and lead-only
`switch_tasks` (the run's tasks and what each waits for). In the chat:
`switch_team` and `switch_limits`; the lead also has `switch_ask`,
`switch_handoff`, `switch_tasks`, `switch_delegate`, which only proposes a
task that stays blocked until you accept it with `/retry <id>`, and, while a
run is active, `switch_steer` (correct a worker's or teammate's running
turn). A teammate answering a question has `switch_progress` and the
subagent tools, and during a run `switch_question`. The same calls work from a shell as
`$SWITCH_BIN __control agent <method> <message>`, or with the tool's
arguments as one JSON object, such as
`$SWITCH_BIN __control agent handoff '{"to": "builder-2", "message": "..."}'`;
`switch __control agent` lists the methods. A run turn's token is revoked
when the turn ends.

Only you approve and merge: the control plane refuses both to every agent,
and `__control approve`, `reject`, `retry` and `prune` refuse to run in an
agent's process (`SWITCH_TOKEN` set). That refusal is a speed bump, not a
sandbox: agents run as you, and an agent can unset the variable.

### Limits and handoffs

Agents know how full their context window is when their adapter reports it:
the lead sees it on every message, workers through `switch_limits`, and both
are reminded on every Switch tool result once they pass `handoff_at` (80% by
default). Near the limit an agent calls `switch_handoff` with a note: what is
done, what is left, and where it was. Context use belongs to the agent's
current session, so a new session reads unknown until its adapter reports.

- **A worker** hands its task to another agent, which continues in the same
  worktree from the note. The lead can take a task too.
- **The lead** in the chat passes the lead role once its reply ends; the new
  lead gets the note and carries on. A teammate answering a question or
  working on a run task takes over once it is free, with one notice.
- **When a limit stops an agent first** (a usage, rate or context error, or
  the model's token limit), Switch hands off for it with a note saying why,
  plus the user's last message for the lead. In the chat it does not hand off
  to a lead that already hit a limit since your last message.

Each agent hands off to its `handoff` agent, or to the lead when it has none
(a lead without one falls back to a standby lead). An agent may also name
another. Every handoff of a task counts as an attempt, so `max_attempts`
stops agents passing it back and forth. When a task's attempts run out, for
a handoff, failed checks, a rejection or a worker error, its worker's
`escalate` agent gets one more attempt with the evidence; without one the
task is blocked with `attempt limit N reached: <reason>`.

```toml
[run]
handoff_at = 80        # % of context at which agents are told to hand off (0 = never)

[[agents]]
name = "builder-1"
handoff = "builder-2"  # who continues builder-1's work
escalate = "builder-2" # who gets one more attempt when builder-1's run out
```

### Subagents

The lead runs the workers; each worker can run its own subagents for parts of
its task (`switch_spawn`, `switch_wait`, `switch_message`, `switch_stop`,
`switch_subagents`). Subagents work in their worker's worktree, report only to
it with `switch_report`, and cannot start subagents of their own. A spawn
names the files the subagent may edit (none means read-only), and siblings
cannot claim the same files. A worker cannot complete its task while any of
its subagents runs, and any it leaves running when its turn ends are stopped.
`switch_wait` returns within 45 seconds with any still running; call it
again to keep waiting. `switch_message` queues a follow-up for a running
subagent or restarts a finished one. Subagent turns count toward the run
budget.

```toml
[run.subagents]
max_per_builder = 3   # running at once per worker (0 = off)
drivers = ["claude", "codex"]  # adapters workers may start (default: any)
```

In the chat, a teammate the lead consults with `switch_ask` can also start
read-only subagents to explore in parallel; they stop when it answers.
Teammates' reads go ahead, and anything else they or their subagents want to
do comes to you as a permission prompt naming the agent.

In the TUI, subagents appear under their worker in the sidebar, and standby
leads are listed with the workers. Tab cycles the main pane through the lead,
each worker, standby lead and subagent (Shift+Tab goes back, or use
`/agent <name>`); a worker's
view lists its subagents as cards, enter opens one, and esc goes back up.
While a run uses the chat lead (a task handed to it, or a review), the lead
is listed as a worker too. Each view streams that agent's thinking, tool
calls and reply live; ctrl+x stops what it is doing (a stopped worker's task,
or its `[run] setup`, is blocked for you to retry), and ctrl+t shows
collapsed thinking. Otherwise the views are read-only: the worker handles its
subagents. Rules for subagents are their worker's rules plus
`roles/subagent.md`. File claims are instructions: Switch keeps siblings'
claims apart but does not check edits against them.

Resume a probe session by passing the session ID printed by a previous run:

```bash
./switch __control probe --agent opencode --session <session-id> \
  --prompt "What did you just say? One short sentence."
```

### Probe flags

| Flag | Purpose |
|---|---|
| `--agent` | `opencode`, `codex`, `claude`, `openclaw`, a configured driver, or `custom` |
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
| `--close` | End the session on the agent when done (it cannot be resumed) |
| `--list` | List adapters, configured drivers, and local availability |

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

Switch runs a modular Go monolith. The engine owns runs, tasks, agents, rules, worktrees, verification, and review; the TUI is a client of that engine. Agents are persistent ACP subprocesses, one per agent instance, wrapped by a thin driver around [`coder/acp-go-sdk`](https://github.com/coder/acp-go-sdk). The lead is the agent you talk to (an `@` question goes to a teammate directly) and the only one allowed to delegate. Workers get isolated git worktrees and report structured results; Switch verifies them before the lead reviews them and you approve a merge.

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
- `internal/plan/` — plan schema, parsing, and DAG validation
- `internal/worktree/` — git worktrees, commits, merges, and conflict checks
- `internal/plancli/` — internal `plan` command; the TUI calls `internal/planning` directly
- `internal/execution/` — run engine: scheduler, worker turns, verification, lead review, merge queue
- `internal/verify/` — verification and setup command runner and evidence
- `internal/control/` — control socket, agent CLI, and MCP bridge
- `internal/runcli/` — headless run and task commands, `summary`, and `prune`

The `tui/` package is wired to the chat engine. `switch` opens the TUI; the hidden `__control` commands use the same engine for automation.

## Known limitations

- Each lead has its own chat session: switching leads starts a fresh transcript, and the new lead gets only a recap of the last 3,000 characters of the conversation.
- The TUI shows only the project's latest run: a new `/plan` hides an unfinished run (it warns once first). Headless commands reach older runs with `--run`.
- `/plan` creates the run's integration worktree and branch at once, not at the first `/run`, and nothing removes those of runs a newer plan replaced until `switch __control prune`.
- A run driven by the TUI does not write the PR description; `switch __control summary` prints it.
- `/adapters` does not show installed versions or offer a reinstall at the pinned one. A globally installed `codex-acp` or `claude-agent-acp` runs instead of the pinned npx package whatever its version; `/doctor` warns when they differ.
- Switch is not a sandbox: agents run as you, so the `SWITCH_TOKEN` refusal of `__control approve` and the like is only a speed bump, and subagents' file claims are not checked against their edits.
- With git older than 2.38 the conflict check before lead review is skipped; a conflict then shows at `/approve`.
- A plan's fix-up turn shares the planning timeout (10 minutes) with the first turn, and the TUI's plan panel lists the tasks of both answers.
- Headless and detached runs cannot ask for permissions: requests that `build` mode would ask about, worker edits included, are denied (or allowed) as chosen up front; only reads inside the worker's directory are always allowed.
- OpenClaw needs a running gateway and a model provider configured in OpenClaw itself; its bridge rejects per-session MCP servers, so OpenClaw workers report through `$SWITCH_BIN __control agent` only.
- Lead review needs a lead in the run; without one, review is left to you.
- OpenCode's default model may hit provider privacy settings; use `--model` to choose a permitted model.
- Claude adapter hooks/plugins can add stderr noise; use `--agent-logs` only when debugging.
- The ACP SDK is v0.x; it is pinned and wrapped behind the driver boundary.
- Windows is out of scope for v1.

## License

No license file yet.
