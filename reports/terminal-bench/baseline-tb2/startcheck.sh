#!/usr/bin/env bash
# The Rosetta start check (PLAN.md, "Sampling"), applied to the candidates in
# draw.py's seeded order until 16 have passed. For each image: pull it for
# linux/amd64, then run `uname -m` in it. It passes if the run exits 0 and
# prints x86_64. A registry rate limit or network error is not a failure:
# the script waits and tries the same image again. One JSON line per task
# goes to startcheck.jsonl.
#
#   reports/terminal-bench/baseline-tb2/startcheck.sh
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="$HERE/startcheck.jsonl"
touch "$OUT"
image_of() {
  python3 -I -c 'import json,sys
print(next(t["image"] for t in json.load(open(sys.argv[1])) if t["task"] == sys.argv[2]))' "$HERE/survey.json" "$1"
}
passed=$(python3 -I -c 'import json,sys; print(sum(json.loads(l)["passed"] for l in open(sys.argv[1])))' "$OUT")
python3 -I "$HERE/draw.py" order | while read -r task; do
  (( passed >= 16 )) && break
  grep -q "\"task\": \"$task\"" "$OUT" && continue
  image=$(image_of "$task")
  while true; do
    pull=$(docker pull --platform linux/amd64 "$image" 2>&1); rc=$?
    if (( rc != 0 )) && grep -q -i -E "toomanyrequests|429|rate limit|timeout|connection reset|i/o timeout|TLS handshake" <<<"$pull"; then
      echo "$(date '+%H:%M:%S %Z') $task: registry limit or network error, waiting 10 min" >&2
      sleep 600
      continue
    fi
    break
  done
  if (( rc != 0 )); then
    ok=false; detail="pull failed: $(tail -1 <<<"$pull" | tr -d '"\\' | cut -c1-200)"
  else
    run=$(perl -e 'alarm 300; exec @ARGV' docker run --rm --platform linux/amd64 --entrypoint /bin/sh "$image" -c 'uname -m' 2>&1); rc=$?
    if (( rc == 0 )) && [[ "$run" == *x86_64* ]]; then ok=true; detail="uname -m: x86_64"
    else ok=false; detail="run exit $rc: $(tail -1 <<<"$run" | tr -d '"\\' | cut -c1-200)"; fi
  fi
  size=$(docker image inspect --format '{{.Size}}' "$image" 2>/dev/null || echo null)
  echo "{\"task\": \"$task\", \"image\": \"$image\", \"passed\": $ok, \"detail\": \"$detail\", \"image_bytes\": $size, \"checked_at\": \"$(date -u +%FT%TZ)\"}" >> "$OUT"
  echo "$(date '+%H:%M:%S %Z') $task: passed=$ok ($detail)" >&2
  [[ $ok == true ]] && passed=$((passed + 1))
done
