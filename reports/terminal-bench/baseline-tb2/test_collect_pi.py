"""Unit tests for collect_pi.py: pi fields, attribution and endings.

Each test builds a fake Harbor trial folder around the adapter's recorded pi
1.5.0 session (bench/harbor/testdata/pi-session.jsonl) and a fake relay log.
Stdlib only.

    python3 -I -m unittest discover -s reports/terminal-bench/baseline-tb2 -p 'test_collect_pi.py'
"""

import json
import os
import shutil
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import collect  # noqa: E402
import collect_pi  # noqa: E402

SESSION = os.path.join(HERE, "..", "..", "..", "bench", "harbor", "testdata", "pi-session.jsonl")
T0 = datetime(2026, 11, 10, 6, 59, 0, tzinfo=timezone.utc)  # the opening status check
START = T0 + timedelta(seconds=5)  # the trial
A_START = START + timedelta(seconds=5)
SAMPLING = ('{"max_tokens": 8192, "model": "qwen3.8-flash-next-nvfp4-nvfp4", "reasoning_budget_tokens": 2048, '
            '"stream": true, "temperature": 0.6, "top_p": 0.95} messages=3 ua="pi"')


def iso(t):
    return t.isoformat().replace("+00:00", "Z")


class Trial:
    def __init__(self, root, *, reward=1.0, exc=None, pi_exit="0", agent_s=60, session=True, torn=False):
        self.job = os.path.join(root, "job")
        tdir = os.path.join(self.job, "git-leak-recovery__abc")
        os.makedirs(os.path.join(tdir, "agent", "pi", "sessions"))
        os.makedirs(os.path.join(tdir, "verifier"))
        self.a_end = A_START + timedelta(seconds=agent_s)
        self.end = self.a_end + timedelta(seconds=10)
        result = {
            "started_at": iso(START), "finished_at": iso(self.end),
            "agent_execution": {"started_at": iso(A_START), "finished_at": iso(self.a_end)},
            "agent_result": {"n_input_tokens": 2500, "n_cache_tokens": 1100, "n_output_tokens": 42,
                             "metadata": {"pi_exit": pi_exit, "pi_version": "1.5.0", "usage_source": "session",
                                          "model": collect.MODEL, "session_files": ["s.jsonl"]}},
            "verifier_result": {"rewards": {"reward": reward}} if reward is not None else None,
            "exception_info": {"exception_type": exc, "exception_message": ""} if exc else None,
        }
        json.dump(result, open(os.path.join(tdir, "result.json"), "w"))
        json.dump({"results": {"tests": [{"name": "t", "status": "passed" if reward else "failed"}]}},
                  open(os.path.join(tdir, "verifier", "ctrf.json"), "w"))
        if session:
            dst = os.path.join(tdir, "agent", "pi", "sessions", "s.jsonl")
            shutil.copy(SESSION, dst)
            if torn:
                with open(dst, "a") as f:
                    f.write('{"type":"message","timestamp":"2026-11-10T07:00:07')


