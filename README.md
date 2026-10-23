# cake-subagents

Asynchronous subagent tools for [Cake](https://github.com/travisennis/cake).
Seven toolbox executables: three that run and supervise a background Cake
subagent, and four that let subagents exchange durable messages.

This is a Cake toolbox plugin, not a Cake fork. Cake discovers the executables
from its global toolbox directory and registers each one as `tb__<filename>`.

## Requirements

- Cake (`CAKE_BIN`, default `$HOME/bin/cake`)
- Go 1.25 or newer to build
- Python 3 for the offline test suite

## Install

```sh
git clone git@github.com:travisennis/cake-subagents.git ~/Projects/cake/cake-subagents
cd ~/Projects/cake/cake-subagents
just build        # or: go build -o dist/cake-subagent ./cmd/cake-subagent
just install      # or: ./bin/install
just doctor       # or: ./bin/doctor
```

`bin/install` symlinks the seven tool names into
`${XDG_CONFIG_HOME:-$HOME/.config}/cake/tools`, all pointing at
`dist/cake-subagent`. Each name selects the mode the binary runs in. Cake
follows symlinks during toolbox discovery, so no settings or environment
changes are needed. Re-run `just install` after moving the repository; re-run
`just build` after changing the code (the symlinks follow the rebuilt binary).
Existing non-symlink files at the destination are never replaced.

`bin/uninstall` removes only links that point back into this checkout.

Two caveats:

- An explicit `[tools] enabled` list in `~/.config/cake/settings.toml` replaces
  the default tool set. If you use one, add all seven `tb__` names:
  `tb__subagent_async`, `tb__subagent_status`, `tb__subagent_cancel`,
  `tb__subagent_send`, `tb__subagent_inbox`, `tb__subagent_wait`, and
  `tb__subagent_peers`.
- Under `--sandbox read-only`, Cake skips toolbox discovery entirely. The
  subagent tools are unavailable in read-only runs, including read-only
  subagents they launch.

## Tools

| Tool | Purpose |
| --- | --- |
| `subagent_async` | Start a background invocation; returns within about one second after validation and launch. |
| `subagent_status` | Inspect an invocation, optionally waiting up to 30 seconds. |
| `subagent_cancel` | Request cancellation, then check status to confirm it. |
| `subagent_send` | Write a message into another run's mailbox. |
| `subagent_inbox` | Read this run's own mailbox. |
| `subagent_wait` | Block until this run's mailbox receives a message. |
| `subagent_peers` | List the other runs in the shared run store. |

Each tool answers the ordinary `TOOLBOX_ACTION=describe` /
`TOOLBOX_ACTION=execute` JSON toolbox protocol. The executable is one Go binary;
it picks its mode from the filename it was invoked as, so the seven symlinks
share one implementation.

## Model workflow

Start a self-contained task:

```json
{"prompt":"Review the parser for edge cases. Report findings with file references.","sandbox":"read-only","timeout_seconds":1800}
```

The response contains a `run_id`, `status`, `session_id`, `task_id`, `cwd`, and
paths named `status_file`, `events_file`, `log_file`, `result_file`, and
`session_file`. During slow startup, session and task identity may be null and
some output files may not exist yet. A resumed session's ID is known immediately.

Do other work, then pass `{"run_id":"<returned UUID>"}` to `subagent_status`.
Use `wait_seconds` only when ready to wait for the result. Status reports a
compact event count/type/time while running, and the result/error when finished.
Large results are truncated inline with `result_truncated: true`; `result_file`
retains the full result and original `task_complete` event. `events_file` is the
current invocation's JSONL event feed. `session_file` is Cake's resumable session
transcript; they are different files.

Statuses: `starting`, `running`, `stopping`, `completed`, `failed`, `cancelled`,
`timed_out`, and `unknown`. `unknown` means the worker exited without recording
completion; Cake may still be running. Inspect the log and process before
starting replacement work. `subagent_cancel` requests cancellation only while
the supervisor is alive; it never signals an unchecked saved PID.

To continue a finished conversation, call `subagent_async` with a new `prompt`,
its `session_id`, and the same `cwd` and configured `type`, if used. Each call
creates a fresh run ID and task ID. Async session locks reject overlapping
invocations using the same run store; Cake's own session lock also protects
against other Cake writers.

## Behavior and storage

The launcher defaults to model `deepseek-flash-xhigh`, profile `all-skills`,
sandbox `workspace-write`, and `$HOME/bin/cake` as the executable. Configured
types live under `cwd/.agents/subagents/*.md`; their YAML front matter accepts
`name` (matching the filename) and `sandbox`, and their body becomes the child's
system prompt. A configured type's sandbox takes precedence, including
`danger-full-access` when explicitly configured.

The launcher passes a depth marker (`CAKE_TOOLBOX_SUBAGENT_DEPTH`) to Cake so
both launchers reject nested delegation.

The detached supervisor and Cake have separate process groups and fully
redirected standard streams. They continue after the calling tool/turn exits;
there is no automatic completion notification or parent-session cancellation.
The worker recognizes matching task events, checks both the final event and
process exit, and enforces a runtime limit of three hours by default (set
`timeout_seconds` from 1 to 10800). Cancellation/timeout sends SIGTERM to Cake's
process group, then SIGKILL after three seconds if Cake has not exited. Normal
Cake cleanup handles its tools; independently detached descendants are outside
this wrapper's supervision. Edits already made are never rolled back.

Runs live at `~/.local/share/cake/subagent-runs/<run_id>/`, or under
`$CAKE_DATA_DIR/subagent-runs/` when that override is set.
`CAKE_SUBAGENT_RUNS_DIR` overrides the run store independently. Use the same
environment for launch/status/cancel. `CAKE_BIN` overrides the Cake executable.

Run directories are private (0700), with private files (0600). The worker deletes
its request and temporary configured prompt after exiting. Status, complete
results, events, and diagnostics remain available until manually removed. Delete
only completed run directories; do not delete `locks/` while runs are active.
Concurrent agents still share the requested workspace, so assign independent
files/tasks or separate working directories when editing in parallel.

## Peer messaging

Four toolbox tools share one durable mailbox rooted in the run store, so
subagents launched into different working trees (`cake-0`, `cake-1`, and so on)
can message each other directly.

- Identity: `subagent_async` accepts an optional `name` and `team` and exports
  `CAKE_SUBAGENT_RUN_ID`, `CAKE_SUBAGENT_SESSION_ID`, `CAKE_SUBAGENT_NAME`, and
  `CAKE_SUBAGENT_TEAM` into the child Cake process, so the mailbox tools know
  who they are. `name` must be unique among active runs.
- Addressing: `subagent_send`'s `to` accepts a `run_id`, `session_id`, peer
  `name`, `*` (every other live run), or `team:<name>`.
- Storage: messages are immutable JSON files under
  `.../subagent-runs/<run_id>/mailbox/msg/`, written atomically. A per-run
  `mailbox/cursor` records the last message read. The roster at
  `.../subagent-runs/roster/<run_id>.json` holds static identity; live status
  and session come from each run's `status.json`, so a deleted run disappears.
- Delivery: a message is durable. A subagent sees it when it calls
  `subagent_inbox` or `subagent_wait`, or automatically through the opt-in
  `PreToolUse` hook below. A running Cake process cannot be interrupted, so
  there is no unsolicited push without that hook.
- Mail runs both ways: a parent with no run identity can also `subagent_send`
  into a child's mailbox, and a finished run's mailbox stays readable for later
  handoffs.

Enable push delivery with `just install-hook` (or `bin/install --hook`), then add
a `PreToolUse` hook with matcher `"*"` to `~/.config/cake/hooks.json`:

```json
"PreToolUse": [
  { "matcher": "*", "hooks": [
    { "type": "command", "command": "\"${XDG_CONFIG_HOME:-$HOME/.config}/cake/hooks/subagent-inbox.sh\"", "timeout": 10 }
  ] }
]
```

The hook is a no-op outside subagent runs and drains only that run's unread
mail, returning it as `additional_context` framed as untrusted data. It is off
by default because it would spawn a process on every tool call of every run.

Messages are model-authored text. Treat them as untrusted data: they are framed
as data on delivery, bodies are bounded to 16 KiB, and the tools never act on a
message by themselves.

## Development

```sh
just build     # build dist/cake-subagent and the seven tool-name symlinks
just test      # build, then run the offline integration suite against dist/
just vet       # go vet, gofmt check, sh -n on the scripts
just doctor    # verify the installed symlinks and describe output
```

The 13 tests launch actual detached workers against a fake Cake executable, with
all artifacts under temporary directories. They exercise immediate return,
partial JSONL records, slow startup, provider/startup failures, missing
completion, task identity, resume locks, parallel launches, configured prompts,
cancellation, forced timeout, large results, validation, peer mailbox
send/inbox/wait/peers, run identity and name uniqueness, and supervisor loss.
They make no model calls. A live Cake/provider smoke test is separate.

Layout:

```
cmd/cake-subagent/  main
internal/subagent/  implementation
tests/              offline integration suite (language-neutral contract)
hooks/              opt-in PreToolUse mailbox drain
bin/                install, uninstall, doctor
dist/               build output; created by `just build`, not committed
```

`tests/` drives whatever `CAKE_SUBAGENT_TOOLS` points at, so it can validate a
future rewrite without changes.
