"""Harbor installed-agent adapter that runs AOS inside a task container.

Install copies a static Linux `aos` binary (built by run-tb.sh) into the
container. Run fills the terminal-bench workflow with the container's working
directory and executes `aos run` there, so AOS's tools act on the task
environment directly. The model endpoint comes from the AOS_LLM_* variables of
the host process; a loopback base URL is rewritten to host.docker.internal so
the container can reach a server on the host.

Use with:
    harbor run --agent-import-path aos_agent:AOSAgent ...
with this directory on PYTHONPATH (run-tb.sh does both).
"""

from __future__ import annotations

import json
import os
import shlex
import time
import tomllib
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

from harbor.agents.installed.base import BaseInstalledAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

REPO_ROOT = Path(__file__).resolve().parents[2]
DIST_DIR = Path(os.environ.get("AOS_HARBOR_DIST", REPO_ROOT / ".cache" / "harbor-dist"))
WORKFLOW_TEMPLATE = REPO_ROOT / "workflows" / "terminal-bench.json"

REMOTE_DIR = "/installed-agent/aos"
REMOTE_BIN = f"{REMOTE_DIR}/aos"
REMOTE_WORKFLOW = f"{REMOTE_DIR}/terminal-bench.json"
LOG_DIR = "/logs/agent"

GOARCH_BY_MACHINE = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}

# Forwarded from the host environment, with defaults for the local Strata server.
FORWARDED_ENV = {
    "AOS_LLM_BACKEND": "openai-compatible",
    "AOS_LLM_BASE_URL": "http://127.0.0.1:8080",
    "AOS_LLM_MODEL": "qwen3.8-flash-next-unsloth-ud-q4_k_xl",
    "AOS_LLM_API_KEY": "",
    "AOS_LLM_CONTEXT_WINDOW": "262144",
    "AOS_COMPACT_RATIO": "0.8",
    "AOS_LLM_TIMEOUT_SECONDS": "3600",
    "AOS_LLM_TEMPERATURE": "0.6",
    "AOS_LLM_TOP_P": "0.95",
    "AOS_LLM_MAX_TOKENS": "8192",
    "AOS_LLM_THINKING_BUDGET": "2048",
}

LOOPBACK_HOSTS = {"127.0.0.1", "localhost", "::1"}

# Seconds kept back from Harbor's agent timeout so AOS stops on its own clock
# first; the shortest useful budget handed to AOS.
TIME_BUDGET_MARGIN_SEC = 20
MIN_TIME_BUDGET_SEC = 60


def container_base_url(url: str) -> str:
    """Point a host-loopback URL at the Docker host as seen from a container."""
    parts = urlsplit(url)
    if parts.hostname not in LOOPBACK_HOSTS:
        return url
    netloc = "host.docker.internal" + (f":{parts.port}" if parts.port else "")
    return urlunsplit((parts.scheme, netloc, parts.path, parts.query, parts.fragment))


def agent_timeout_sec(task_toml: Path, job_config: Path) -> float | None:
    """Harbor's agent timeout for a trial: min(base, max) * multiplier.

    Harbor computes this in Trial but only hands it to its oracle agent, so
    it is rebuilt here from the task's task.toml and the job's config.json.
    """
    try:
        base = tomllib.loads(task_toml.read_text())["agent"]["timeout_sec"]
    except (OSError, KeyError, TypeError, tomllib.TOMLDecodeError):
        return None
    if base is None:
        return None
    try:
        job = json.loads(job_config.read_text())
    except (OSError, json.JSONDecodeError):
        job = {}
    agent = (job.get("agents") or [{}])[0]
    base = agent.get("override_timeout_sec") or base
    if agent.get("max_timeout_sec"):
        base = min(base, agent["max_timeout_sec"])
    multiplier = job.get("agent_timeout_multiplier")
    if multiplier is None:
        multiplier = job.get("timeout_multiplier", 1.0)
    return float(base) * float(multiplier)


def parse_run_summary(output: str) -> dict | None:
    """Return the JSON summary `aos run` prints last, if present.

    A failed workflow prints "workflow failed: ..." after the summary, so
    only the JSON object itself is decoded.
    """
    start = output.rfind("\n{")
    start = start + 1 if start >= 0 else output.find("{")
    if start < 0:
        return None
    try:
        summary, _ = json.JSONDecoder().raw_decode(output[start:])
    except json.JSONDecodeError:
        return None
    return summary if isinstance(summary, dict) else None


