# Switch as an A2A server (design)

Status: proposal, nothing built. Spec: A2A 1.0 (a2a-protocol.org). SDK:
`github.com/a2aproject/a2a-go/v2` v2.9.0 (Apache-2.0, Go 1.26; the JSON-RPC
server does not link gRPC).

## Goal

Another agent or orchestrator hands Switch a goal for one repository. Switch
plans it, runs its team, and reports back through A2A: progress while it runs,
a pause when it needs a decision, and the diff and PR description when it is
done. The user keeps the TUI: an A2A-driven run is an ordinary headless run,
so `switch` in the same project shows it, and the user can approve, reject,
`/fix` or `/land` there as usual.

Not in scope: Switch calling remote A2A agents, multi-repository servers,
push notifications, the gRPC and REST bindings.

## Command

```
switch serve [--addr 127.0.0.1:7777] [--allow-approve] [--permission deny|allow]
```

- Serves the project of the current directory (the same root discovery as
  `switch`), one project per server.
- Listens on loopback only. A non-loopback `--addr` is refused: workers run
  code on this machine, so this is not a network service.
- Every JSON-RPC call needs `Authorization: Bearer <token>`. The token is
  generated at start, written to `<state>/serve.token` (0600) and printed once.
- The Agent Card is public at `/.well-known/agent-card.json`; the JSON-RPC
  endpoint is `/a2a`.

## Mapping

| A2A | Switch |
|---|---|
| task ID, context ID | run ID |
| `SendMessage` with a new task | plan the goal (as `__control plan`), then run it (as `__control run`) |
| `SendMessage` on an input-required task | an action on the run (below), then the run goes on |
| `SendStreamingMessage`, `SubscribeToTask` | the run's events as status updates while it runs |
| `GetTask`, `ListTasks` | the run, read from the store |
| `CancelTask` | `__control stop`: running tasks become interrupted, the run can resume |

| Run state | A2A task state |
|---|---|
| planning, running (a worker, check or review under way) | `working` |
| waiting for review, blocked, budget spent | `input-required`, with what waits and why (the `/why` reasons) |
| completed | `completed` |
| stopped | `canceled` |
| planning failed | `failed` |
| another run of this project is active | the new task is `rejected` |

An input-required status carries a text part (what waits, in plain words) and
a data part:

```json
{"run": "50b46a54-…", "status": "review",
 "tasks": [{"id": "create-hello-txt", "title": "…", "status": "review",
            "waits": "waits for your review", "diffstat": "hello.txt | 1 +"}]}
```

A completed task has two artifacts: `summary.md` (the PR description, the same
file `__control summary` writes) and `changes.diff` (base..integration). The
integration branch is named in the final message. Nothing is pushed and nothing
is merged into the user's branches: `/land` stays a human step.

## Actions on an input-required task

A follow-up message carries either a data part
`{"action": "approve", "task": "create-hello-txt"}` or the same as text, for
chat-style clients: `approve <task>|all`, `reject <task> <reason>`,
`retry <task>`, `diff <task>`, `continue`. Each one calls the engine method the
headless CLI uses (`Approve`, `Reject`, `Retry`, `Diff`), then the run goes on
until it next needs a decision. `diff` answers with the diff as an artifact
and leaves the task input-required.

`approve` merges into the run's integration branch, so it is refused unless the
server was started with `--allow-approve`. Without it the client is told to ask
the user, and the user approves in the TUI or with `__control approve`; the
A2A task sees the change and goes on.

## Permissions and questions

The run is headless: workers get build mode's answer inside their worktree and
the `--permission` answer elsewhere (default deny), as a detached run does.
`switch_question` answers "no user is watching", as in any headless run.

## Shape of the code

- `internal/a2aserve` (new): the Agent Card, the bearer-token check, and an
  `a2asrv.AgentExecutor`. `Execute` plans or applies the action, runs the
  engine with `Headless: true` until the run needs a decision, and yields
  status events, read from the run's event log, until it stops. `Cancel` stops
  the run.
- `main.go`: the `serve` command.
- Reuses `planning.Execute`, `execution.Engine` (`Run`, `Approve`, `Reject`,
  `Retry`, `Diff`, `Explain`), `runcli.Summary`, `worktree.Diff` and the run
  lock, so the TUI and `__control` keep working on the same run.
- Tests: the SDK's `a2aclient` against an `httptest` server with fake worker
  turns: a goal to `completed` with both artifacts; review becoming
  input-required; approve refused without `--allow-approve`; cancel; a second
  task rejected while a run is active; requests without the token refused.

Estimate: about 400 lines plus tests, one new dependency.

## Known limits (v1)

- `shortcut:` the SDK keeps A2A task history in memory, so after a server
  restart `GetTask` on an old ID fails, although the run itself is intact and
  `__control` sees it. Upgrade by giving the SDK a `taskstore.Store` backed by
  Switch's store.
- A worker's `switch_question` is not turned into input-required: the engine
  would have to stay mid-turn across A2A calls. A later version can keep the
  engine alive per task and answer questions from follow-up messages.
- No push notifications; `[run] notify` covers "the run needs you" for now.

## Decisions for the user

1. May an A2A client approve (merge into the integration branch)? Proposed:
   only with `--allow-approve`.
2. Default `--permission` for an A2A run: deny (proposed) or allow.
3. One project per server (proposed), or a `project` field per task.
