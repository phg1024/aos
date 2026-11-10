"""Run with Harbor's Python: bench/harbor/run-tb.sh --self-test"""

import asyncio
import io
import json
import os
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import pi_agent
from harbor.models.agent.context import AgentContext
from pi_agent import (
    PI_ENV,
    PI_VERSION,
    PLACEHOLDER_API_KEY,
    PROVIDER,
    PiAgent,
    endpoint_settings,
    extract_bundle,
    models_config,
    openai_base_url,
    pi_command,
    pi_exit_from_output,
    release_tarball,
)

TESTDATA = Path(__file__).resolve().parent / "testdata"
SECRET = "sk-test-not-a-real-key-1234567890"
SECRET_ENV = {"AOS_LLM_API_KEY": SECRET, "OPENAI_API_KEY": SECRET, "ANTHROPIC_API_KEY": SECRET, "STRATA_KEY": SECRET}
BASELINE_ENV = {"AOS_LLM_BASE_URL": "http://127.0.0.1:18080", "AOS_LLM_MODEL": "qwen3.8-flash-next-nvfp4-nvfp4"}


class OpenAIBaseURLTest(unittest.TestCase):
    def test_rewrites_loopback_and_adds_v1(self):
        self.assertEqual(openai_base_url("http://127.0.0.1:18080"), "http://host.docker.internal:18080/v1")
        self.assertEqual(openai_base_url("http://localhost:18080/"), "http://host.docker.internal:18080/v1")

    def test_keeps_an_existing_v1_and_remote_hosts(self):
        self.assertEqual(openai_base_url("http://127.0.0.1:18080/v1"), "http://host.docker.internal:18080/v1")
        self.assertEqual(openai_base_url("http://relay.example:8080/v1/"), "http://relay.example:8080/v1")


class EndpointSettingsTest(unittest.TestCase):
    def test_defaults_are_the_aos_arms(self):
        settings = endpoint_settings({})
        self.assertEqual(settings["AOS_LLM_TEMPERATURE"], "0.6")
        self.assertEqual(settings["AOS_LLM_TOP_P"], "0.95")
        self.assertEqual(settings["AOS_LLM_MAX_TOKENS"], "8192")
        self.assertEqual(settings["AOS_LLM_THINKING_BUDGET"], "2048")
        self.assertEqual(settings["AOS_LLM_CONTEXT_WINDOW"], "262144")

    def test_environment_overrides_and_no_key_is_read(self):
        settings = endpoint_settings({**BASELINE_ENV, **SECRET_ENV})
        self.assertEqual(settings["AOS_LLM_MODEL"], "qwen3.8-flash-next-nvfp4-nvfp4")
        self.assertEqual(settings["AOS_LLM_BASE_URL"], "http://127.0.0.1:18080")
        self.assertNotIn(SECRET, json.dumps(settings))


class ModelsConfigTest(unittest.TestCase):
    def test_the_baseline_model(self):
        config = models_config(endpoint_settings(BASELINE_ENV))
        provider = config["providers"][PROVIDER]
        self.assertEqual(provider["baseUrl"], "http://host.docker.internal:18080/v1")
        self.assertEqual(provider["api"], "openai-completions")
        self.assertEqual(provider["apiKey"], PLACEHOLDER_API_KEY)
        [model] = provider["models"]
        self.assertEqual(model["id"], "qwen3.8-flash-next-nvfp4-nvfp4")
        self.assertEqual(model["contextWindow"], 262144)
        self.assertEqual(model["maxTokens"], 8192)
        self.assertIs(model["reasoning"], False)
        self.assertEqual(model["compat"], {"maxTokensField": "max_tokens", "supportsStore": False})
        self.assertEqual(model["samplingParams"], {"temperature": 0.6, "top_p": 0.95, "reasoning_budget_tokens": 2048})
        # Values are numbers, as the request body needs them.
        self.assertIsInstance(model["samplingParams"]["reasoning_budget_tokens"], int)

    def test_holds_no_key(self):
        text = json.dumps(models_config(endpoint_settings({**BASELINE_ENV, **SECRET_ENV})))
        self.assertNotIn(SECRET, text)
        self.assertNotIn("$", text)  # no environment interpolation either


