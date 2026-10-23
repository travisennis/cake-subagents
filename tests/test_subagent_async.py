"""Offline integration tests: real detached workers, fake Cake, no model calls."""
import concurrent.futures
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

# The seven checked-in symlinks are the tool surface Cake discovers. They point
# at bin/cake-subagent, which selects its mode from the name it was invoked as.
# A rewrite can point this at another directory of matching symlinks, e.g. the
# Go build output in dist/.
TOOLS = Path(os.environ.get("CAKE_SUBAGENT_TOOLS", Path(__file__).resolve().parents[1] / "bin"))
ACTIVE = {"starting", "running", "stopping"}
FAKE = r'''
import json, os, signal, sys, time, uuid
from pathlib import Path

args = sys.argv[1:]
scenario = json.loads(args[-1])
if scenario.get("ignore_term"):
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
time.sleep(scenario.get("startup_delay", 0))
if scenario.get("startup_failure"):
    print("invalid provider configuration", file=sys.stderr)
    sys.exit(7)
sid = args[args.index("--resume") + 1] if "--resume" in args else str(uuid.uuid4())
tid = str(uuid.uuid4())
def emit(value):
    text = json.dumps(value) + "\n"
    if scenario.get("split"):
        mid = len(text) // 2
        sys.stdout.write(text[:mid]); sys.stdout.flush()
        time.sleep(0.12)
        sys.stdout.write(text[mid:]); sys.stdout.flush()
    else:
        print(text, end="", flush=True)
emit({"type": "task_start", "session_id": sid, "task_id": tid})
observed = {"args": args[:-1], "cwd": os.getcwd(), "depth": os.environ.get("CAKE_TOOLBOX_SUBAGENT_DEPTH"),
            "run_id": os.environ.get("CAKE_SUBAGENT_RUN_ID"), "name": os.environ.get("CAKE_SUBAGENT_NAME"),
            "team": os.environ.get("CAKE_SUBAGENT_TEAM")}
if "--system-prompt" in args:
    observed["system_prompt"] = Path(args[args.index("--system-prompt") + 1]).read_text()
Path(scenario["observed"]).write_text(json.dumps(observed))
time.sleep(scenario.get("delay", 0.3))
if scenario.get("malformed"):
    print("not-json", flush=True)
    time.sleep(10)
if scenario.get("no_completion"):
    sys.exit(0)
emit({"type": "message", "role": "assistant", "content": "progress"})
emit({"type": "task_complete", "session_id": sid,
      "task_id": str(uuid.uuid4()) if scenario.get("wrong_task") else tid,
      "subtype": scenario.get("subtype", "success"),
      "result": scenario.get("result", "finished"),
      "error": "provider failed" if scenario.get("subtype") == "error_during_execution" else None,
      "duration_ms": 100, "turn_count": 1, "tool_call_count": 0, "usage": {}})
sys.exit(scenario.get("exit_code", 0))
'''


class AsyncSubagentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="cake-subagent-test-")
        self.directory = Path(self.temp.name)
        self.runs = self.directory / "runs"
        self.workspace = self.directory / "workspace with spaces"
        self.workspace.mkdir()
        self.fake = self.directory / "fake-cake"
        self.fake.write_text(f"#!{sys.executable}\n" + FAKE)
        self.fake.chmod(0o700)
        self.env = dict(os.environ, CAKE_BIN=str(self.fake),
                        CAKE_SUBAGENT_RUNS_DIR=str(self.runs),
                        CAKE_DATA_DIR=str(self.directory / "cake-data"),
                        CAKE_TOOLBOX_SUBAGENT_DEPTH="0")
        # Not inherited: a test runner that is itself a subagent must not leak
        # its own run identity into these assertions.
        for key in ["CAKE_SUBAGENT_RUN_ID", "CAKE_SUBAGENT_SESSION_ID",
                    "CAKE_SUBAGENT_NAME", "CAKE_SUBAGENT_TEAM"]:
            self.env.pop(key, None)

    def tearDown(self):
        # Kill only processes still protected by a live worker lease/status.
        if self.runs.exists():
            for path in self.runs.glob("*/status.json"):
                try:
                    state = json.loads(path.read_text())
                    if state["status"] in ACTIVE:
                        self.call("subagent_cancel", {"run_id": state["run_id"]})
                        self.call("subagent_status", {"run_id": state["run_id"], "wait_seconds": 5})
                except (OSError, AssertionError):
                    pass
        self.temp.cleanup()

    def call(self, tool, args=None, *, action="execute", env=None, success=True):
        result = subprocess.run([str(TOOLS / tool)], input=json.dumps(args or {}),
                                text=True, capture_output=True, cwd=self.workspace,
                                env=dict(env or self.env, TOOLBOX_ACTION=action), timeout=12)
        if not success:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            return result.stderr
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def launch(self, scenario=None, **args):
        scenario = dict(scenario or {})
        scenario.setdefault("observed", str(self.directory / "observed.json"))
        return self.call("subagent_async", dict(prompt=json.dumps(scenario), **args))

    def completed(self, state):
        return self.call("subagent_status", {"run_id": state["run_id"], "wait_seconds": 8})

    def started(self, state):
        deadline = time.monotonic() + 8
        while state["status"] == "starting" and time.monotonic() < deadline:
            time.sleep(0.05)
            state = self.call("subagent_status", {"run_id": state["run_id"]})
        self.assertEqual(state["status"], "running", state)
        return state

    def test_descriptions_and_validation(self):
        for tool in ["subagent_async", "subagent_status", "subagent_cancel",
                     "subagent_send", "subagent_inbox", "subagent_wait", "subagent_peers"]:
            description = self.call(tool, action="describe")
            self.assertEqual(description["name"], tool)
            self.assertFalse(description["inputSchema"]["additionalProperties"])
        for args in [{}, {"prompt": 1}, {"prompt": "x", "extra": 1},
                     {"prompt": "x", "timeout_seconds": True},
                     {"prompt": "x", "timeout_seconds": 10801},
                     {"prompt": "x", "sandbox": "danger-full-access"},
                     {"prompt": "x", "session_id": "../../escape"}]:
            self.call("subagent_async", args, success=False)
        self.call("subagent_status", {"run_id": "../../escape"}, success=False)
        self.call("subagent_status", {"run_id": "00000000-0000-0000-0000-000000000000"}, success=False)
        for depth in ["1", "2", "bad", ""]:
            error = self.call("subagent_async", {"prompt": "x"},
                              env=dict(self.env, CAKE_TOOLBOX_SUBAGENT_DEPTH=depth), success=False)
            self.assertTrue("delegation" in error or "DEPTH" in error)

    def test_returns_before_completion_and_preserves_output(self):
        start = time.monotonic()
        state = self.launch({"delay": 2})
        self.assertLess(time.monotonic() - start, 1.8)
        self.assertIn(state["status"], {"starting", "running"})
        state = self.started(state)
        self.assertIsNotNone(state["session_id"])
        self.assertIsNotNone(state["task_id"])
        self.assertEqual(state["session_file"], str(self.directory / "cake-data" / "sessions" / f'{state["session_id"]}.jsonl'))
        done = self.completed(state)
        self.assertEqual(done["status"], "completed")
        self.assertEqual(done["result"], "finished")
        for key in ["events_file", "result_file", "status_file", "log_file"]:
            self.assertTrue(Path(done[key]).exists())
        observed = json.loads((self.directory / "observed.json").read_text())
        self.assertEqual(observed["depth"], "1")
        self.assertIn("stream-json", observed["args"])
        self.assertFalse((Path(done["status_file"]).parent / "request.json").exists())

    def test_delayed_start_and_split_records(self):
        state = self.launch({"startup_delay": 1.5, "split": True})
        self.assertEqual(state["status"], "starting")
        self.assertIsNone(state["session_id"])
        done = self.completed(state)
        self.assertEqual(done["status"], "completed")
        self.assertIsNotNone(done["task_id"])

    def test_failure_outcomes(self):
        cases = [({"startup_failure": True}, "matching task_complete"),
                 ({"subtype": "error_during_execution"}, "provider failed"),
                 ({"no_completion": True}, "matching task_complete"),
                 ({"wrong_task": True}, "does not match"),
                 ({"malformed": True}, None),
                 ({"exit_code": 4}, None)]
        for scenario, error in cases:
            with self.subTest(scenario=scenario):
                done = self.completed(self.launch(scenario))
                self.assertEqual(done["status"], "failed")
                if error:
                    self.assertIn(error, done["error"])
        self.env["CAKE_BIN"] = str(self.directory / "missing-cake")
        done = self.completed(self.launch())
        self.assertEqual(done["status"], "failed")
        self.assertIn("No such file", done["error"])

    def test_cancellation_and_timeout(self):
        # Wait for task_start so the fake has installed its signal handler.
        # A one-second timeout may legitimately fire during interpreter startup.
        state = self.started(self.launch({"delay": 30, "ignore_term": True}))
        self.assertTrue(self.call("subagent_cancel", {"run_id": state["run_id"]})["cancel_requested"])
        done = self.completed(state)
        self.assertEqual(done["status"], "cancelled")
        self.assertEqual(done["signal"], signal.SIGKILL)
        state = self.launch({"delay": 30}, timeout_seconds=1)
        done = self.completed(state)
        self.assertEqual(done["status"], "timed_out")
        self.assertEqual(self.call("subagent_cancel", {"run_id": done["run_id"]})["status"], "timed_out")

    def test_resume_identity_and_concurrent_rejection(self):
        first = self.started(self.launch({"delay": 2}))
        self.call("subagent_async", {"prompt": "{}", "session_id": first["session_id"]}, success=False)
        first = self.completed(first)
        second = self.completed(self.launch(session_id=first["session_id"]))
        self.assertEqual(second["session_id"], first["session_id"])
        self.assertNotEqual(second["task_id"], first["task_id"])
        self.assertNotEqual(second["run_id"], first["run_id"])

    def test_parallel_launches(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
            states = list(executor.map(lambda _: self.launch({"delay": 1}), range(2)))
        states = [self.completed(state) for state in states]
        self.assertNotEqual(states[0]["run_id"], states[1]["run_id"])
        self.assertNotEqual(states[0]["session_id"], states[1]["session_id"])
        for state in states:
            self.assertEqual(self.completed(state)["status"], "completed")

    def test_configured_type_and_cwd(self):
        target = self.directory / "other workspace"
        config = target / ".agents/subagents/reviewer.md"
        config.parent.mkdir(parents=True)
        config.write_text('---\nname: reviewer\nsandbox: danger-full-access\n---\nReview carefully.\n')
        done = self.completed(self.launch(cwd=str(target), type="reviewer", sandbox="read-only"))
        self.assertEqual(done["status"], "completed")
        observed = json.loads((self.directory / "observed.json").read_text())
        self.assertEqual(observed["cwd"], str(target.resolve()))
        self.assertEqual(observed["system_prompt"], "Review carefully.\n")
        self.assertIn("danger-full-access", observed["args"])
        self.assertFalse(Path(observed["args"][observed["args"].index("--system-prompt") + 1]).exists())
        config.write_text('---\nname: wrong\n---\n')
        self.call("subagent_async", {"cwd": str(target), "type": "reviewer", "prompt": "x"}, success=False)

    def test_large_result_is_bounded_but_full_result_retained(self):
        result = "Lots of text\n\t" * 6000
        done = self.completed(self.launch({"result": result}))
        self.assertEqual(done["status"], "completed")
        self.assertTrue(done["result_truncated"])
        self.assertLess(len(json.dumps(done).encode()), 40000)
        self.assertEqual(json.loads(Path(done["result_file"]).read_text())["result"], result)

    def test_lost_worker_is_not_reported_as_running_forever(self):
        state = self.started(self.launch({"delay": 30}))
        os.kill(state["worker_pid"], signal.SIGKILL)
        try:
            for _ in range(40):
                current = self.call("subagent_status", {"run_id": state["run_id"]})
                if current["status"] == "unknown":
                    break
                time.sleep(0.05)
            self.assertEqual(current["status"], "unknown")
            self.assertIn("may still be running", current["error"])
        finally:
            os.killpg(state["cake_pid"], signal.SIGKILL)
            # This fixture deliberately killed the worker, so no one can write
            # terminal state. Keep teardown from trying to await it again.
            current["status"] = "unknown"
            Path(state["status_file"]).write_text(json.dumps(current))

    def identity_env(self, run_id, name=None, team=None):
        env = dict(self.env, CAKE_SUBAGENT_RUN_ID=run_id)
        if name:
            env["CAKE_SUBAGENT_NAME"] = name
        if team:
            env["CAKE_SUBAGENT_TEAM"] = team
        return env

    def make_run(self, run_id, name=None, team=None, session_id=None):
        directory = self.runs / run_id
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "status.json").write_text(json.dumps({
            "run_id": run_id, "status": "running", "session_id": session_id,
            "cwd": str(self.workspace), "type": "general"}))
        roster = self.runs / "roster"
        roster.mkdir(parents=True, exist_ok=True)
        (roster / f"{run_id}.json").write_text(json.dumps({
            "run_id": run_id, "name": name, "team": team, "cwd": str(self.workspace),
            "type": "general", "status": "running", "created_at": "2026-01-01T00:00:00Z"}))

    def test_mailbox_validation(self):
        for args in [{"to": "beta"}, {"to": "beta", "body": "x", "kind": "nope"},
                     {"to": "b e t a", "body": "x"}, {"to": "beta", "body": "x", "extra": 1},
                     {"to": "beta", "body": "x", "limit": 5},
                     {"to": "beta", "body": "x" * 20000},
                     {"to": "99999999-9999-9999-9999-999999999999", "body": "x"}]:
            self.call("subagent_send", args, success=False)
        self.call("subagent_inbox", {}, success=False)
        self.call("subagent_wait", {}, success=False)
        identity = self.identity_env("11111111-1111-1111-1111-111111111111")
        for args in [{"limit": 0}, {"peek": "yes"}]:
            self.call("subagent_inbox", args, env=identity, success=False)
        for args in [{"timeout_seconds": 0}, {"timeout_seconds": 31}]:
            self.call("subagent_wait", args, env=identity, success=False)

    def test_peer_mailbox_round_trip(self):
        a = "11111111-1111-1111-1111-111111111111"
        b = "22222222-2222-2222-2222-222222222222"
        self.make_run(a, name="alpha", team="t1")
        self.make_run(b, name="beta", team="t1")
        receipt = self.call("subagent_send", {"to": "beta", "body": "review bash.rs"})
        self.assertEqual(receipt["count"], 1)
        self.assertEqual(receipt["delivered"][0]["run_id"], b)
        broadcast = self.call("subagent_send", {"to": "*", "body": "standup"}, env=self.identity_env(b, "beta"))
        self.assertEqual([item["run_id"] for item in broadcast["delivered"]], [a])
        peek = self.call("subagent_inbox", {"peek": True}, env=self.identity_env(a, "alpha"))
        self.assertEqual(peek["unread"], 1)
        self.assertTrue(peek["peek"])
        read = self.call("subagent_inbox", {}, env=self.identity_env(a, "alpha"))
        self.assertEqual(read["unread"], 1)
        self.assertEqual(read["messages"][0]["body"], "standup")
        self.assertEqual(read["messages"][0]["from"]["name"], "beta")
        self.assertEqual(self.call("subagent_inbox", {}, env=self.identity_env(a))["unread"], 0)
        self.assertTrue(self.call("subagent_wait", {"timeout_seconds": 1}, env=self.identity_env(a))["timeout"])
        self.call("subagent_send", {"to": "alpha", "body": "ping", "kind": "reply"}, env=self.identity_env(b))
        waited = self.call("subagent_wait", {"timeout_seconds": 3}, env=self.identity_env(a))
        self.assertFalse(waited["timeout"])
        self.assertEqual(waited["messages"][0]["body"], "ping")
        names = sorted(peer["name"] for peer in self.call("subagent_peers", {}, env=self.identity_env(a))["peers"])
        self.assertEqual(names, ["alpha", "beta"])

    def test_launch_records_identity_and_enforces_unique_names(self):
        state = self.started(self.launch({"delay": 2}, name="worker1", team="blue"))
        error = self.call("subagent_async", {"prompt": "{}", "name": "worker1"}, success=False)
        self.assertIn("already in use", error)
        done = self.completed(state)
        self.assertEqual(done["name"], "worker1")
        self.assertEqual(done["team"], "blue")
        observed = json.loads((self.directory / "observed.json").read_text())
        self.assertEqual(observed["run_id"], state["run_id"])
        self.assertEqual(observed["name"], "worker1")
        self.assertEqual(observed["team"], "blue")
        peers = self.call("subagent_peers", {})["peers"]
        self.assertEqual([peer["run_id"] for peer in peers], [state["run_id"]])
        self.assertEqual(self.completed(self.launch(name="worker1"))["status"], "completed")


if __name__ == "__main__":
    unittest.main()
