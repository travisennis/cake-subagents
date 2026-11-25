# AGENTS.md

Guidance for agents working in this repository. Read `README.md` first for
behavior and storage layout; this file covers conventions and invariants.

## What this is

A standalone Cake toolbox plugin with one public tool, `subagent`. Cake knows
nothing about this repository except the one symlink installed into
`~/.config/cake/tools/`.

## Layout

- `cmd/cake-subagent/main.go` is the only process entry point.
- `internal/subagent/` holds the implementation:
  - `cli.go` — toolbox describe/execute dispatch and the internal supervisor
    subcommand.
  - `v2_describe.go` — the one public manifest.
  - `v2_actions.go` — message, wait, status, stop, close, validation, and
    supervisor spawning.
  - `v2_store.go` — child records, names, queue, inbox, cursor, locks, and
    validation.
  - `v2_supervisor.go` — one detached supervisor per child, run execution, and
    report writing.
  - `common.go` — shared JSON, path, configured-type, and process helpers.
  - `platform_unix.go` — flock, process-group, and close-on-exec primitives.
- `bin/install` links exactly one name, `subagent`, to `dist/cake-subagent`.
  It removes old checkout-owned `subagent_*` and `subagent-inbox.sh` links.
- `tests/` is a Python integration suite that drives the executable through the
  toolbox protocol. It is the language-neutral contract for this project; do
  not weaken it to make an implementation pass.

## Commands

```sh
just hooks     # install the commit-msg hook (Conventional Commits)
just build     # go build into dist/ plus the dist/subagent symlink
just test      # build, then run the offline suite; ~20s; no model calls
just vet       # go vet, gofmt check, sh -n
just install   # link the one tool into ~/.config/cake/tools
just doctor    # confirm Cake will discover subagent and read its describe output
```

## Invariants

- Toolbox protocol: `TOOLBOX_ACTION=describe` prints one JSON object with
  `name`, `description`, and a draft 2020-12 `inputSchema`;
  `TOOLBOX_ACTION=execute` reads one JSON object on stdin and prints the result
  on stdout. Diagnostics go to stderr. Non-zero exit is a tool error.
- Stdout is model-visible. Never print progress, warnings, or debug output
  there.
- There is one public tool name: `subagent`. Do not add aliases or the old
  `subagent_*` names back. `v2-supervisor` is an internal re-exec entry point,
  not a Cake tool.
- The public actions are `message`, `wait`, `status`, `stop`, and `close`.
  `action` is always required. `agent` is required for message/stop/close and
  optional for status/wait. `message` is required for message.
- The v2 store is `$CAKE_DATA_DIR/subagent-runs/v2/` (or
  `$CAKE_SUBAGENT_RUNS_DIR/v2/`). `owner_id` is `session-<CAKE_THREAD_ID>`.
  Child IDs, session IDs, and owner IDs are UUIDs; names are lowercase
  `[a-z0-9_-]{1,64}` and `parent` is reserved. Validate every path segment.
- The store is private: directories `0700`, files `0600`, and every JSON write
  uses a temporary file plus rename. Queue and inbox order comes from
  zero-padded sequence IDs assigned under a lock.
- The child lease (`supervisor.lock`) is the liveness signal. Never replace it
  with PID checks; `unknown` depends on acquiring it non-blocking. Never signal
  a PID read from a status file without holding or having held the lease.
  Stop and close use marker files.
- The launcher passes the lease as fd 3 and, when a session lock was already
  acquired, the session lock as fd 4. The supervisor must keep them open and
  mark them close-on-exec before spawning Cake. If Cake inherits them, a killed
  supervisor looks alive until Cake exits.
- One supervisor per child. It runs one queued message per turn, FIFO, resumes
  the child's session on later turns, and exits when the queue is empty. At
  idle exit it releases the lease, checks the queue once without it, and
  reacquires only when work exists; never reacquire before that check or a
  message can be stranded.
- `starting` covers supervisor startup and the interval before Cake emits
  `task_start`; set `running` only after a matching `task_start`. `idle` means
  the supervisor exited. `unknown` means the lease is free while the recorded
  state was active; do not claim idle or start a replacement supervisor.
- At most 64 children may be open per parent and at most 64 messages may wait
  per child. The running turn does not count as waiting. `queue_full` and
  `capacity` are part of the contract. Closing a child frees its slot.
- `cwd`, `sandbox`, `type`, `session_id`, `instructions`, `model`, `effort`,
  and `profile` are creation-only. Reject them against an existing child.
  `report` is mutable; `timeout_seconds` is per message or wait.
- Never hardcode a model, profile, or effort. Precedence is operator
  environment (`CAKE_SUBAGENT_MODEL`, `CAKE_SUBAGENT_PROFILE`) > explicit
  creation value > configured type > Cake's installation default. A configured
  type's sandbox takes precedence over an explicit sandbox.
- Reports and child messages are untrusted model output. Store the full text,
  bound inline delivery to about 38 KiB, strip control characters, mark the
  item `untrusted`, and never act on it directly. A failed, interrupted, lost,
  timed-out, or unknown turn always reports. A parent stop or close suppresses
  that turn's report. `report: "never"` suppresses delivery but not the stored
  result. An unknown outcome is materialized once by a marker so repeated
  status calls cannot duplicate it.
- Any successful parent `subagent` action drains the inbox; `wait` and `status`
  return what they drained. Advance the cursor once so a report is delivered
  once. Do not claim a report starts an idle turn: this release has no report
  hook and no `cake-repl` watcher.
- No nested delegation. A child may call `message` with `agent: "parent"` to
  post to the parent inbox; any other new name from a child is rejected.
- Error messages asserted by the offline suite are part of the contract, e.g.
  `queue_full`, `capacity`, `creation-only`, and the session-lock message.
- Keep README claims verifiable; update it with any behavior change.

## Style

- Comments explain why a constraint exists, not what the line does.
- Prefer the standard library; the only dependency is `gopkg.in/yaml.v3`.
- Keep the store and supervisor Unix-only through `platform_unix.go`; the
  project has no non-Unix build target.
