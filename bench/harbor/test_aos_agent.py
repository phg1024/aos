"""Run with Harbor's Python: bench/harbor/run-tb.sh --self-test"""

import json
import tempfile
import unittest
from pathlib import Path

from aos_agent import agent_timeout_sec, container_base_url, parse_run_summary, usage_from_ledger


class ContainerBaseURLTest(unittest.TestCase):
    def test_rewrites_loopback_hosts(self):
        self.assertEqual(container_base_url("http://127.0.0.1:8080"), "http://host.docker.internal:8080")
        self.assertEqual(container_base_url("http://localhost:11434/v1"), "http://host.docker.internal:11434/v1")

    def test_keeps_remote_hosts(self):
        self.assertEqual(container_base_url("http://phg-gpu.local:8080"), "http://phg-gpu.local:8080")


class ParseRunSummaryTest(unittest.TestCase):
    def test_reads_trailing_summary_after_trace(self):
        output = '[10:00:00] solver (pid=pid-000002) TOOL_CALL\n  run command={"x": 1}\n{\n  "workflow": "terminal-bench",\n  "usage": {"prompt_tokens": 10}\n}\n'
        summary = parse_run_summary(output)
        self.assertEqual(summary["workflow"], "terminal-bench")
        self.assertEqual(summary["usage"]["prompt_tokens"], 10)

    def test_ignores_failure_line_after_summary(self):
        output = '{\n  "exit_status": {"code": 3, "reason": "entry agent solver exited with code 3: bad {x}"}\n}\nworkflow failed: entry agent solver exited with code 3: bad {x}\n'
        summary = parse_run_summary(output)
        self.assertEqual(summary["exit_status"]["code"], 3)

    def test_missing_summary(self):
        self.assertIsNone(parse_run_summary("panic: boom\n"))


class AgentTimeoutTest(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.task = self.dir / "task.toml"
        self.task.write_text('[agent]\ntimeout_sec = 28800.0\n')
        self.job = self.dir / "config.json"

    def write_job(self, config):
        self.job.write_text(json.dumps(config))

    def test_applies_agent_multiplier(self):
        self.write_job({"agent_timeout_multiplier": 0.25, "agents": [{}]})
        self.assertEqual(agent_timeout_sec(self.task, self.job), 7200.0)

    def test_defaults_to_task_timeout(self):
        self.write_job({"agents": [{}]})
        self.assertEqual(agent_timeout_sec(self.task, self.job), 28800.0)

    def test_override_and_max(self):
        self.write_job({"timeout_multiplier": 2, "agents": [{"override_timeout_sec": 600, "max_timeout_sec": 500}]})
        self.assertEqual(agent_timeout_sec(self.task, self.job), 1000.0)

    def test_missing_task_toml(self):
        self.assertIsNone(agent_timeout_sec(self.dir / "absent.toml", self.job))


class UsageFromLedgerTest(unittest.TestCase):
    def test_totals_skip_a_torn_last_line(self):
        path = Path(tempfile.mkdtemp()) / "calls.jsonl"
        path.write_text(
            '{"seq":1,"prompt_tokens":100,"cached_tokens":60,"completion_tokens":7}\n'
            '{"seq":2,"prompt_tokens":120,"cached_tokens":100,"completion_tokens":9}\n'
            '{"seq":3,"prompt_tok'
        )
        self.assertEqual(usage_from_ledger(path), {"requests": 2, "prompt_tokens": 220, "cached_tokens": 160, "completion_tokens": 16})

    def test_missing_ledger(self):
        self.assertIsNone(usage_from_ledger(Path(tempfile.mkdtemp()) / "absent.jsonl"))


if __name__ == "__main__":
    unittest.main()
