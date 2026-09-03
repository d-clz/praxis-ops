#!/usr/bin/env bash
# sabotage-b: ignores the hour window. MUST trip `answer_correct`.
set -euo pipefail
awk '$9 >= 500 && $9 < 600 { seen[$1] } END { print length(seen) }' \
  /var/log/edge/access.log > /home/candidate/answer.txt
