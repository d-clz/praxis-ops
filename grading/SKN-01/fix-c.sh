#!/usr/bin/env bash
# fix-c: python stdlib, streaming (never loads the file into memory).
set -euo pipefail
python3 - <<'PY' > /home/candidate/answer.txt
seen = set()
with open("/var/log/edge/access.log") as fh:
    for line in fh:
        parts = line.split()
        if len(parts) < 9:
            continue
        if ":03:" not in parts[3]:
            continue
        if parts[3].split(":")[1] != "03":
            continue
        try:
            status = int(parts[8])
        except ValueError:
            continue
        if 500 <= status < 600:
            seen.add(parts[0])
print(len(seen))
PY