class PiCommandTest(unittest.TestCase):
    def test_flags(self):
        command = pi_command("qwen3.8-flash-next-nvfp4-nvfp4", "/logs/agent/pi/sessions", "/logs/agent/pi.txt")
        for part in (
            "/installed-agent/pi/pi-1.5.0/pi --print --mode json --no-approve",
            "--session-dir /logs/agent/pi/sessions",
            f"--provider {PROVIDER} --model qwen3.8-flash-next-nvfp4-nvfp4",
            '"$PI_INSTRUCTION" </dev/null',
            "> /logs/agent/pi.txt",
        ):
            self.assertIn(part, command)
        # Pi's defaults: no thinking level, tool list, prompt or turn limit,
        # and no key on the command line.
        for flag in ("--thinking", "--tools", "--system-prompt", "--append-system-prompt", "--extension", "--api-key"):
            self.assertNotIn(flag, command)

    def test_quotes_the_model(self):
        self.assertIn("--model 'a b;c'", pi_command("a b;c", "/s", "/o"))

    def test_records_the_exit_and_drops_streaming_deltas(self):
        tmp = Path(tempfile.mkdtemp())
        fake = tmp / "pi"
        fake.write_text(
            "#!/bin/sh\n"
            'echo \'{"type":"message_update","delta":"x"}\'\n'
            'echo \'{"type":"message_end","args":"\'"$*"\'"}\'\n'
            "exit 3\n"
        )
        fake.chmod(0o755)
        output = tmp / "logs" / "pi.txt"
        output.parent.mkdir()
        command = pi_command("m", str(tmp / "logs" / "pi" / "sessions"), str(output), binary=str(fake))
        result = subprocess.run(
            ["bash", "-c", f"set -o pipefail; {command}"],
            env={"PATH": os.environ["PATH"], "PI_INSTRUCTION": "fix it; don't $(rm) anything"},
            capture_output=True, text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        text = output.read_text()
        self.assertNotIn("message_update", text)
        self.assertIn("fix it; don't $(rm) anything", text)
        self.assertEqual(pi_exit_from_output(output), "3")
        self.assertTrue((tmp / "logs" / "pi" / "sessions").is_dir())

    def test_no_exit_line_after_a_kill(self):
        tmp = Path(tempfile.mkdtemp())
        self.assertIsNone(pi_exit_from_output(tmp / "absent.txt"))
        (tmp / "pi.txt").write_text('{"type":"message_end"}\n')
        self.assertIsNone(pi_exit_from_output(tmp / "pi.txt"))


def fake_release(directory: Path, members: dict[str, bytes]) -> Path:
    path = directory / "download" / "pi-linux-x64.tar.gz"
    path.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(path, "w:gz") as archive:
        for name, data in members.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o755
            archive.addfile(info, io.BytesIO(data))
    return path


class ReleaseTest(unittest.TestCase):
    def setUp(self):
        self.cache = Path(tempfile.mkdtemp())

    def test_pinned_hashes_are_sha256(self):
        self.assertEqual(PI_VERSION, "1.5.0")
        for digest in pi_agent.RELEASE_SHA256.values():
            self.assertRegex(digest, r"^[0-9a-f]{64}$")

    def test_missing_tarball(self):
        with self.assertRaisesRegex(RuntimeError, "fetch it with"):
            release_tarball("linux-x64", self.cache)

    def test_refuses_a_hash_mismatch(self):
        fake_release(self.cache, {"pi/pi": b"#!/bin/sh\n"})
        with self.assertRaisesRegex(RuntimeError, "not the pinned"):
            release_tarball("linux-x64", self.cache)

    def test_unknown_platform(self):
        with self.assertRaisesRegex(RuntimeError, "no pinned"):
            release_tarball("linux-riscv64", self.cache)

    def test_accepts_the_pinned_hash_and_extracts(self):
        path = fake_release(self.cache, {"pi/pi": b"#!/bin/sh\n", "pi/docs/a.md": b"doc"})
        with mock.patch.dict(pi_agent.RELEASE_SHA256, {"linux-x64": pi_agent.sha256_file(path)}):
            self.assertEqual(release_tarball("linux-x64", self.cache), path)
        bundle = extract_bundle(path, self.cache / "x")
        self.assertTrue((bundle / "pi").is_file())
        self.assertTrue((bundle / "docs" / "a.md").is_file())

    def test_extract_refuses_a_bundle_without_pi(self):
        path = fake_release(self.cache, {"other/pi": b""})
        with self.assertRaisesRegex(RuntimeError, "no pi/pi"):
            extract_bundle(path, self.cache / "x")

    def test_extract_refuses_paths_outside(self):
        path = fake_release(self.cache, {"../escape": b"", "pi/pi": b""})
        with self.assertRaises(tarfile.OutsideDestinationError):
            extract_bundle(path, self.cache / "x")


class FakeEnvironment:
    """Records what an agent does to a container."""

    def __init__(self, machine="x86_64", version=PI_VERSION, default_user=None):
        self.machine, self.version, self.default_user = machine, version, default_user
        self.commands: list[tuple[str, str | None, dict | None]] = []
        self.files: dict[str, str] = {}
        self.dirs: dict[str, list[str]] = {}

    async def exec(self, command, user=None, env=None, cwd=None, timeout_sec=None):
        self.commands.append((command, user, env))
        stdout = ""
        if "uname -m" in command:
            stdout = self.machine + "\n"
        elif command.endswith("--version"):
            stdout = self.version + "\n"
        return mock.Mock(return_code=0, stdout=stdout, stderr="")

    async def upload_file(self, source, target):
        self.files[target] = Path(source).read_text()

    async def upload_dir(self, source, target):
        self.dirs[target] = sorted(str(p.relative_to(source)) for p in Path(source).rglob("*"))


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.cache = Path(tempfile.mkdtemp())
        tarball = fake_release(self.cache, {"pi/pi": b"#!/bin/sh\n", "pi/docs/a.md": b"doc"})
        self.patches = [
            mock.patch.object(pi_agent, "CACHE_DIR", self.cache),
            mock.patch.dict(pi_agent.RELEASE_SHA256, {"linux-x64": pi_agent.sha256_file(tarball)}),
        ]
        for p in self.patches:
            p.start()
        self.addCleanup(lambda: [p.stop() for p in self.patches])

    def install(self, environment):
        asyncio.run(PiAgent(logs_dir=Path(tempfile.mkdtemp())).install(environment))

    def test_uploads_the_bundle_and_checks_its_version(self):
        environment = FakeEnvironment(default_user="agent")
        self.install(environment)
        self.assertEqual(environment.dirs, {"/installed-agent/pi/pi-1.5.0": ["docs", "docs/a.md", "pi"]})
        commands = [c for c, _, _ in environment.commands]
        self.assertIn("chown agent /installed-agent/pi/config", "\n".join(commands))
        version_env = next(env for c, _, env in environment.commands if c.endswith("--version"))
        self.assertEqual(version_env, PI_ENV)
        # Nothing comes from the image's package managers or the network.
        for word in ("apt", "apk", "yum", "npm", "curl", "wget", "nvm"):
            self.assertNotIn(word, " ".join(commands).split())

    def test_refuses_a_wrong_version(self):
        with self.assertRaisesRegex(RuntimeError, "not 1.5.0"):
            self.install(FakeEnvironment(version="1.5.1"))

    def test_refuses_an_unknown_architecture(self):
        with self.assertRaisesRegex(RuntimeError, "no pi build"):
            self.install(FakeEnvironment(machine="riscv64"))


class RunTest(unittest.TestCase):
    def run_agent(self):
        environment = FakeEnvironment()
        context = AgentContext()
        with mock.patch.dict(os.environ, {**BASELINE_ENV, **SECRET_ENV}):
            agent = PiAgent(logs_dir=Path(tempfile.mkdtemp()))
            asyncio.run(agent.run("Fix the bug in /app.", environment, context))
        return environment, context

    def test_runs_pi_with_the_models_json_and_no_key(self):
        environment, context = self.run_agent()
        config = json.loads(environment.files["/installed-agent/pi/config/models.json"])
        self.assertEqual(config, models_config(endpoint_settings(BASELINE_ENV)))
        command, _, env = environment.commands[-1]
        self.assertIn("--print --mode json", command)
        self.assertEqual(env, {**PI_ENV, "PI_INSTRUCTION": "Fix the bug in /app."})
        everything = json.dumps([environment.files, environment.commands])
        self.assertNotIn(SECRET, everything)
        for name in SECRET_ENV:
            self.assertNotIn(name, everything)
        # Left empty, so Harbor fills it from the synced logs.
        self.assertTrue(context.is_empty())


class UsageTest(unittest.TestCase):
    """Usage from a session file recorded by pi 1.5.0 against a stub server
    (pi_plumbing.py), with its paths replaced."""

    def setUp(self):
        self.logs = Path(tempfile.mkdtemp())
        self.sessions = self.logs / "pi" / "sessions"
        self.sessions.mkdir(parents=True)

    def populate(self):
        context = AgentContext()
        PiAgent(logs_dir=self.logs).populate_context_post_run(context)
        return context

    def test_totals_the_recorded_session(self):
        shutil.copy(TESTDATA / "pi-session.jsonl", self.sessions / "s.jsonl")
        (self.logs / "pi.txt").write_text('{"type":"agent_end"}\npi_exit=0\n')
        context = self.populate()
        # Turn 1: 1200 prompt, 0 cached, 30 out; turn 2: 1300 prompt, 1100 cached, 12 out.
        self.assertEqual((context.n_input_tokens, context.n_cache_tokens, context.n_output_tokens), (2500, 1100, 42))
        self.assertEqual(context.metadata["usage_source"], "session")
        self.assertEqual(context.metadata["pi_exit"], "0")
        self.assertEqual(context.metadata["pi_version"], PI_VERSION)
        self.assertTrue((self.logs / "trajectory.json").is_file())

    def test_a_killed_run_keeps_what_was_written(self):
        # Killed during the second reply: the session file ends after the
        # tool result, maybe with a torn line, and pi.txt has no exit line.
        lines = (TESTDATA / "pi-session.jsonl").read_text().splitlines()
        (self.sessions / "s.jsonl").write_text("\n".join(lines[:-1]) + '\n{"type":"mess')
        (self.logs / "pi.txt").write_text('{"type":"turn_start"}\n')
        context = self.populate()
        self.assertEqual((context.n_input_tokens, context.n_cache_tokens, context.n_output_tokens), (1200, 0, 30))
        self.assertIsNone(context.metadata["pi_exit"])

    def test_falls_back_to_the_output_before_a_session_file(self):
        end = {"type": "message_end", "message": {"role": "assistant", "usage": {"input": 100, "output": 5, "cacheRead": 50, "cacheWrite": 0}}}
        (self.logs / "pi.txt").write_text(json.dumps(end) + "\n")
        context = self.populate()
        self.assertEqual(context.metadata["usage_source"], "output")
        self.assertEqual(context.n_output_tokens, 5)


if __name__ == "__main__":
    unittest.main()
