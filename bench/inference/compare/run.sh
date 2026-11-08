#!/usr/bin/env bash
# One trial of the Chapter 9 comparison: workflows/live-reviewed-merge.json
# on one input set, through one arm's model profile.
#
#   run.sh ARM SET TRIAL [--dry-run]
#
#   ARM    strata | sonnet
#   SET    good | contradictory   (bench/inference/compare/fixtures/SET)
#   TRIAL  1, 2 or 3
#
# Each trial gets its own directory, which must not exist yet:
#   $AOS_COMPARE_ROOT/ARM/SET/trial-TRIAL      ($AOS_COMPARE_ROOT/dry-run/... with --dry-run)
# holding the state directory, output.html, report.html, run.log (aos's JSON
# result), stderr.log and meta.json. AOS_COMPARE_ROOT defaults to
# ~/aos-compare-runs, outside the repository; analyze reads it.
#
# The arms differ only in their profile, from $AOS_MODELS_FILE (default
# scripts/models.json): $AOS_COMPARE_STRATA_PROFILE (default strata-phg) and
# $AOS_COMPARE_SONNET_PROFILE (default claude). The profile names its base
# URL, model, sampling and the variable its key is in; this script copies the
# key from that variable into the one aos reads and never prints it. Every
# trial runs with --token-budget 250000 --time-budget 15m --strict-time-budget,
# and the workflow requires review_partner's verdict.
#
# The Sonnet arm has a hard cap: before a trial the script sums what the
# arm's earlier trials cost, from their call ledgers, and refuses to start
# when that plus one trial's worst case would pass $5 (AOS_COMPARE_CAP_USD).
# It prints the arm's spend after every trial.
#
# --dry-run answers every call from fake_model.py on a local port, in the
# arm's own wire format, with no key: it spends nothing and checks the whole
# path, workflow included.
set -euo pipefail

usage() { echo "usage: run.sh strata|sonnet good|contradictory 1|2|3 [--dry-run]" >&2; exit 2; }
[[ $# -ge 3 ]] || usage
arm=$1 set=$2 trial=$3 dry=0
[[ ${4:-} == --dry-run ]] && dry=1
[[ $# -le 4 && ( -z ${4:-} || $dry == 1 ) ]] || usage
case $arm in strata | sonnet) ;; *) usage ;; esac
case $set in good | contradictory) ;; *) usage ;; esac
case $trial in 1 | 2 | 3) ;; *) usage ;; esac

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../.." && pwd)
root=${AOS_COMPARE_ROOT:-$HOME/aos-compare-runs}
[[ $dry == 1 ]] && root=$root/dry-run
models=${AOS_MODELS_FILE:-$repo/scripts/models.json}
token_budget=250000
time_budget=15m
cap=${AOS_COMPARE_CAP_USD:-5}
case $arm in
  strata) profile=${AOS_COMPARE_STRATA_PROFILE:-strata-phg} ;;
  sonnet) profile=${AOS_COMPARE_SONNET_PROFILE:-claude} ;;
esac

dir=$root/$arm/$set/trial-$trial
if [[ -e $dir ]]; then
  echo "run.sh: $dir exists; a trial runs once, on a fresh state directory" >&2
  exit 3
fi

# The binaries, built once per root from this checkout.
bin=$root/bin
if [[ ! -x $bin/aos || ! -x $bin/compare ]]; then
  mkdir -p "$bin"
  (cd "$repo" && go build -buildvcs=false -o "$bin/aos" ./cmd/aos && go build -buildvcs=false -o "$bin/compare" ./bench/inference/compare)
fi

# The profile's settings, without its key: one NAME=VALUE per line.
settings=$(python3 -I - "$models" "$profile" <<'PY'
import json, sys
path, name = sys.argv[1], sys.argv[2]
profiles = {p["name"]: p for p in json.load(open(path))["profiles"]}
if name not in profiles:
    sys.exit(f"run.sh: no profile {name!r} in {path}")
p = profiles[name]
backend = p.get("backend", "vllm")
base = p.get("base_url", "")
if backend == "anthropic" and not base:
    base = "https://api.anthropic.com"
s = p.get("sampling", {})
print("backend=" + backend)
print("base_url=" + base)
print("model=" + p["model"])
print("key_env=" + p.get("api_key_env", ""))
print("context_window=" + str(p.get("context_window", "")))
print("timeout=" + str(p.get("timeout_seconds", "")))
print("temperature=" + str(s.get("temperature", "")))
print("top_p=" + str(s.get("top_p", "")))
print("max_tokens=" + str(s.get("max_tokens", "")))
print("thinking_budget=" + str(s.get("thinking_budget", "")))
PY
)
# cfg_NAME variables: macOS ships bash 3.2, which has no associative arrays.
while IFS='=' read -r key value; do printf -v "cfg_$key" '%s' "$value"; done <<<"$settings"

