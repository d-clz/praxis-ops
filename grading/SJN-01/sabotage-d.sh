#!/usr/bin/env bash
# Shotgun: kill everything with the file open, readers included.
# MUST trip no_collateral.
for p in $(fuser /var/log/app/service.log 2>/dev/null); do kill -9 "$p" 2>/dev/null || true; done
