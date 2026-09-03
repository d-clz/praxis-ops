#!/usr/bin/env bash
# sabotage-c: counts requests, not distinct clients. MUST trip `answer_correct`.
set -euo pipefail
awk '$4 ~ /:03:/ && $9 >= 500 && $9 < 600 { n++ } END { print n+0 }' \
  /var/log/edge/access.log > /home/candidate/answer.txt
