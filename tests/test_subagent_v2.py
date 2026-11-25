"""Offline tests for the v2 child-conversation store and supervisor.

These exercise the internal `v2` harness, not the public seven-tool surface.
That surface is intentionally unchanged until Slice 2.
"""
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time
import unittest
import uuid

TOOLS = Path(os.environ.get("CAKE_SUBAGENT_TOOLS", Path(__file__).resolve().parents[1] / "dist"))
BINARY = TOOLS / "cake-subagent"
PARENT_ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
OWNER = f"session-{PARENT_ID}"
ACTIVE = {"starting", "running", "stopping"}

FAKE = r'''
import json, os, signal, sys, time, uuid
from pathlib import Path

args = sys.argv[1:]
try:
    scenario = json.loads(args[-1]) if args else {}
except Exception:
    scenario = {}
log_path = os.environ.get("FAKE_CAKE_LOG")

def log(event):
    if not log_path:
        return
    line = json.dumps(event, sort_keys=True) + "\n"
    fd = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
    os.write(fd, line.encode())
    os.close(fd)

observed = {
    "args": args,
    "cwd": os.getcwd(),
    "prompt": args[-1] if args else None,
    "depth": os.environ.get("CAKE_SUBAGENT_DEPTH"),
    "legacy_depth": os.environ.get("CAKE_TOOLBOX_SUBAGENT_DEPTH"),
    "child_id": os.environ.get("CAKE_SUBAGENT_CHILD_ID"),
    "parent_owner": os.environ.get("CAKE_SUBAGENT_PARENT_OWNER"),
    "name": os.environ.get("CAKE_SUBAGENT_NAME"),
    "run_id": os.environ.get("CAKE_SUBAGENT_RUN_ID"),
    "session_id": os.environ.get("CAKE_SUBAGENT_SESSION_ID"),
}
if "--system-prompt" in args:
    observed["system_prompt"] = Path(args[args.index("--system-prompt") + 1]).read_text()
log({"event": "start", "pid": os.getpid(), "time": time.time(), **observed})

if scenario.get("ignore_term"):
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
time.sleep(scenario.get("startup_delay", 0))
if scenario.get("startup_failure"):
    print("invalid provider configuration", file=sys.stderr, flush=True)
    log({"event": "end", "pid": os.getpid(), "time": time.time(), "outcome": "startup_failure"})
    sys.exit(7)

sid = args[args.index("--resume") + 1] if "--resume" in args else str(uuid.uuid4())
tid = str(uuid.uuid4())

def emit(value):
    text = json.dumps(value) + "\n"
    if scenario.get("split"):
        middle = len(text) // 2
        sys.stdout.write(text[:middle])
        sys.stdout.flush()
        time.sleep(0.12)
        sys.stdout.write(text[middle:])
        sys.stdout.flush()
    else:
        sys.stdout.write(text)
        sys.stdout.flush()

emit({"type": "task_start", "session_id": sid, "task_id": tid,
      "model": "fake-provider-model", "model_config": "fake-entry"})
time.sleep(scenario.get("delay", 0.05))
if scenario.get("malformed"):
    print("not-json", flush=True)
    time.sleep(10)
    log({"event": "end", "pid": os.getpid(), "time": time.time(), "outcome": "malformed"})
    sys.exit(0)
if scenario.get("no_completion"):
    log({"event": "end", "pid": os.getpid(), "time": time.time(), "outcome": "no_completion"})
    sys.exit(0)
emit({"type": "message", "role": "assistant", "content": "progress"})
emit({"type": "task_complete", "session_id": sid,
      "task_id": str(uuid.uuid4()) if scenario.get("wrong_task") else tid,
      "subtype": scenario.get("subtype", "success"),
      "result": scenario.get("result", "finished"),
      "error": "provider failed" if scenario.get("subtype") == "error_during_execution" else None,
      "duration_ms": 100, "turn_count": 1, "tool_call_count": 0, "usage": {}})
log({"event": "end", "pid": os.getpid(), "time": time.time(),
     "sid": sid, "tid": tid, "outcome": scenario.get("subtype", "success")})
sys.exit(scenario.get("exit_code", 0))
'''


def timestamp():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