class CollectPiTest(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="collect-pi-")
        self.log = os.path.join(self.root, "relay-pi.log")
        self.saved = collect.RELAY_LOG
        collect.RELAY_LOG = self.log

    def tearDown(self):
        collect.RELAY_LOG = self.saved
        shutil.rmtree(self.root)

    def relay(self, trial, posts, close_late=()):
        """posts: (seconds after the agent's start, status). close_late are
        logged after the closing check, as a request cut off at the end is."""
        lines = [f"{T0.isoformat()} GET /v1/status 200"]
        lines += [f"{(A_START + timedelta(seconds=s)).isoformat()} POST /v1/chat/completions {st} {SAMPLING}"
                  for s, st in posts]
        lines.append(f"{(trial.end + timedelta(seconds=1)).isoformat()} GET /v1/status 200")
        lines += [f"{(A_START + timedelta(seconds=s)).isoformat()} POST /v1/chat/completions {st} {SAMPLING}"
                  for s, st in close_late]
        open(self.log, "w").write("\n".join(lines) + "\n")
        before = json.dumps({"requests": 100, "relay_lines": 1})
        after = json.dumps({"requests": 100 + len(posts) + len(close_late), "relay_lines": len(posts) + 2})
        return before, after

    def record(self, trial, before, after):
        return collect_pi.record(1, 1, "git-leak-recovery", trial.job, before, after)

    def test_completed_run(self):
        t = Trial(self.root)
        r = self.record(t, *self.relay(t, [(1, 200), (3, 200)]))
        self.assertEqual(r["ending"], "completed")
        self.assertEqual(r["reward"], 1.0)
        self.assertEqual(r["model_calls"], 2)
        self.assertEqual(r["tool_calls"], {"bash": 1})
        self.assertEqual(r["stop_reasons"], {"toolUse": 1, "stop": 1})
        self.assertEqual(r["other_entries"], {})
        self.assertEqual(r["latency_s"]["n"], 2)
        self.assertEqual(r["contention"], {"server_requests": 2, "posts_started": 2, "surplus": 0})
        self.assertEqual(r["retries_or_cancelled"], 0)
        self.assertEqual(r["usage"], {"prompt_tokens": 2500, "cached_tokens": 1100, "completion_tokens": 42})
        self.assertEqual(r["attribution"]["own_502_list"], [])

    def test_recovered_502_is_not_infra(self):
        t = Trial(self.root)
        r = self.record(t, *self.relay(t, [(1, 502), (2, 200), (3, 200)]))
        self.assertEqual(r["ending"], "completed")
        self.assertEqual(r["attribution"]["own_502_list"][0]["recovered"], True)
        self.assertEqual(r["attribution"]["unrecovered_502"], 0)
        self.assertEqual(r["retries_or_cancelled"], 1)

    def test_unrecovered_502_is_infra(self):
        t = Trial(self.root, pi_exit="1")
        r = self.record(t, *self.relay(t, [(1, 200), (3, 502)]))
        self.assertEqual(r["ending"], "infra_error")
        self.assertEqual(r["attribution"]["unrecovered_502"], 1)

    def test_timeout_cancels_the_last_request(self):
        t = Trial(self.root, reward=0.0, exc="AgentTimeoutError", pi_exit=None, agent_s=900)
        r = self.record(t, *self.relay(t, [(1, 200), (3, 200)], close_late=[(890, 502)]))
        self.assertEqual(r["ending"], "harbor_timeout")
        self.assertEqual(len(r["attribution"]["cancelled_at_end"]), 1)
        self.assertEqual(r["attribution"]["unrecovered_502"], 0)
        self.assertEqual(r["contention"]["surplus"], 0)

    def test_torn_last_line(self):
        t = Trial(self.root, torn=True)
        r = self.record(t, *self.relay(t, [(1, 200), (3, 200)]))
        self.assertEqual(r["model_calls"], 2)
        self.assertEqual(r["ending"], "completed")

    def test_no_session_file(self):
        t = Trial(self.root, session=False)
        r = self.record(t, *self.relay(t, [(1, 200)]))
        self.assertEqual(r["model_calls"], 0)
        self.assertEqual(r["usage"]["prompt_tokens"], 2500)
        self.assertEqual(r["retries_or_cancelled"], 1)

    def test_no_exit_without_timeout_is_a_crash(self):
        t = Trial(self.root, reward=0.0, pi_exit=None)
        r = self.record(t, *self.relay(t, [(1, 200)]))
        self.assertEqual(r["ending"], "agent_crash")

    def test_exception_before_the_agent_ran_is_infra(self):
        t = Trial(self.root, reward=None, exc="RuntimeError", pi_exit=None)
        r = self.record(t, *self.relay(t, []))
        self.assertEqual(r["ending"], "infra_error")

    def test_unsaved_backend_counts(self):
        t = Trial(self.root)
        self.relay(t, [(1, 200)])
        r = self.record(t, "{}", "{}")
        self.assertEqual(r["contention"], {"error": "backend counts not saved"})

    def test_stop_rule(self):
        ok = {"ending": "completed", "contention": {"surplus": 0}, "seq": 1}
        self.assertIsNone(collect_pi.stop_rule([ok] * 12))
        infra = dict(ok, ending="infra_error")
        self.assertIn("infra", collect_pi.stop_rule([infra] * 5))
        self.assertIn("infra", collect_pi.stop_rule([ok] * 8 + [infra] * 2 + [ok] * 2 + [infra]))
        busy = dict(ok, contention={"surplus": 1})
        self.assertIn("contended", collect_pi.stop_rule([ok, busy, busy, busy]))
        self.assertIn("agent_crash", collect_pi.stop_rule([ok, dict(ok, ending="agent_crash", seq=2)]))


if __name__ == "__main__":
    unittest.main()
