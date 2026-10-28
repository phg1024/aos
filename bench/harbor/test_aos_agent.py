"""Run with Harbor's Python: bench/harbor/run-tb.sh --self-test"""

import json
import tempfile
import unittest
from pathlib import Path

from aos_agent import agent_timeout_sec, container_base_url, parse_run_summary


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


if __name__ == "__main__":
    unittest.main()
