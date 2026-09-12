#!/usr/bin/env bash
# Growth "stops" because the file is gone. MUST trip log_exists.
rm -f /var/log/app/service.log
