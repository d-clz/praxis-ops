#!/usr/bin/env bash
# fix-a: single-pass awk. Field 1 is the client, field 4 the timestamp, field 9 status.
set -euo pipefail
awk '$4 ~ /:03:/ && $9 >= 500 && $9 < 600 { seen[$1] } END { print length(seen) }' \
  /var/log/edge/access.log > /home/candidate/answer.txt
