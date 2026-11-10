#!/usr/bin/env bash
# Starts relay.py on 127.0.0.1:18080 for the pi arm (PLAN-pi.md), logging to relay-pi.log. The key is
# read from its config file into the relay's environment here, and is never
# printed or put on a command line. The upstream comes from the first
# argument, as host:port.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STRATA_KEY="$(python3 -I -c 'import os, re
text = open(os.path.expanduser("~/.hermes/config.yaml")).read()
print(re.search(r"api_key:\s*[\x27\"]?([^\s\x27\"]+)", text[text.index("strata-phg-gpu"):]).group(1))')"
export STRATA_KEY
exec python3 -I "$HERE/relay.py" --listen 18080 --upstream "${1:?upstream host:port}" --host-header phg-gpu.local:8080 --log "$HERE/relay-pi.log"
