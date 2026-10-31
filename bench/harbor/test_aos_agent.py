"""Run with Harbor's Python: bench/harbor/run-tb.sh --self-test"""

import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from aos_agent import (
    AOSAgent,
    agent_timeout_sec,
    container_base_url,
    parse_run_summary,
    time_budget_flag,
    time_budget_mode,
    usage_from_ledger,
)


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


class TimeBudgetFlagTest(unittest.TestCase):
    def test_strict_budget_leaves_a_margin(self):
        self.assertEqual(time_budget_flag(900, 30), "--time-budget 850s --strict-time-budget ")

    def test_no_budget_without_a_timeout_or_room(self):
        self.assertEqual(time_budget_flag(None, 0), "")
        self.assertEqual(time_budget_flag(90, 20), "")

    def test_advisory_budget_is_not_strict(self):
        self.assertEqual(time_budget_flag(900, 30, "advisory"), "--time-budget 850s ")
        self.assertEqual(time_budget_flag(None, 0, "advisory"), "")

    def test_off_gives_no_budget(self):
        self.assertEqual(time_budget_flag(900, 30, "off"), "")


class TimeBudgetModeTest(unittest.TestCase):
    def test_kwarg_then_environment_then_strict(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(time_budget_mode(None), "strict")
            self.assertEqual(time_budget_mode("Off"), "off")
        with mock.patch.dict(os.environ, {"AOS_TIME_BUDGET": "advisory"}):
            self.assertEqual(time_budget_mode(None), "advisory")
            self.assertEqual(time_budget_mode("strict"), "strict")

    def test_rejects_unknown_modes(self):
        for value in ("none", "", False):
            with self.assertRaises(ValueError):
                time_budget_mode(value)
        with mock.patch.dict(os.environ, {"AOS_TIME_BUDGET": "loose"}):
            with self.assertRaises(ValueError):
                time_budget_mode(None)

    def test_agent_takes_the_kwarg(self):
        logs = Path(tempfile.mkdtemp())
        with mock.patch.dict(os.environ, {}, clear=True):
            self.assertEqual(AOSAgent(logs_dir=logs)._time_budget, "strict")
            self.assertEqual(AOSAgent(logs_dir=logs, time_budget="off")._time_budget, "off")
            with self.assertRaises(ValueError):
                AOSAgent(logs_dir=logs, time_budget="sometimes")


if __name__ == "__main__":
    unittest.main()
