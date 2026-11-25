"""Offline tests for the single public subagent tool.

These drive the real `subagent` symlink through the toolbox protocol, with a
fake Cake executable. No model calls and no network.
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
TOOL = TOOLS / "subagent"
BINARY = TOOLS / "cake-subagent"
PARENT_ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
OWNER = f"session-{PARENT_ID}"
LEGACY = ["subagent_async", "subagent_cancel", "subagent_inbox",
          "subagent_peers", "subagent_send", "subagent_status", "subagent_wait"]

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
result_value = scenario.get("result", "finished")
if scenario.get("result_file"):
    result_value = Path(scenario["result_file"]).read_text()
emit({"type": "task_complete", "session_id": sid,
      "task_id": str(uuid.uuid4()) if scenario.get("wrong_task") else tid,
      "subtype": scenario.get("subtype", "success"),
      "result": result_value,
      "error": "provider failed" if scenario.get("subtype") == "error_during_execution" else None,
      "duration_ms": 100, "turn_count": 1, "tool_call_count": 0, "usage": {}})
log({"event": "end", "pid": os.getpid(), "time": time.time(),
     "sid": sid, "tid": tid, "outcome": scenario.get("subtype", "success")})
sys.exit(scenario.get("exit_code", 0))
'''


def timestamp():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


class SubagentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="cake-subagent-test-")
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
                        FAKE_CAKE_LOG=str(self.log))
        for key in ["CAKE_SUBAGENT_PARENT_OWNER", "CAKE_SUBAGENT_CHILD_ID",
                    "CAKE_SUBAGENT_NAME", "CAKE_SUBAGENT_RUN_ID",
                    "CAKE_SUBAGENT_SESSION_ID", "CAKE_SUBAGENT_MODEL",
                    "CAKE_SUBAGENT_PROFILE", "CAKE_TOOLBOX_SUBAGENT_DEPTH"]:
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
        result = subprocess.run([str(TOOL)], input=json.dumps(payload),
                                text=True, capture_output=True, cwd=self.workspace,
                                env=dict(env or self.env, TOOLBOX_ACTION="execute"), timeout=20)
        if expect:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            return result.stderr
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def describe(self):
        result = subprocess.run([str(TOOL)], input="{}", text=True,
                                capture_output=True, cwd=self.workspace,
                                env=dict(self.env, TOOLBOX_ACTION="describe"), timeout=12)
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout)

    def hook(self, event="SessionStart", *, env=None, session_id=PARENT_ID,
             follow_up_index=None, max_follow_ups=None, expect=0):
        payload = {"version": 1, "session_id": session_id, "hook_event_name": event}
        if follow_up_index is not None:
            payload["follow_up_index"] = follow_up_index
        if max_follow_ups is not None:
            payload["max_follow_ups"] = max_follow_ups
        result = subprocess.run([str(TOOL), "event"], input=json.dumps(payload),
                                text=True, capture_output=True, cwd=self.workspace,
                                env=dict(env or self.env), timeout=12)
        if expect:
            self.assertNotEqual(result.returncode, 0, result.stdout)
            return result.stderr
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(result.stdout) if result.stdout.strip() else {}

    def hooks(self, *args, env=None):
        result = subprocess.run([str(TOOL), "hooks", *args], text=True,
                                capture_output=True, cwd=self.workspace,
                                env=dict(env or self.env), timeout=12)
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

    def wait_for_inbox(self, count, timeout=10):
        # A second message to an idle child returns while the old child record
        # still says idle, so state alone is not a completion signal. Inbox
        # files survive cursor advancement, so their count is cumulative.
        deadline = time.monotonic() + timeout
        last = []
        while time.monotonic() < deadline:
            last = list((self.owner_dir / "inbox").glob("*.json"))
            if len(last) >= count:
                return last
            time.sleep(0.05)
        self.fail(f"inbox did not reach {count} items: {[path.name for path in last]}")

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

    def test_describe_and_install_surface(self):
        description = self.describe()
        self.assertEqual(description["name"], "subagent")
        self.assertEqual(description["replay"], "never")
        self.assertGreater(description["timeout"], 300)
        schema = description["inputSchema"]
        self.assertFalse(schema["additionalProperties"])
        self.assertEqual(schema["required"], ["action"])
        self.assertEqual(schema["properties"]["action"]["enum"],
                         ["message", "wait", "status", "stop", "close"])
        self.assertIn("allOf", schema)
        self.assertTrue(TOOL.exists())
        for name in LEGACY:
            self.assertFalse((TOOLS / name).exists(), name)

        root = Path(__file__).resolve().parents[1]
        dest = self.directory / "tools"
        dest.mkdir()
        config = self.directory / "config"
        binary = BINARY.resolve()
        (dest / "subagent_async").symlink_to(binary)
        (dest / "subagent_status").write_text("user file\n")
        legacy_hook = config / "cake/hooks/subagent-inbox.sh"
        legacy_hook.parent.mkdir(parents=True)
        legacy_hook.symlink_to(root / "README.md")
        env = dict(self.env, CAKE_TOOLBOX_DEST=str(dest), XDG_CONFIG_HOME=str(config))

        install = subprocess.run([str(root / "bin/install")], text=True,
                                 capture_output=True, cwd=self.workspace, env=env, timeout=12)
        self.assertEqual(install.returncode, 0, install.stderr)
        self.assertTrue((dest / "subagent").is_symlink())
        self.assertEqual((dest / "subagent").resolve(), binary)
        self.assertFalse((dest / "subagent_async").exists())
        self.assertFalse(legacy_hook.exists())
        self.assertTrue((dest / "subagent_status").exists())
        installed_hook = config / "cake/hooks/subagent-reports.sh"
        self.assertTrue(installed_hook.is_symlink())
        self.assertEqual(installed_hook.resolve(), root / "hooks/subagent-reports.sh")
        document = json.loads((config / "cake/hooks.json").read_text())
        for event in ["SessionStart", "Stop"]:
            commands = [item["command"] for group in document["hooks"][event]
                        for item in group["hooks"]]
            self.assertTrue(any("subagent-reports.sh" in item for item in commands), event)
        self.assertNotIn("PreToolUse", document["hooks"])

        doctor = subprocess.run([str(root / "bin/doctor")], text=True,
                                capture_output=True, cwd=self.workspace, env=env, timeout=12)
        self.assertEqual(doctor.returncode, 0, doctor.stderr)
        self.assertIn("ok: subagent", doctor.stdout)
        self.assertIn("ok: hooks SessionStart, Stop", doctor.stdout)

        pretool = subprocess.run([str(root / "bin/install"), "--pre-tool-use"], text=True,
                                 capture_output=True, cwd=self.workspace, env=env, timeout=12)
        self.assertEqual(pretool.returncode, 0, pretool.stderr)
        document = json.loads((config / "cake/hooks.json").read_text())
        self.assertIn("PreToolUse", document["hooks"])
        doctor = subprocess.run([str(root / "bin/doctor")], text=True,
                                capture_output=True, cwd=self.workspace, env=env, timeout=12)
        self.assertEqual(doctor.returncode, 0, doctor.stderr)
        self.assertIn("note: PreToolUse report hook is installed", doctor.stdout)

        uninstall = subprocess.run([str(root / "bin/uninstall")], text=True,
                                   capture_output=True, cwd=self.workspace, env=env, timeout=12)
        self.assertEqual(uninstall.returncode, 0, uninstall.stderr)
        self.assertFalse((dest / "subagent").exists())
        self.assertTrue((dest / "subagent_status").exists())
        self.assertFalse(installed_hook.exists())
        document = json.loads((config / "cake/hooks.json").read_text())
        for event in ["SessionStart", "Stop"]:
            self.assertNotIn(event, document.get("hooks", {}))

    def test_any_successful_action_drains_reports(self):
        self.message("first", {"delay": 0.05, "result": "one"})
        self.wait_for_state("first", {"idle"})
        second = self.message("second", {"delay": 0.5, "result": "two"})
        self.assertEqual([report["text"] for report in second["reports"]], ["one"])
        self.wait_for_state("second", {"idle"})
        stopped = self.call("stop", agent="second")
        self.assertEqual([report["text"] for report in stopped["reports"]], ["two"])

    def test_hook_delivers_reports_exactly_once(self):
        first_message = self.message("worker", {"delay": 0.05, "result": "hook one"})
        self.assertTrue(first_message["created"])
        self.wait_for_inbox(1)
        first = self.hook("SessionStart")
        self.assertIn("hook one", first["additional_context"])
        self.assertIn("untrusted", first["additional_context"])
        self.assertEqual(self.hook("SessionStart"), {})
        self.assertEqual(self.status()["reports"], [])

        second_message = self.message("worker", {"delay": 0.05, "result": "hook two"})
        self.assertFalse(second_message["created"])
        self.wait_for_inbox(2)
        child_env = dict(self.env, CAKE_SUBAGENT_DEPTH="1")
        self.assertEqual(self.hook("SessionStart", env=child_env), {})
        second = self.hook("UserPromptSubmit")
        self.assertIn("hook two", second["additional_context"])
        self.assertEqual(self.hook("PreToolUse"), {})
        self.assertEqual(self.status()["reports"], [])

    def test_stop_hook_requests_one_follow_up_and_preserves_spillover(self):
        self.message("worker", {"delay": 0.05, "result": "stop one"})
        self.wait_for_inbox(1)
        decision = self.hook("Stop", follow_up_index=0, max_follow_ups=1)
        self.assertTrue(decision["follow_up"])
        self.assertIn("stop one", decision["additional_context"])

        self.message("worker", {"delay": 0.05, "result": "stop two"})
        self.wait_for_inbox(2)
        self.assertEqual(self.hook("Stop", follow_up_index=1, max_follow_ups=1), {})
        self.assertEqual(self.hook("Stop", follow_up_index=0, max_follow_ups=0), {})
        late = self.hook("SessionStart")
        self.assertIn("stop two", late["additional_context"])
        self.assertEqual(self.status()["reports"], [])

    def test_pre_tool_use_and_tool_share_one_cursor(self):
        self.message("worker", {"delay": 0.05, "result": "pre one"})
        self.wait_for_inbox(1)
        decision = self.hook("PreToolUse")
        self.assertIn("pre one", decision["additional_context"])
        self.assertEqual(self.status()["reports"], [])
        self.assertEqual(self.hook("PreToolUse"), {})

    def test_hook_report_policy(self):
        self.message("broken", {"startup_failure": True}, report="never")
        self.wait_for_inbox(1)
        context = self.hook("SessionStart")["additional_context"]
        self.assertIn("failed", context)
        self.assertIn("invalid provider configuration", context)

        self.message("quiet", {"delay": 0.05, "result": "done"}, report="never")
        self.wait_for_state("quiet", {"idle"})
        self.assertEqual(self.hook("SessionStart"), {})

        self.message("slow", {"delay": 30, "ignore_term": True})
        self.wait_for_state("slow", {"running"})
        self.call("stop", agent="slow")
        self.wait_for_state("slow", {"idle"})
        self.assertEqual(self.hook("SessionStart"), {})

    def test_hook_strips_control_characters_and_bounds_reports(self):
        self.message("ansi", {"delay": 0.05,
                              "result": "\u001b[31mred\u001b[0m\u0000done"})
        self.wait_for_inbox(1)
        context = self.hook("SessionStart")["additional_context"]
        self.assertIn("done", context)
        self.assertNotIn("\u001b", context)
        self.assertNotIn("\u0000", context)

        first_file = self.directory / "first-report.txt"
        second_file = self.directory / "second-report.txt"
        first_file.write_text("q" * 100_000)
        second_file.write_text("z" * 100_000)
        # Queue both turns before the first report exists, because every
        # successful message action drains pending reports.
        self.message("big", {"delay": 2, "result_file": str(first_file)})
        self.message("big", {"delay": 0.05, "result_file": str(second_file)})
        self.wait_for_inbox(3)

        first = self.hook("SessionStart")
        first_context = first["additional_context"]
        self.assertIn("qqqq", first_context)
        self.assertNotIn("zzzz", first_context)
        self.assertIn("more item(s) pending", first_context)
        self.assertIn("text_truncated", first_context)
        self.assertLessEqual(len(first_context.encode()), 38_000)
        self.assertLessEqual(len(json.dumps(first).encode()), 65_536)

        second = self.hook("SessionStart")
        second_context = second["additional_context"]
        self.assertIn("zzzz", second_context)
        self.assertNotIn("more item(s) pending", second_context)
        self.assertLessEqual(len(second_context.encode()), 38_000)
        self.assertEqual(self.hook("SessionStart"), {})

        child_id = self.child_record("big")["child_id"]
        results = [json.loads(path.read_text())["result"]
                   for path in (self.children_root / child_id / "runs").glob("*/result.json")]
        self.assertEqual(len(results), 2)
        self.assertTrue(all(len(result) == 100_000 for result in results))

    def test_hooks_install_merges_preserves_and_is_idempotent(self):
        config = self.directory / "config"
        hooks_path = config / "cake/hooks.json"
        hooks_path.parent.mkdir(parents=True)
        existing = {
            "version": 1,
            "hooks": {
                "SessionStart": [{"matcher": "*", "hooks": [
                    {"type": "command", "command": "echo existing", "timeout": 10}]}],
                "Stop": [{"hooks": [
                    {"type": "command",
                     "command": "\"${XDG_CONFIG_HOME:-$HOME/.config}/cake/hooks/subagent-inbox.sh\""}]}],
                "UserPromptSubmit": [{"hooks": [
                    {"type": "command", "command": "echo prompt"}]}],
            },
        }
        hooks_path.write_text(json.dumps(existing))
        env = dict(self.env, XDG_CONFIG_HOME=str(config))

        result = self.hooks("install", env=env)
        self.assertTrue(result["changed"])
        self.assertEqual(result["legacy_removed"], 1)
        document = json.loads(hooks_path.read_text())
        self.assertEqual(document["version"], 1)
        self.assertIn("UserPromptSubmit", document["hooks"])
        self.assertNotIn("subagent-inbox.sh", hooks_path.read_text())
        for event in ["SessionStart", "Stop"]:
            commands = [item["command"] for group in document["hooks"][event]
                        for item in group["hooks"]]
            self.assertEqual(sum("subagent-reports.sh" in item for item in commands), 1)
        self.assertIn("echo existing", hooks_path.read_text())
        self.assertNotIn("PreToolUse", document["hooks"])

        before = hooks_path.read_text()
        unchanged = self.hooks("install", env=env)
        self.assertFalse(unchanged["changed"])
        self.assertEqual(hooks_path.read_text(), before)

        self.hooks("install", "--pre-tool-use", env=env)
        document = json.loads(hooks_path.read_text())
        self.assertIn("PreToolUse", document["hooks"])
        self.hooks("install", env=env)
        document = json.loads(hooks_path.read_text())
        self.assertIn("PreToolUse", document["hooks"])

        removed = self.hooks("uninstall", "--pre-tool-use", env=env)
        self.assertEqual(removed["removed"], 1)
        document = json.loads(hooks_path.read_text())
        self.assertNotIn("PreToolUse", document["hooks"])
        self.assertIn("SessionStart", document["hooks"])
        self.assertIn("Stop", document["hooks"])

        removed = self.hooks("uninstall", env=env)
        self.assertEqual(removed["removed"], 2)
        document = json.loads(hooks_path.read_text())
        self.assertNotIn("Stop", document["hooks"])
        self.assertIn("SessionStart", document["hooks"])
        self.assertEqual(document["hooks"]["SessionStart"][0]["hooks"][0]["command"],
                         "echo existing")
        self.assertIn("UserPromptSubmit", document["hooks"])

        hooks_path.write_text("{not json\n")
        broken = subprocess.run([str(TOOL), "hooks", "status"], text=True,
                                capture_output=True, cwd=self.workspace, env=env, timeout=12)
        self.assertNotEqual(broken.returncode, 0)
        self.assertEqual(hooks_path.read_text(), "{not json\n")

    def test_installed_hook_command_runs_the_report_script(self):
        root = Path(__file__).resolve().parents[1]
        dest = self.directory / "tools"
        dest.mkdir()
        config = self.directory / "config"
        env = dict(self.env, CAKE_TOOLBOX_DEST=str(dest), XDG_CONFIG_HOME=str(config))
        install = subprocess.run([str(root / "bin/install")], text=True,
                                 capture_output=True, cwd=self.workspace, env=env, timeout=12)
        self.assertEqual(install.returncode, 0, install.stderr)

        self.message("worker", {"delay": 0.05, "result": "wrapper report"})
        self.wait_for_state("worker", {"idle"})
        document = json.loads((config / "cake/hooks.json").read_text())
        command = next(item["command"] for group in document["hooks"]["SessionStart"]
                       for item in group["hooks"] if "subagent-reports.sh" in item["command"])
        payload = {"version": 1, "session_id": PARENT_ID, "hook_event_name": "SessionStart"}
        result = subprocess.run(["/bin/sh", "-c", command], input=json.dumps(payload),
                                text=True, capture_output=True, cwd=self.workspace,
                                env=env, timeout=12)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("wrapper report", json.loads(result.stdout)["additional_context"])

        again = subprocess.run(["/bin/sh", "-c", command], input=json.dumps(payload),
                               text=True, capture_output=True, cwd=self.workspace,
                               env=env, timeout=12)
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertEqual(again.stdout.strip(), "")

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
        self.assertIsNone(start["legacy_depth"])
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
            result = self.wait("worker", timeout_seconds=8)
            self.assertEqual(result["reports"][0]["outcome"], "unknown")
            state = self.status("worker")["children"][0]
            self.assertEqual(state["state"], "unknown")
            self.assertIn("may still be running", state["error"])
            self.assertEqual(self.status("worker")["reports"], [])
            error = self.message("worker", {"delay": 0.05}, expect=2)
            self.assertIn("unknown", error)
        finally:
            try:
                os.killpg(run["cake_pid"], signal.SIGKILL)
            except OSError:
                pass

    def test_validation_creation_only_instructions_and_child_message(self):
        self.call("nope", expect=2)
        self.call("status", agent="worker", extra=1, expect=2)
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
