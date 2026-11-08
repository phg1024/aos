#!/usr/bin/env bash
# Dry-runs all twelve trials of the comparison with the scripted model, in
# a fresh root, then the analyzer over them, and checks what came out. It
# spends nothing and reads no key.
#
#   bench/inference/compare/test_run.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
unset ANTHROPIC_API_KEY AOS_LLM_API_KEY AOS_VLLM_API_KEY
export AOS_COMPARE_ROOT=$root
for arm in strata sonnet; do
  for set in good contradictory; do
    for trial in 1 2 3; do
      "$here/run.sh" "$arm" "$set" "$trial" --dry-run 2>/dev/null
    done
  done
done
if "$here/run.sh" strata good 1 --dry-run 2>/dev/null; then
  echo "FAIL: a trial ran twice"; exit 1
fi
"$root/dry-run/bin/compare" -root "$root/dry-run" -html "$root/compare.html" -json "$root/compare.json"
python3 -I - "$root/compare.json" <<'PY'
import json, sys
summary = json.load(open(sys.argv[1]))
groups = summary["groups"]
assert len(groups) == 4 and len(summary["trials"]) == 12, (len(groups), len(summary["trials"]))
for g in groups:
    name = f'{g["arm"]}/{g["set"]}'
    assert g["dry_run"] and g["passes"] == 3 and g["all_trials_passed"], name
    assert g["html_intact"] == 3 and g["misreads"] == 0 and g["repairs"] == 0, name
    assert g["steps_per_agent"] == {"delivery_lead": 15, "merger": 15, "file_writer": 9, "review_partner": 9}, (name, g["steps_per_agent"])
    assert g["stages"].get("trailing object / nested") == 6, (name, g["stages"])
    if g["arm"] == "sonnet":
        assert g["usd"] > 0 and g["reasoning_chars"] > 0, name
    else:
        assert g["usd"] == 0 and g["reasoning_chars"] == 0, name
print("ok: 12 dry-run trials, all passed, HTML intact, report written")
PY
