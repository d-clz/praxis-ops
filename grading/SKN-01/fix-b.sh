#!/usr/bin/env bash
# fix-b: pipeline. Different tooling, same result -- proves the check is not bound
# to one command.
set -euo pipefail
grep ':03:[0-9][0-9]:[0-9][0-9] ' /var/log/edge/access.log \
  | grep -E '" 5[0-9]{2} ' \
  | cut -d' ' -f1 \
  | sort -u \
  | wc -l \
  | tr -d ' ' > /home/candidate/answer.txt