class AOSAgent(BaseInstalledAgent):
    @staticmethod
    def name() -> str:
        return "aos"

    def version(self) -> str | None:
        return self._version or "dev"

    def get_version_command(self) -> str | None:
        return None

    async def install(self, environment: BaseEnvironment) -> None:
        machine = (await self.exec_as_root(environment, "uname -m")).stdout.strip()
        goarch = GOARCH_BY_MACHINE.get(machine)
        if goarch is None:
            raise RuntimeError(f"no AOS build for container architecture {machine!r}")
        binary = DIST_DIR / f"aos-linux-{goarch}"
        if not binary.is_file():
            raise RuntimeError(f"missing {binary}; build it with bench/harbor/run-tb.sh")
        await self.exec_as_root(environment, f"mkdir -p {REMOTE_DIR}")
        await environment.upload_file(binary, REMOTE_BIN)
        await self.exec_as_root(environment, f"chmod 755 {REMOTE_DIR} {REMOTE_BIN}")

    def _trial_timeout_sec(self, environment: BaseEnvironment) -> float | None:
        try:
            task_toml = Path(environment.environment_dir).parent / "task.toml"
            job_config = Path(environment.trial_paths.trial_dir).parent / "config.json"
        except (AttributeError, TypeError):
            return None
        return agent_timeout_sec(task_toml, job_config)

    async def run(self, instruction: str, environment: BaseEnvironment, context: AgentContext) -> None:
        started = time.monotonic()
        timeout = self._trial_timeout_sec(environment)
        workdir = (await self.exec_as_agent(environment, "pwd")).stdout.strip() or "/"
        workflow = WORKFLOW_TEMPLATE.read_text().replace("__WORKDIR__", workdir)
        await self._upload_config_text(
            environment, content=workflow, remote_path=REMOTE_WORKFLOW, filename="terminal-bench.json"
        )

        env = {key: os.environ.get(key, default) for key, default in FORWARDED_ENV.items()}
        env["AOS_LLM_BASE_URL"] = container_base_url(env["AOS_LLM_BASE_URL"])
        # The instruction travels in the environment to avoid shell quoting.
        env["AOS_INSTRUCTION"] = self.render_instruction(instruction)
        # amd64 task images run under QEMU on Apple Silicon, where Go's
        # signal-based async preemption can corrupt memory and crash the
        # runtime. Turning it off costs nothing for this I/O-bound process.
        env["GODEBUG"] = "asyncpreemptoff=1"

        # Let AOS see Harbor's deadline, less what setup used and a margin.
        budget_flag = ""
        if timeout is not None:
            budget = int(timeout - (time.monotonic() - started) - TIME_BUDGET_MARGIN_SEC)
            if budget >= MIN_TIME_BUDGET_SEC:
                budget_flag = f"--time-budget {budget}s "

        # Record the exit code instead of failing the trial: the hidden tests
        # decide the outcome even when the agent stops early.
        command = (
            f"mkdir -p {LOG_DIR} && cd {shlex.quote(workdir)} && "
            f"{{ {REMOTE_BIN} run --trace {budget_flag}--workflow {REMOTE_WORKFLOW} "
            f'--state-dir {LOG_DIR}/aos-state "$AOS_INSTRUCTION" 2>&1; '
            f'echo "aos_exit=$?"; }} | tee {LOG_DIR}/aos.log'
        )
        result = await self.exec_as_agent(environment, command=command, env=env)

        output = result.stdout or ""
        summary = parse_run_summary(output.rsplit("aos_exit=", 1)[0])
        metadata: dict = {"workdir": workdir, "model": env["AOS_LLM_MODEL"], "agent_timeout_sec": timeout}
        if "aos_exit=" in output:
            metadata["aos_exit"] = output.rsplit("aos_exit=", 1)[1].strip().splitlines()[0]
        if summary:
            metadata["agent_runs"] = summary.get("agent_run_cnt")
            usage = summary.get("usage") or {}
            context.n_input_tokens = usage.get("prompt_tokens")
            context.n_output_tokens = usage.get("completion_tokens")
            context.n_cache_tokens = usage.get("cached_tokens")
            metadata["requests"] = usage.get("requests")
        context.metadata = {**(context.metadata or {}), **metadata}