class V2SubagentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="cake-subagent-v2-")
        self.directory = Path(self.temp.name)
        self.runs = self.directory / "runs"
        self.workspace = self.directory / "workspace"
        self.workspace.mkdir()
        self.fake = self.directory / "fake-cake"
        self.fake.write_text(f"#!{sys.executable}\n" + FAKE)
        self.fake.chmod(0o700)
        self.log = self.directory / "fake-log.jsonl"
        self.owner_dir = self.runs / "v2" / "parents" / OWNER
        self.children_root = self.runs / "v2" / "children"
        self.env = dict(os.environ,
                        CAKE_BIN=str(self.fake),
                        CAKE_SUBAGENT_RUNS_DIR=str(self.runs),
                        CAKE_DATA_DIR=str(self.directory / "cake-data"),
                        CAKE_THREAD_ID=PARENT_ID,
                        CAKE_SUBAGENT_DEPTH="0",
                        CAKE_TOOLBOX_SUBAGENT_DEPTH="0",
                        FAKE_CAKE_LOG=str(self.log))
        for key in ["CAKE_SUBAGENT_PARENT_OWNER", "CAKE_SUBAGENT_CHILD_ID",
                    "CAKE_SUBAGENT_NAME", "CAKE_SUBAGENT_RUN_ID",
                    "CAKE_SUBAGENT_SESSION_ID", "CAKE_SUBAGENT_MODEL",
                    "CAKE_SUBAGENT_PROFILE"]:
            self.env.pop(key, None)

    def tearDown(self):
        try:
            if self.children_root.exists():
                for child_dir in self.children_root.iterdir():
                    path = child_dir / "child.json"
                    if not path.exists():
                        continue
                    try:
                        record = json.loads(path.read_text())
                    except (OSError, json.JSONDecodeError):
                        continue
                    if record.get("state") != "closed" and record.get("name"):
                        try:
                            self.call("close", agent=record["name"])
                        except Exception:
                            pass
                    pid = record.get("supervisor_pid")
                    if pid:
                        try:
                            os.kill(pid, signal.SIGKILL)
                        except OSError:
                            pass
                for status_path in self.children_root.glob("*/runs/*/status.json"):
                    try:
                        run = json.loads(status_path.read_text())
                        pid = run.get("cake_pid")
                        if pid:
                            os.killpg(pid, signal.SIGKILL)
                    except (OSError, json.JSONDecodeError):
                        pass
        finally:
            self.temp.cleanup()

    def call(self, action, *, env=None, expect=0, **args):
        payload = {"action": action, **args}
        result = subprocess.run([str(BINARY), "v2"], input=json.dumps(payload),
                                text=True, capture_output=True, cwd=self.workspace,
                                env=dict(env or self.env), timeout=20)
        if expect:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            return result.stderr
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def message(self, agent, scenario=None, env=None, **args):
        return self.call("message", agent=agent, message=json.dumps(scenario or {}),
                         env=env, **args)

    def status(self, agent=None):
        return self.call("status", **({"agent": agent} if agent else {}))

    def wait(self, agent=None, timeout_seconds=10):
        args = {"timeout_seconds": timeout_seconds}
        if agent:
            args["agent"] = agent
        return self.call("wait", **args)

    def child_record(self, agent):
        child_id = (self.owner_dir / "names" / agent).read_text().strip()
        return self.child_record_by_id(child_id)

    def child_record_by_id(self, child_id):
        path = self.children_root / child_id / "child.json"
        return json.loads(path.read_text())

    def wait_for_state(self, agent, states, timeout=8):
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            try:
                last = self.child_record(agent)
            except FileNotFoundError:
                break
            if last["state"] in states:
                return last
            time.sleep(0.05)
        self.fail(f"{agent} did not reach {states}: {last}")

    def wait_for_child_state(self, child_id, state, timeout=8):
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            try:
                last = self.child_record_by_id(child_id)
            except FileNotFoundError:
                break
            if last["state"] == state:
                return last
            time.sleep(0.05)
        self.fail(f"{child_id} did not reach {state}: {last}")

    def latest_run(self, child_id):
        directory = self.children_root / child_id / "runs"
        entries = sorted(path for path in directory.iterdir() if path.is_dir())
        if not entries:
            return None
        return json.loads((entries[-1] / "status.json").read_text())

    def fake_events(self):
        if not self.log.exists():
            return []
        events = []
        for line in self.log.read_text().splitlines():
            if not line.strip():
                continue
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                pass
        return events

    def starts(self):
        return [event for event in self.fake_events() if event.get("event") == "start"]

    def make_child_record(self, name, state="idle"):
        child_id = str(uuid.uuid4())
        child_dir = self.children_root / child_id
        (child_dir / "queue").mkdir(parents=True)
        (child_dir / "runs").mkdir()
        record = {
            "child_id": child_id, "name": name, "owner_id": OWNER,
            "type": "general", "cwd": str(self.workspace),
            "sandbox": "workspace-write", "report": "final",
            "state": state, "last_outcome": "never",
            "created_at": timestamp(), "last_active_at": timestamp(),
        }
        (child_dir / "child.json").write_text(json.dumps(record))
        names = self.owner_dir / "names"
        names.mkdir(parents=True, exist_ok=True)
        (names / name).write_text(child_id + "\n")
        return child_id

    def add_queue_entry(self, child_id, index, prompt):
        queue = self.children_root / child_id / "queue"
        queue.mkdir(parents=True, exist_ok=True)
        message_id = f"{index:020d}-deadbeef"
        path = queue / f"{message_id}.json"
        path.write_text(json.dumps({
            "id": message_id, "child_id": child_id, "prompt": prompt,
            "timeout_seconds": 10800, "created_at": timestamp(),
        }))
        path.chmod(0o600)

    def test_message_wait_status_and_environment(self):
        response = self.message("worker", {"delay": 0.05, "result": "first"})
        self.assertTrue(response["created"])
        self.assertIn(response["state"], {"starting", "running"})
        child_id = response["child_id"]

        result = self.wait("worker", timeout_seconds=5)
        self.assertFalse(result["timeout"])
        self.assertEqual(len(result["reports"]), 1)
        report = result["reports"][0]
        self.assertEqual(report["kind"], "report")
        self.assertEqual(report["agent"], "worker")
        self.assertEqual(report["text"], "first")
        self.assertEqual(report["outcome"], "completed")
        self.assertEqual(report["child_id"], child_id)
        self.assertTrue(report["untrusted"])
        self.assertTrue(report["session_id"])
        self.assertTrue(Path(report["report_file"]).exists())

        record = self.wait_for_state("worker", {"idle"})
        self.assertEqual(record["last_outcome"], "completed")
        self.assertEqual(record["session_id"], report["session_id"])

        status = self.status("worker")
        child = status["children"][0]
        self.assertEqual(child["state"], "idle")
        self.assertEqual(child["last_outcome"], "completed")
        self.assertEqual(child["queued"], 0)
        self.assertEqual(child["report"], "final")

        self.assertTrue(self.wait("worker", timeout_seconds=1)["timeout"])
        close = self.call("close", agent="worker")
        self.assertIn("closed", close)
        self.wait_for_child_state(child_id, "closed")

        starts = self.starts()
        self.assertEqual(len(starts), 1)
        start = starts[0]
        self.assertNotIn("--resume", start["args"])
        self.assertEqual(start["depth"], "1")
        self.assertEqual(start["legacy_depth"], "1")
        self.assertEqual(start["child_id"], child_id)
        self.assertEqual(start["parent_owner"], OWNER)
        self.assertEqual(start["name"], "worker")
        self.assertTrue(start["run_id"])

        child_dir = self.children_root / child_id
        self.assertEqual(stat.S_IMODE(child_dir.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE((child_dir / "child.json").stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(self.owner_dir.stat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE((self.owner_dir / "inbox").stat().st_mode), 0o700)
        inbox_files = list((self.owner_dir / "inbox").glob("*.json"))
        self.assertEqual(len(inbox_files), 1)
        self.assertEqual(stat.S_IMODE(inbox_files[0].stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE((self.owner_dir / "cursor").stat().st_mode), 0o600)

    def test_fifo_and_same_session_resume(self):
        self.message("worker", {"delay": 0.4, "result": "one"})
        self.wait_for_state("worker", {"running"})
        self.message("worker", {"delay": 0.05, "result": "two"})
        self.message("worker", {"delay": 0.05, "result": "three"})

        reports = []
        deadline = time.monotonic() + 10
        while len(reports) < 3 and time.monotonic() < deadline:
            result = self.wait("worker", timeout_seconds=2)
            reports.extend(result["reports"])
        self.assertEqual([report["text"] for report in reports], ["one", "two", "three"])
        session_id = reports[0]["session_id"]
        self.assertTrue(all(report["session_id"] == session_id for report in reports))

        starts = self.starts()
        self.assertEqual(len(starts), 3)
        self.assertNotIn("--resume", starts[0]["args"])
        for start in starts[1:]:
            self.assertIn("--resume", start["args"])
            self.assertEqual(start["args"][start["args"].index("--resume") + 1], session_id)
            self.assertEqual(start["session_id"], session_id)
        self.assertEqual([json.loads(start["prompt"])["result"] for start in starts],
                         ["one", "two", "three"])
        self.wait_for_state("worker", {"idle"})
        self.assertEqual(self.child_record("worker")["last_outcome"], "completed")

    def test_queue_cap_and_idle_stop_drops(self):
        child_id = self.make_child_record("capped")
        for index in range(1, 65):
            self.add_queue_entry(child_id, index, json.dumps({"delay": 0.05}))
        error = self.message("capped", {"delay": 0.05}, expect=2)
        self.assertIn("queue_full", error)
        stop = self.call("stop", agent="capped")
        self.assertEqual(stop["dropped"], 64)
        self.assertEqual(stop["state"], "idle")
        self.assertEqual(self.child_record("capped")["last_outcome"], "cancelled")
        self.assertEqual(len(list((self.children_root / child_id / "queue").glob("*.json"))), 0)
        self.assertEqual(self.wait("capped", timeout_seconds=1)["reports"], [])

    def test_capacity_and_close_frees_a_slot(self):
        for index in range(64):
            self.make_child_record(f"agent{index:02d}")
        error = self.message("new", {"delay": 0.05}, expect=2)
        self.assertIn("capacity", error)
        self.assertTrue(self.call("close", agent="agent00")["closed"])
        response = self.message("new", {"delay": 0.05, "result": "ok"})
        self.assertTrue(response["created"])
        report = self.wait("new", timeout_seconds=5)
        self.assertEqual(report["reports"][0]["text"], "ok")

    def test_failure_reports_even_when_never(self):
        self.message("broken", {"startup_failure": True}, report="never")
        result = self.wait("broken", timeout_seconds=8)
        self.assertEqual(len(result["reports"]), 1)
        report = result["reports"][0]
        self.assertEqual(report["outcome"], "failed")
        self.assertEqual(report["report"], "never")
        self.assertIn("invalid provider configuration", report["error"])
        self.assertEqual(self.wait_for_state("broken", {"idle"})["last_outcome"], "failed")

    def test_never_suppresses_completed_report_but_keeps_result(self):
        self.message("quiet", {"delay": 0.05, "result": "done"}, report="never")
        record = self.wait_for_state("quiet", {"idle"})
        self.assertEqual(record["last_outcome"], "completed")
        result = self.wait("quiet", timeout_seconds=1)
        self.assertTrue(result["timeout"])
        self.assertEqual(result["reports"], [])
        run = self.latest_run(record["child_id"])
        self.assertEqual(run["status"], "completed")
        self.assertEqual(run["result"], "done")

    def test_stop_drops_queue_and_suppresses_report(self):
        self.message("worker", {"delay": 30, "ignore_term": True})
        self.wait_for_state("worker", {"running"})
        record = self.child_record("worker")
        session_id = record["session_id"]
        for index in range(3):
            self.message("worker", {"delay": 0.05, "result": str(index)})
        stop = self.call("stop", agent="worker")
        self.assertEqual(stop["dropped"], 3)
        record = self.wait_for_state("worker", {"idle"}, timeout=8)
        self.assertEqual(record["last_outcome"], "cancelled")
        self.assertEqual(record["session_id"], session_id)
        result = self.wait("worker", timeout_seconds=1)
        self.assertTrue(result["timeout"])
        self.assertEqual(result["reports"], [])

        self.message("worker", {"delay": 0.05, "result": "after"})
        report = self.wait("worker", timeout_seconds=5)
        self.assertEqual(report["reports"][0]["text"], "after")
        starts = self.starts()
        self.assertIn("--resume", starts[-1]["args"])
        self.assertEqual(starts[-1]["args"][starts[-1]["args"].index("--resume") + 1], session_id)

    def test_close_running_and_name_reuse(self):
        response = self.message("worker", {"delay": 30, "ignore_term": True})
        old_id = response["child_id"]
        self.wait_for_state("worker", {"running"})
        old_session = self.child_record("worker")["session_id"]
        self.message("worker", {"delay": 0.05, "result": "queued one"})
        self.message("worker", {"delay": 0.05, "result": "queued two"})
        close = self.call("close", agent="worker")
        self.assertEqual(close["dropped"], 2)
        self.assertIn("closed", close)
        self.wait_for_child_state(old_id, "closed", timeout=8)
        self.assertEqual(self.wait("worker", timeout_seconds=1)["reports"], [])

        response = self.message("worker", {"delay": 0.05, "result": "new"})
        self.assertTrue(response["created"])
        self.assertNotEqual(response["child_id"], old_id)
        report = self.wait("worker", timeout_seconds=5)
        self.assertEqual(report["reports"][0]["text"], "new")
        self.assertNotEqual(report["reports"][0]["session_id"], old_session)
        starts = self.starts()
        self.assertNotIn("--resume", starts[-1]["args"])

    def test_session_lock_rejects_overlap_and_releases(self):
        session_id = str(uuid.uuid4())
        self.message("first", {"delay": 30}, session_id=session_id)
        self.wait_for_state("first", {"running"})
        error = self.message("second", {"delay": 0.05}, session_id=session_id, expect=2)
        self.assertIn("session", error)
        self.assertIn("active", error)
        first = self.child_record("first")
        close = self.call("close", agent="first")
        self.assertIn("closed", close)
        self.wait_for_child_state(first["child_id"], "closed", timeout=8)

        self.message("second", {"delay": 0.05, "result": "ok"}, session_id=session_id)
        report = self.wait("second", timeout_seconds=5)
        self.assertEqual(report["reports"][0]["text"], "ok")
        starts = self.starts()
        self.assertIn("--resume", starts[-1]["args"])
        self.assertEqual(starts[-1]["args"][starts[-1]["args"].index("--resume") + 1], session_id)

    def test_unknown_supervisor_is_not_reported_as_idle(self):
        response = self.message("worker", {"delay": 30})
        child_id = response["child_id"]
        self.wait_for_state("worker", {"running"})
        record = self.child_record("worker")
        self.assertTrue(record["supervisor_pid"])
        run = self.latest_run(child_id)
        self.assertTrue(run["cake_pid"])
        os.kill(record["supervisor_pid"], signal.SIGKILL)
        try:
            deadline = time.monotonic() + 8
            state = None
            while time.monotonic() < deadline:
                state = self.status("worker")["children"][0]
                if state["state"] == "unknown":
                    break
                time.sleep(0.05)
            self.assertEqual(state["state"], "unknown")
            self.assertIn("may still be running", state["error"])
        finally:
            try:
                os.killpg(run["cake_pid"], signal.SIGKILL)
            except OSError:
                pass

    def test_validation_creation_only_instructions_and_child_message(self):
        for args in [{}, {"agent": "Bad", "message": "x"},
                     {"agent": "worker"}, {"agent": "worker", "message": "x", "extra": 1},
                     {"agent": "worker", "message": "x", "report": "sometimes"},
                     {"agent": "worker", "message": "x", "effort": "extreme"},
                     {"agent": "worker", "message": "x", "sandbox": "danger-full-access"},
                     {"agent": "worker", "message": "x", "session_id": "../../escape"}]:
            self.call("message", expect=2, **args)
        self.call("status", agent="missing", expect=2)

        response = self.message("worker", {"delay": 0.05, "result": "ok"},
                                model="m1", profile="p1", effort="high",
                                report="every_turn", instructions="Be terse.")
        self.assertTrue(response["created"])
        report = self.wait("worker", timeout_seconds=5)
        self.assertEqual(report["reports"][0]["text"], "ok")
        child = self.status("worker")["children"][0]
        self.assertEqual(child["model"], "m1")
        self.assertEqual(child["profile"], "p1")
        self.assertEqual(child["effort"], "high")
        self.assertEqual(child["report"], "every_turn")
        start = self.starts()[0]
        self.assertEqual(start["args"][start["args"].index("--model") + 1], "m1")
        self.assertEqual(start["args"][start["args"].index("--profile") + 1], "p1")
        self.assertEqual(start["args"][start["args"].index("--reasoning-effort") + 1], "high")
        self.assertEqual(start["system_prompt"], "Be terse.")

        error = self.call("message", agent="worker", message="{}", model="m2", expect=2)
        self.assertIn("creation-only", error)
        error = self.call("message", agent="worker", message="{}", cwd=str(self.workspace), expect=2)
        self.assertIn("creation-only", error)

        self.call("message", agent="worker", message=json.dumps({"delay": 0.05}),
                  report="never")
        self.wait_for_state("worker", {"idle"})
        self.assertEqual(self.status("worker")["children"][0]["report"], "never")

        error = self.call("message", agent="parent", message="x", expect=2)
        self.assertIn("reserved", error)

        child_env = dict(self.env,
                         CAKE_SUBAGENT_DEPTH="1",
                         CAKE_SUBAGENT_CHILD_ID=str(uuid.uuid4()),
                         CAKE_SUBAGENT_PARENT_OWNER=OWNER,
                         CAKE_SUBAGENT_NAME="kid")
        receipt = self.call("message", agent="parent", message="need help", env=child_env)
        self.assertEqual(receipt["to"], "parent")
        parent_wait = self.wait(timeout_seconds=2)
        messages = [item for item in parent_wait["reports"] if item.get("kind") == "message"]
        self.assertEqual(len(messages), 1)
        self.assertEqual(messages[0]["body"], "need help")
        self.assertEqual(messages[0]["agent"], "kid")

    def test_environment_model_override_wins(self):
        env = dict(self.env, CAKE_SUBAGENT_MODEL="env-model",
                   CAKE_SUBAGENT_PROFILE="env-profile")
        self.message("worker", {"delay": 0.05, "result": "ok"}, env=env,
                     model="explicit-model", profile="explicit-profile")
        self.wait("worker", timeout_seconds=5)
        child = self.status("worker")["children"][0]
        self.assertEqual(child["model"], "env-model")
        self.assertEqual(child["profile"], "env-profile")
        start = self.starts()[0]
        self.assertEqual(start["args"][start["args"].index("--model") + 1], "env-model")
        self.assertEqual(start["args"][start["args"].index("--profile") + 1], "env-profile")

    def test_configured_type_body_and_settings(self):
        target = self.directory / "typed workspace"
        config = target / ".agents/subagents/reviewer.md"
        config.parent.mkdir(parents=True)
        config.write_text("---\nname: reviewer\nsandbox: read-only\n"
                          "model: reviewer-model\nprofile: review\n---\nBaseline.\n")
        self.message("reviewer", {"delay": 0.05, "result": "ok"},
                     cwd=str(target), type="reviewer", sandbox="workspace-write",
                     instructions="Extra.")
        self.wait("reviewer", timeout_seconds=5)
        child = self.status("reviewer")["children"][0]
        self.assertEqual(child["type"], "reviewer")
        self.assertEqual(child["sandbox"], "read-only")
        self.assertEqual(child["model"], "reviewer-model")
        self.assertEqual(child["profile"], "review")
        start = self.starts()[0]
        self.assertEqual(start["cwd"], str(target.resolve()))
        self.assertEqual(start["args"][start["args"].index("--sandbox") + 1], "read-only")
        self.assertEqual(start["args"][start["args"].index("--model") + 1], "reviewer-model")
        self.assertEqual(start["args"][start["args"].index("--profile") + 1], "review")
        self.assertEqual(start["system_prompt"], "Baseline.\n\nExtra.")

    def test_partial_jsonl_and_timeout_report(self):
        self.message("split", {"split": True, "delay": 0.05, "result": "ok"})
        report = self.wait("split", timeout_seconds=5)
        self.assertEqual(report["reports"][0]["outcome"], "completed")
        self.assertEqual(report["reports"][0]["text"], "ok")

        self.message("slow", {"delay": 30}, timeout_seconds=1)
        report = self.wait("slow", timeout_seconds=8)
        self.assertEqual(report["reports"][0]["outcome"], "timed_out")
        self.assertEqual(self.wait_for_state("slow", {"idle"})["last_outcome"], "timed_out")

    def test_idle_exit_handoff_does_not_strand_messages(self):
        for index in range(8):
            self.message("worker", {"delay": 0.02, "result": f"r{index}"})
            result = self.wait("worker", timeout_seconds=5)
            self.assertEqual(result["reports"][0]["text"], f"r{index}")
            if index % 2:
                time.sleep(0.15)

        starts = self.starts()
        self.assertEqual(len(starts), 8)
        self.assertEqual([json.loads(start["prompt"])["result"] for start in starts],
                         [f"r{index}" for index in range(8)])
        events = [event for event in self.fake_events() if event.get("event") in {"start", "end"}]
        intervals = {}
        for event in events:
            pid = event["pid"]
            intervals.setdefault(pid, {})["event"] = event
            intervals[pid][event["event"]] = event["time"]
        spans = sorted((times.get("start", 0), times.get("end", float("inf")))
                       for times in intervals.values())
        for (_, previous_end), (next_start, _) in zip(spans, spans[1:]):
            self.assertLessEqual(previous_end, next_start)
        record = self.wait_for_state("worker", {"idle"})
        self.assertEqual(record["last_outcome"], "completed")
        self.assertEqual(record["state"], "idle")


if __name__ == "__main__":
    unittest.main()