if [[ $arm == sonnet && $dry == 0 ]]; then
  max_tokens=${cfg_max_tokens:-16000}
  spent=$("$bin/compare" -spend "$root/$arm")
  worst=$("$bin/compare" -worst-case "$token_budget" -max-tokens "$max_tokens")
  if ! python3 -I -c "import sys; sys.exit(0 if float(sys.argv[1]) + float(sys.argv[2]) <= float(sys.argv[3]) else 1)" "$spent" "$worst" "$cap"; then
    echo "run.sh: refused: the sonnet arm has spent \$$spent, and one more trial could cost up to \$$worst, past the \$$cap cap" >&2
    exit 4
  fi
  echo "run.sh: sonnet arm spent so far \$$spent; this trial at most \$$worst; cap \$$cap" >&2
fi

mkdir -p "$dir"
inputs=()
for f in "$here/fixtures/$set"/0*.txt; do inputs+=(--input "$f"); done

# Only what the profile says reaches aos: clear what the shell had.
for name in $(compgen -e | grep -E '^AOS_(LLM|VLLM)_' || true); do unset "$name"; done
export AOS_LLM_BACKEND=${cfg_backend} AOS_LLM_MODEL=${cfg_model}
[[ -n ${cfg_context_window} ]] && export AOS_LLM_CONTEXT_WINDOW=${cfg_context_window}
[[ -n ${cfg_timeout} ]] && export AOS_LLM_TIMEOUT_SECONDS=${cfg_timeout}
[[ -n ${cfg_temperature} ]] && export AOS_LLM_TEMPERATURE=${cfg_temperature}
[[ -n ${cfg_top_p} ]] && export AOS_LLM_TOP_P=${cfg_top_p}
[[ -n ${cfg_max_tokens} ]] && export AOS_LLM_MAX_TOKENS=${cfg_max_tokens}
[[ -n ${cfg_thinking_budget} ]] && export AOS_LLM_THINKING_BUDGET=${cfg_thinking_budget}

fake=
cleanup() { [[ -n $fake ]] && kill "$fake" 2>/dev/null || true; }
trap cleanup EXIT
if [[ $dry == 1 ]]; then
  port_file=$dir/fake-port
  python3 -I "$here/fake_model.py" "$port_file" &
  fake=$!
  for _ in $(seq 50); do [[ -s $port_file ]] && break; sleep 0.1; done
  export AOS_LLM_BASE_URL=http://127.0.0.1:$(cat "$port_file")
  rm -f "$port_file"
  unset ANTHROPIC_API_KEY
else
  export AOS_LLM_BASE_URL=${cfg_base_url}
  key_env=${cfg_key_env}
  if [[ -n $key_env ]]; then
    if [[ -z ${!key_env:-} ]]; then
      echo "run.sh: profile $profile reads its key from $key_env, which is not set" >&2
      rmdir "$dir"
      exit 5
    fi
    if [[ ${cfg_backend} == anthropic ]]; then
      export ANTHROPIC_API_KEY=${!key_env}
    else
      export AOS_LLM_API_KEY=${!key_env}
    fi
  fi
fi

started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
set +e
"$bin/aos" run --workflow "$repo/workflows/live-reviewed-merge.json" "${inputs[@]}" \
  --output "$dir/output.html" --report "$dir/report.html" --state-dir "$dir/state" \
  --token-budget "$token_budget" --time-budget "$time_budget" --strict-time-budget \
  "Merge the input fragments into one HTML note, write it to the output path, and have it reviewed." \
  >"$dir/run.log" 2>"$dir/stderr.log"
code=$?
set -e
ended=$(date -u +%Y-%m-%dT%H:%M:%SZ)

python3 -I - "$dir/meta.json" "$arm" "$set" "$trial" "$dry" "$profile" "${cfg_backend}" "${cfg_model}" \
  "$token_budget" "$time_budget" "$started" "$ended" "$code" "$(git -C "$repo" rev-parse --short HEAD)" <<'PY'
import json, sys
(path, arm, set_, trial, dry, profile, backend, model, budget, time_budget, started, ended, code, rev) = sys.argv[1:]
json.dump({"arm": arm, "set": set_, "trial": int(trial), "dry_run": dry == "1", "profile": profile, "backend": backend,
           "model": model, "token_budget": int(budget), "time_budget": time_budget, "started": started, "ended": ended,
           "exit_code": int(code), "rev": rev}, open(path, "w"), indent=2)
PY

echo "run.sh: $arm/$set/trial-$trial exited $code ($started to $ended)" >&2
if [[ $arm == sonnet ]]; then
  echo "run.sh: sonnet arm spend now \$$("$bin/compare" -spend "$root/$arm")" >&2
fi
exit 0
