"""Harbor installed-agent adapter that runs the pi coding agent in a task container.

Pi is the Terminal-Bench baseline's third arm: an agent that, like AOS, runs
inside the container with read, write, edit and bash tools, but has no
runtime contracts. It runs with its own default tools, system prompt and
context handling, and with no time budget: Harbor's timeout ends it, as with
Terminus-2.

Install uploads a pinned pi release (a standalone Bun build, PI_VERSION) into
the container, so nothing depends on the task image's Node, package manager
or network. The release tarball is fetched once into the cache with
`python3 pi_agent.py fetch` and checked against RELEASE_SHA256 on every
install.

Run writes a models.json that points pi at the same OpenAI-compatible
endpoint and sampling as the AOS arm (the AOS_LLM_* variables of the host
process, read as aos_agent.py reads them), then runs `pi --print --mode json`
in the task's working directory. No key is placed anywhere: the endpoint is
the host relay, which adds it; models.json carries the placeholder "relay".

Usage and the trajectory come from pi's session file, which pi appends to as
it runs, so they survive a Harbor kill. Harbor's own pi agent parses it:
this class reuses that parsing and replaces only install and run.

Use with:
    harbor run --agent-import-path pi_agent:PiAgent ...
with this directory on PYTHONPATH.
"""

from __future__ import annotations

import hashlib
import json
import os
import shlex
import sys
import tarfile
import tempfile
import urllib.request
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

from harbor.agents.installed.pi import Pi
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

from aos_agent import FORWARDED_ENV, container_base_url

REPO_ROOT = Path(__file__).resolve().parents[2]

PI_VERSION = "1.5.0"
RELEASE_URL = "https://github.com/earendil-works/pi/releases/download/v{version}/pi-{platform}.tar.gz"
# From the release's SHA256SUMS. Only the Linux builds go into containers;
# darwin-arm64 is for the local plumbing test.
RELEASE_SHA256 = {
    "linux-x64": "df870ba0e344cb7b3358d8d8f19397b8b8d225a8669784736ad68a5c2169d53b",
    "linux-arm64": "5d5711b48600756473682e86360b026b11abf7591e5d112eb411d4da2c100d9a",
    "darwin-arm64": "a9bf0d82d58d2a7d337f4ae4171b5fc9ebffe42509527a831364c4b65adf2cdf",
}
CACHE_DIR = Path(os.environ.get("PI_HARBOR_CACHE", REPO_ROOT / ".cache" / f"pi-v{PI_VERSION}"))

PLATFORM_BY_MACHINE = {"x86_64": "linux-x64", "amd64": "linux-x64", "aarch64": "linux-arm64", "arm64": "linux-arm64"}

REMOTE_DIR = "/installed-agent/pi"
REMOTE_BUNDLE = f"{REMOTE_DIR}/pi-{PI_VERSION}"
REMOTE_BIN = f"{REMOTE_BUNDLE}/pi"
# PI_CODING_AGENT_DIR: models.json, and whatever pi stores for itself.
REMOTE_CONFIG_DIR = f"{REMOTE_DIR}/config"

PROVIDER = "aos-relay"
# The relay replaces the Authorization header with its own key; pi only needs
# some value to treat the provider as usable.
PLACEHOLDER_API_KEY = "relay"

# Pi makes no network call but the model's: no catalog refresh, no version
# check, no telemetry.
PI_ENV = {
    "PI_CODING_AGENT_DIR": REMOTE_CONFIG_DIR,
    "PI_OFFLINE": "1",
    "PI_SKIP_VERSION_CHECK": "1",
    "PI_TELEMETRY": "0",
}

# The AOS arm's endpoint and sampling variables that pi uses. AOS_LLM_API_KEY
# is never read.
ENDPOINT_KEYS = (
    "AOS_LLM_BASE_URL",
    "AOS_LLM_MODEL",
    "AOS_LLM_CONTEXT_WINDOW",
    "AOS_LLM_TEMPERATURE",
    "AOS_LLM_TOP_P",
    "AOS_LLM_MAX_TOKENS",
    "AOS_LLM_THINKING_BUDGET",
)


def endpoint_settings(environ=os.environ) -> dict[str, str]:
    """The endpoint and sampling settings, with aos_agent.py's defaults."""
    return {key: environ.get(key) or FORWARDED_ENV[key] for key in ENDPOINT_KEYS}


def openai_base_url(url: str) -> str:
    """The container's view of the endpoint, ending in /v1 as pi expects.

    AOS takes the server root and adds /v1 itself; pi's OpenAI client appends
    only /chat/completions.
    """
    parts = urlsplit(container_base_url(url))
    path = parts.path.rstrip("/")
    if not path.endswith("/v1"):
        path += "/v1"
    return urlunsplit((parts.scheme, parts.netloc, path, parts.query, parts.fragment))


