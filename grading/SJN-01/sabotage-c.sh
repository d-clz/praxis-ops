#!/usr/bin/env bash
# Freeze the file and walk away. Writer still running. MUST trip writer_stopped.
chattr +i /var/log/app/service.log 2>/dev/null || true
