#!/usr/bin/env python3
"""Tests analyze.py on a state directory that was copied off the machine that ran it. Usage: python3 test_analyze.py"""
import json, os, shutil, subprocess, sys, tempfile, unittest

ANALYZE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "analyze.py")
FOREIGN = "/home/elsewhere/aos-ipc-live/reports/ipc-live/runs/gone/state/artifacts/payloads"
PAYLOADS = {
    "art-000001": ("tool_result", {"tool": "message_agent", "output": {"to": "pid-000002", "body": "the numbers", "artifacts": "art-000002"}}),
    "art-000002": ("published.raw_timings", {"body": "twelve bytes"}),
    "art-000003": ("tool_result", {"tool": "read_artifact", "output": {"artifact_id": "art-000002", "offset": 0, "next_offset": 0}}),
    "art-000004": ("verification", {"status": "pass", "reason": "the numbers hold"}),
}


def write_state(root, index_dir=FOREIGN):
    for sub in ("artifacts/payloads", "kernel", "run"):
        os.makedirs(os.path.join(root, sub))
    index = {}
    for artifact, (kind, payload) in PAYLOADS.items():
        with open(os.path.join(root, "artifacts", "payloads", artifact + ".json"), "w") as f:
            json.dump(payload, f)
        index[artifact] = {"id": artifact, "type": kind, "producer_pid": "pid-000001", "created_at": "2026-11-06T07:00:0" + artifact[-1] + "Z",
                           "path": os.path.join(index_dir, artifact + ".json")}
    with open(os.path.join(root, "artifacts", "index.json"), "w") as f:
        json.dump(index, f)
    processes = [{"pid": "pid-000001", "metadata": {"agent_id": "writer"}}, {"pid": "pid-000002", "metadata": {"agent_id": "verifier"}}]
    with open(os.path.join(root, "kernel", "processes.json"), "w") as f:
        json.dump(processes, f)
    with open(os.path.join(root, "kernel", "events.json"), "w") as f:
        json.dump([], f)


def run(root):
    return subprocess.run([sys.executable, "-I", ANALYZE, root], capture_output=True, text=True)


class Copied(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir)
        self.state = os.path.join(self.dir, "state")
        write_state(self.state)

    def test_payloads_come_from_the_given_directory(self):
        result = run(self.state)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("art-000002 (published.raw_timings) from writer (pid-000001) to verifier (pid-000002)", result.stdout)
        self.assertIn("published: art-000002 published.raw_timings by writer (pid-000001), 12 bytes", result.stdout)
        self.assertIn("verdict art-000004: pass: the numbers hold", result.stdout)
        self.assertIn("exercised 1      a handle attached to mail", result.stdout)
        self.assertNotIn("no handle was attached", result.stdout)

    def test_the_same_files_give_the_same_answer_anywhere(self):
        moved = os.path.join(self.dir, "somewhere", "else")
        os.makedirs(os.path.dirname(moved))
        shutil.copytree(self.state, moved)
        here, there = run(self.state), run(moved)
        self.assertEqual(here.returncode, 0, here.stderr)
        self.assertEqual(here.stdout.split("\n", 1)[1], there.stdout.split("\n", 1)[1])

    def test_a_path_that_exists_elsewhere_is_not_read(self):
        other = os.path.join(self.dir, "other-run")
        os.makedirs(other)
        with open(os.path.join(other, "art-000004.json"), "w") as f:
            json.dump({"status": "fail", "reason": "another run's verdict"}, f)
        shutil.rmtree(self.state)
        write_state(self.state, other)
        self.assertIn("verdict art-000004: pass", run(self.state).stdout)

    def test_a_missing_payload_is_an_error_naming_it(self):
        os.remove(os.path.join(self.state, "artifacts", "payloads", "art-000002.json"))
        result = run(self.state)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("art-000002", result.stderr)
        self.assertNotIn("Traceback", result.stderr)
        self.assertNotIn("Contract situations", result.stdout)

    def test_an_unreadable_payload_is_an_error_naming_it(self):
        with open(os.path.join(self.state, "artifacts", "payloads", "art-000004.json"), "w") as f:
            f.write('{"status": "pa')
        result = run(self.state)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("art-000004", result.stderr)
        self.assertNotIn("Traceback", result.stderr)


unittest.main()