def models_config(settings: dict[str, str]) -> dict:
    """Pi's models.json: one provider, one model, the AOS arm's sampling.

    - samplingParams is merged last into every request body, unfiltered, so
      temperature, top_p and reasoning_budget_tokens go out as they do from
      the other arms.
    - maxTokens with maxTokensField max_tokens sends max_tokens rather than
      max_completion_tokens. Pi lowers it near the end of the window.
    - reasoning false: pi sends no reasoning_effort and keeps the system
      prompt in the system role. It still reads reasoning_content.
    - supportsStore false: no store field.
    Pi's compaction and retry settings stay at their defaults.
    """
    return {
        "providers": {
            PROVIDER: {
                "baseUrl": openai_base_url(settings["AOS_LLM_BASE_URL"]),
                "api": "openai-completions",
                "apiKey": PLACEHOLDER_API_KEY,
                "models": [
                    {
                        "id": settings["AOS_LLM_MODEL"],
                        "reasoning": False,
                        "input": ["text"],
                        "contextWindow": int(settings["AOS_LLM_CONTEXT_WINDOW"]),
                        "maxTokens": int(settings["AOS_LLM_MAX_TOKENS"]),
                        "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
                        "compat": {"maxTokensField": "max_tokens", "supportsStore": False},
                        "samplingParams": {
                            "temperature": float(settings["AOS_LLM_TEMPERATURE"]),
                            "top_p": float(settings["AOS_LLM_TOP_P"]),
                            "reasoning_budget_tokens": int(settings["AOS_LLM_THINKING_BUDGET"]),
                        },
                    }
                ],
            }
        }
    }


def pi_command(model: str, session_dir: str, output_path: str, binary: str = REMOTE_BIN) -> str:
    """The shell command that runs pi on $PI_INSTRUCTION.

    Pi's exit status is recorded in its output instead of failing the trial:
    the hidden tests decide the outcome. message_update events (streaming
    deltas) are dropped from the output, as Harbor's pi agent does; the
    session file has every finished message.
    """
    return (
        f"mkdir -p {shlex.quote(session_dir)} && "
        f"{{ {shlex.quote(binary)} --print --mode json --no-approve "
        f"--session-dir {shlex.quote(session_dir)} "
        f"--provider {PROVIDER} --model {shlex.quote(model)} "
        f'"$PI_INSTRUCTION" </dev/null; echo "pi_exit=$?"; }} 2>&1 '
        f"| {{ grep --line-buffered -v '\"type\":\"message_update\"' || true; }} "
        f"> {shlex.quote(output_path)}"
    )


def pi_exit_from_output(path: Path) -> str | None:
    """The exit status pi_command recorded, or None if pi was killed first."""
    try:
        lines = path.read_text(errors="replace").splitlines()
    except OSError:
        return None
    for line in reversed(lines):
        if line.startswith("pi_exit="):
            return line.removeprefix("pi_exit=").strip()
    return None


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def release_tarball(platform: str, cache_dir: Path | None = None) -> Path:
    """The cached release tarball for a platform, checked against its pinned hash."""
    cache_dir = cache_dir or CACHE_DIR
    if platform not in RELEASE_SHA256:
        raise RuntimeError(f"no pinned pi {PI_VERSION} build for {platform!r}")
    path = cache_dir / "download" / f"pi-{platform}.tar.gz"
    if not path.is_file():
        raise RuntimeError(f"missing {path}; fetch it with: python3 bench/harbor/pi_agent.py fetch {platform}")
    got = sha256_file(path)
    if got != RELEASE_SHA256[platform]:
        raise RuntimeError(f"{path} has sha256 {got}, not the pinned {RELEASE_SHA256[platform]}")
    return path


def extract_bundle(tarball: Path, into: Path) -> Path:
    """Unpack a release tarball; returns the bundle directory (pi/)."""
    with tarfile.open(tarball) as archive:
        archive.extractall(into, filter="data")
    bundle = into / "pi"
    if not (bundle / "pi").is_file():
        raise RuntimeError(f"{tarball} has no pi/pi")
    return bundle


