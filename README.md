# cake-subagents

One asynchronous subagent tool for [Cake](https://github.com/travisennis/cake):
durable named child conversations that run alongside the parent.

This is a standalone Cake toolbox plugin, not a Cake fork. Cake discovers the
executable from its global toolbox directory and registers it as
`tb__subagent`.

## Requirements

- Cake (`CAKE_BIN`, else `$HOME/bin/cake`, else `cake` on `PATH`). It must have
  a usable `default_model` unless every launch sets a model.
- Go 1.27.3 or newer to build
- Python 3 for the offline test suite

## Install

```sh
git clone git@github.com:travisennis/cake-subagents.git ~/Projects/cake/cake-subagents
cd ~/Projects/cake/cake-subagents
just build        # or: go build -o dist/cake-subagent ./cmd/cake-subagent
just install      # or: ./bin/install
just doctor       # or: ./bin/doctor
```

`bin/install` links only `subagent` into
`${XDG_CONFIG_HOME:-$HOME/.config}/cake/tools`, pointing at
`dist/cake-subagent`. Cake follows symlinks during toolbox discovery, so no
settings or environment changes are needed. Re-run `just install` after moving
the repository; re-run `just build` after changing the code.

The installer also removes old `subagent_async`, `subagent_status`,
`subagent_cancel`, `subagent_send`, `subagent_inbox`, `subagent_wait`, and
`subagent_peers` links, plus the old `subagent-inbox.sh` hook link, but only
when they still point into this checkout. Non-symlink files are never replaced
or deleted.

Two caveats:

- An explicit `[tools] enabled` list in `~/.config/cake/settings.toml` replaces
  the default tool set. If you use one, add `tb__subagent`. The old seven
  `tb__subagent_*` names no longer exist.
- Under `--sandbox read-only`, Cake skips toolbox discovery entirely. The
  `subagent` tool is unavailable in read-only runs, including read-only
  subagents they launch.

## The tool

| Action | Behavior |
| --- | --- |
| `message` | Resolve `agent`. If unknown, create the child, enqueue the message, start a supervisor, and return at once. If idle, enqueue and start. If running, enqueue for the next turn. If 64 messages are already waiting, return `queue_full`. If 64 children are open, a new name returns `capacity`. |
| `wait` | Drain pending reports and messages. Parent: block until something arrives or `timeout_seconds` expires (default 30, maximum 300); return `empty: true` immediately when no child is open and nothing is pending. |
| `status` | Drain pending reports, then list open children with name, child ID, state, last outcome, queued count, report rule, session ID, cwd, type, effective model/profile/effort, and last run paths. A child gets a `self` block. |
| `stop` | End the running turn, drop queued messages, and leave the child open with its conversation. Return the dropped count. The parent's stop never produces a report for that turn. |
| `close` | `stop`, drop queued messages, remove the name mapping, and mark the child closed. Files stay for audit. A later `message` to the same name starts a new child with no context. The parent's close never produces a report for that turn. |

`action` is required. `agent` is required for `message`, `stop`, and `close`,
and optional for `status` and `wait` (to filter one child). `message` is
required for `message`.

Names are lowercase letters, digits, `_`, or `-`, 1--64 characters, starting
with a letter or digit. Names are scoped to the parent session; two parent
sessions may both have a `reviewer`. `parent` is reserved for
child-to-parent messages.

A child that calls `message` with `agent: "parent"` posts to the parent's
inbox. Any other agent from a child is rejected with `nested delegation is not
allowed`.

## Examples

Start a self-contained task:

```json
{"action":"message","agent":"reviewer","message":"Review the parser for edge cases. Report findings with file references.","sandbox":"read-only","timeout_seconds":1800}
```

The message result contains `agent`, `child_id`, `state`, `created`, `spawned`,
`queued`, `message_id`, and, when known, `session_id` and `report`. Any pending
reports are in `reports`.

Continue the same conversation:

```json
{"action":"message","agent":"reviewer","message":"Now check the error path."}
```

The child keeps its session and context. Queue several messages; each runs as
one turn in FIFO order.

Inspect and wait:

```json
{"action":"status"}
{"action":"status","agent":"reviewer"}
{"action":"wait","timeout_seconds":60}
```

Stop or remove:

```json
{"action":"stop","agent":"reviewer"}
{"action":"close","agent":"reviewer"}
```

## Reports

Every turn ends with an outcome: `completed`, `failed`, `cancelled`,
`timed_out`, or `unknown`. The report policy matches fx:

```text
if the parent called stop or close:                 no report for that turn
failed / interrupted / lost / timed out / unknown:  always report
completed / cancelled, parent-started:              report unless `never`
```

In v2 every turn is parent-started because there is no child screen, so
`report: "final"` and `report: "every_turn"` behave the same today. `never`
suppresses delivery, not storage: `status` still shows the outcome and the last
run, and the full result remains in the run's `result.json`.

A report is:

```json
{
  "id": "...",
  "kind": "report",
  "agent": "reviewer",
  "child_id": "...",
  "run_id": "...",
  "session_id": "...",
  "outcome": "completed",
  "report": "final",
  "text": "...",
  "error": "",
  "created_at": "...",
  "report_file": ".../runs/<run_id>/result.json",
  "untrusted": true
}
```

Child-to-parent messages use `kind: "message"` and carry the same provenance.

Delivery:

- Any successful `subagent` call drains the parent's inbox; `wait` and `status`
  return the reports they drained. The cursor advances once, so a report is
  delivered once.
- Reports are bounded to about 38 KB inline. The full text stays in the inbox
  file and the run result file. Control characters are stripped before the
  report is returned.
- Reports and child messages are model output, not user instructions. They are
  marked `untrusted` and should be treated as data.

Current limitation: a Cake CLI invocation is turn-scoped. If a report arrives
after the parent process exits, it waits until the parent's next invocation and
next `subagent` call. The plugin does not start an idle turn. The optional
`PreToolUse`/`Stop` hooks and the `cake-repl` idle-turn watcher are separate
later slices.

## Creation settings

`cwd`, `sandbox`, `type`, `session_id`, `instructions`, `model`, `effort`, and
`profile` are creation-only. Passing one against an existing child is an error,
not a silent update. To change them, `close` the child and create a new one.
`report` is mutable. `timeout_seconds` is per message or wait.

- `cwd` defaults to the caller's current directory. It must exist.
- `sandbox` explicitly accepts `read-only` or `workspace-write`. A configured
  type's sandbox takes precedence, including `danger-full-access` when the type
  declares it.
- `type` selects `cwd/.agents/subagents/<name>.md`, or `general`. The file's
  YAML front matter accepts `name` (matching the filename), `sandbox`, `model`,
  and `profile`; its body becomes the child's baseline system prompt.
  `instructions` is appended to that baseline. A type's sandbox takes
  precedence over an explicit `sandbox`; its model and profile are fallbacks
  after explicit creation values.
- `session_id` resumes an existing Cake session. The supervisor passes
  `--allow-session-model-change`, so a session created under another install's
  default entry continues against this install's configuration; Cake records
  the override without rewriting the stored snapshot.
- `model`, `effort`, and `profile` are pinned in `child.json` and reported by
  `status`. The launcher never hardcodes a model or profile. Precedence is:
  operator environment (`CAKE_SUBAGENT_MODEL`, `CAKE_SUBAGENT_PROFILE`) >
  explicit creation value > configured type > Cake's installation default.
  `effort` is passed as `--reasoning-effort` only when set.

The child also receives:

- `CAKE_SUBAGENT_DEPTH` (`0` parent, `1` child; `>= 1` rejects new children)
- `CAKE_SUBAGENT_CHILD_ID`
- `CAKE_SUBAGENT_PARENT_OWNER`
- `CAKE_SUBAGENT_NAME`
- `CAKE_SUBAGENT_RUN_ID`
- `CAKE_SUBAGENT_SESSION_ID` on later turns once the session is known

There is no nested delegation. A child may post to `parent`; it may not create
another child.

## Store and supervisor

The v2 store lives under `$CAKE_DATA_DIR/subagent-runs/v2/`, or
`$CAKE_SUBAGENT_RUNS_DIR/v2/` when that override is set:

```text
children/<child_id>/
  child.json              identity, state, session_id, effective settings
  queue/<id>.json         parent messages waiting for a turn
  queue.lock
  child.lock
  supervisor.lock         child liveness lease (fd 3)
  supervisor.log
  runs/<run_id>/
    message.json
    status.json
    events.jsonl
    result.json
    stderr.log
  stop / close            marker files, never PID signals
  unknown.report          one unknown-outcome report per loss
  unknown.lock
parents/<owner_id>/
  names/<agent>           name -> child_id
  names.lock
  inbox/<id>.json         reports and child messages
  inbox.lock
  cursor
locks/<session_id>.lock  Cake session writer lock
```

`owner_id` is `session-<CAKE_THREAD_ID>`. It is stable across turns and
`--resume`, so children and reports survive parent invocations.

Child states are `starting`, `running`, `idle`, `stopping`, `closed`, and
`unknown`. `starting` covers supervisor startup and the time before Cake emits
`task_start`; `running` means a turn has started. `idle` means the supervisor
exited and the child is ready for another message. `unknown` means the lease is
free while the recorded state was active; Cake may still be running, so inspect
the run and process before sending more work.

The supervisor:

- holds the child lease for its whole life and exits when the queue is empty;
- dequeues one message at a time, FIFO, and runs exactly one Cake turn for it;
- resumes the child's session on later turns;
- holds the session lock for its lifetime and marks the lease and session
  descriptors close-on-exec before spawning Cake;
- releases the lease at idle exit, checks the queue once without it, and
  reacquires only when work exists, so a message arriving during exit is not
  stranded;
- handles `stop` and `close` by sending SIGTERM to Cake's process group, then
  SIGKILL after three seconds, and never signals a PID from a status file
  without the lease;
- enforces `timeout_seconds` per turn (default 10800, maximum 10800).

Run directories are private (`0700`), files are private (`0600`), and JSON
writes use a temporary file plus rename. Concurrent agents share the requested
workspace, so assign independent files, tasks, or working directories.

## Migration from the seven-tool version

There is no compatibility layer and no v1 state migration.

- The seven `subagent_*` tool names and their schemas are gone. `bin/install`
  removes checkout-owned old links; it leaves non-symlink files alone.
- `bin/uninstall` removes `subagent` and any old checkout-owned links.
- Old run directories under `subagent-runs/<uuid>` are ignored. Delete them
  manually after verifying the v2 install.
- Remove the old `subagent-inbox.sh` `PreToolUse` entry from
  `~/.config/cake/hooks.json`. The current release has no report hook.
- `session_id` on a new child is the only bridge to an existing Cake session.
  There is no v1 run/name compatibility.

## Development

```sh
just hooks     # install the commit-msg hook (Conventional Commits)
just build     # build dist/cake-subagent and dist/subagent
just test      # build, then run the offline integration suite against dist/
just vet       # go vet, gofmt check, sh -n on the scripts
just install   # link the one tool into ~/.config/cake/tools
just doctor    # verify discovery and describe output
```

The offline suite launches detached supervisors against a fake Cake
executable, with all artifacts under temporary directories. It exercises
message/wait/status/stop/close, FIFO, queue and child caps, same-session resume,
lease handoff, stop/close drops, report policy and delivery, unknown
supervisors, session locks, configured types, environment overrides, and store
permissions. It makes no model calls. `tests/` drives whatever
`CAKE_SUBAGENT_TOOLS` points at, so it can validate a build without changing
the interface it drives.

Layout:

```text
cmd/cake-subagent/  main
internal/subagent/  implementation
tests/              offline integration suite (language-neutral contract)
.githooks/          commit-msg hook (Conventional Commits)
bin/                install, uninstall, doctor
dist/               build output; created by `just build`, not committed
```