def fetch(platforms: list[str], cache_dir: Path | None = None) -> None:
    """Download release tarballs into the cache, keeping only pinned-hash matches."""
    download = (cache_dir or CACHE_DIR) / "download"
    download.mkdir(parents=True, exist_ok=True)
    for platform in platforms:
        if platform not in RELEASE_SHA256:
            raise SystemExit(f"no pinned pi {PI_VERSION} build for {platform!r}")
        target = download / f"pi-{platform}.tar.gz"
        if target.is_file() and sha256_file(target) == RELEASE_SHA256[platform]:
            print(f"{target}: OK (cached)")
            continue
        partial = target.with_suffix(".part")
        url = RELEASE_URL.format(version=PI_VERSION, platform=platform)
        with urllib.request.urlopen(url, timeout=600) as response, partial.open("wb") as out:
            while block := response.read(1 << 20):
                out.write(block)
        got = sha256_file(partial)
        if got != RELEASE_SHA256[platform]:
            partial.unlink()
            raise SystemExit(f"{url} has sha256 {got}, not the pinned {RELEASE_SHA256[platform]}")
        partial.replace(target)
        print(f"{target}: OK")


class PiAgent(Pi):
    """Harbor's pi agent with a pinned, uploaded build and the baseline's endpoint."""

    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self._version = PI_VERSION

    def get_version_command(self) -> str | None:
        return None

    async def install(self, environment: BaseEnvironment) -> None:
        machine = (await self.exec_as_root(environment, "uname -m")).stdout.strip()
        platform = PLATFORM_BY_MACHINE.get(machine)
        if platform is None:
            raise RuntimeError(f"no pi build for container architecture {machine!r}")
        tarball = release_tarball(platform)
        with tempfile.TemporaryDirectory(prefix="pi-bundle-") as tmp:
            bundle = extract_bundle(tarball, Path(tmp))
            await self.exec_as_root(environment, f"mkdir -p {REMOTE_DIR}")
            await environment.upload_dir(bundle, REMOTE_BUNDLE)
        await self.exec_as_root(environment, f"chmod -R a+rX {REMOTE_BUNDLE} && chmod 755 {REMOTE_DIR} {REMOTE_BIN}")
        # The config directory belongs to the agent user, who runs pi.
        await self.exec_as_root(environment, f"mkdir -p {REMOTE_CONFIG_DIR} && chmod 700 {REMOTE_CONFIG_DIR}")
        if environment.default_user is not None:
            owner = shlex.quote(str(environment.default_user))
            await self.exec_as_root(environment, f"chown {owner} {REMOTE_CONFIG_DIR}")
        version = (await self.exec_as_agent(environment, f"{REMOTE_BIN} --version", env=PI_ENV)).stdout.strip()
        if version.splitlines()[-1:] != [PI_VERSION]:
            raise RuntimeError(f"{REMOTE_BIN} reports version {version!r}, not {PI_VERSION}")

    async def run(self, instruction: str, environment: BaseEnvironment, context: AgentContext) -> None:
        settings = endpoint_settings()
        await self._upload_config_text(
            environment,
            content=json.dumps(models_config(settings), indent=2) + "\n",
            remote_path=f"{REMOTE_CONFIG_DIR}/models.json",
            filename="models.json",
        )
        session_dir = str(self.environment_logs_dir / self._SESSIONS_DIRECTORY)
        output_path = str(self.environment_logs_dir / self._OUTPUT_FILENAME)
        # The instruction travels in the environment to avoid shell quoting.
        env = {**PI_ENV, "PI_INSTRUCTION": self.render_instruction(instruction)}
        # The context is left empty: Harbor then fills it from the synced
        # logs in populate_context_post_run, after a timeout too.
        await self.exec_as_agent(
            environment, command=pi_command(settings["AOS_LLM_MODEL"], session_dir, output_path), env=env
        )

    def populate_context_post_run(self, context: AgentContext) -> None:
        super().populate_context_post_run(context)
        settings = endpoint_settings()
        session_dir = self.logs_dir / self._SESSIONS_DIRECTORY
        sessions = sorted(p.name for p in session_dir.glob("*.jsonl")) if session_dir.is_dir() else []
        # Harbor reads the session file, else the message_end events in pi.txt
        # (a run killed before pi's first reply has no session file).
        source = None
        if context.n_input_tokens is not None:
            source = "session" if sessions else "output"
        context.metadata = {
            **(context.metadata or {}),
            "pi_version": PI_VERSION,
            "model": settings["AOS_LLM_MODEL"],
            "pi_exit": pi_exit_from_output(self.logs_dir / self._OUTPUT_FILENAME),
            "session_files": sessions,
            "usage_source": source,
        }


if __name__ == "__main__":
    if len(sys.argv) < 2 or sys.argv[1] != "fetch":
        raise SystemExit("usage: pi_agent.py fetch [platform ...]  (default: linux-x64)")
    fetch(sys.argv[2:] or ["linux-x64"])
