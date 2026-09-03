#!/usr/bin/env bash
# Build-time seeding for SKN-01. Deterministic: fixed PRNG seed, fixed row count.
# Changing anything here changes the expected answer -- re-run the bake pipeline's
# expectation capture and update grading/SKN-01/check.sh.
set -euo pipefail

mkdir -p /var/log/edge /var/log/app /opt/praxis

python3 - <<'PY'
import random, ipaddress

random.seed(20260411)

PATHS = ["/api/v2/orders", "/api/v2/orders/confirm", "/health", "/static/app.js",
         "/api/v2/accounts", "/api/v2/transfer"]
AGENTS = ["Mozilla/5.0", "curl/8.5.0", "python-requests/2.31.0", "Go-http-client/2.0"]

# Distinct client pool. The expected answer is derived from this data at bake
# time, never hand-written into the ticket.
pool = [str(ipaddress.IPv4Address(random.randint(0x0A000001, 0x0AFFFFFE)))
        for _ in range(4200)]

rows = []
for hour in range(24):
    # The 03:00 hour is the incident window: elevated 5xx across a subset of clients.
    burst = (hour == 3)
    for _ in range(34000 if burst else 18000):
        client = random.choice(pool)
        minute, second = random.randint(0, 59), random.randint(0, 59)
        if burst and random.random() < 0.31:
            status = random.choice([500, 502, 503, 504])
        elif random.random() < 0.004:
            status = random.choice([500, 503])
        else:
            status = random.choice([200, 200, 200, 201, 204, 301, 404, 401])
        rows.append(
            f'{client} - - [11/Apr/2026:{hour:02d}:{minute:02d}:{second:02d} +0000] '
            f'"GET {random.choice(PATHS)} HTTP/1.1" {status} '
            f'{random.randint(120, 9000)} "-" "{random.choice(AGENTS)}"'
        )

with open("/var/log/edge/access.log", "w") as fh:
    fh.write("\n".join(rows) + "\n")
PY

# Decoy: a second, smaller log from a different service, same date, similar shape.
# Counting from this one gives a plausible but wrong number.
head -n 4000 /var/log/edge/access.log | sed 's#/api/v2#/internal#' > /var/log/app/service.log

chown root:root /var/log/edge/access.log
chmod 0444 /var/log/edge/access.log

install -o candidate -g candidate -m 0644 /dev/null /home/candidate/answer.txt

# Provision fingerprint. check.sh compares against its own embedded copy; this file
# exists so the hardening suite can prove /opt/praxis was not tampered with.
sha256sum /var/log/edge/access.log | awk '{print $1}' > /opt/praxis/.provision-sha256
chmod 0400 /opt/praxis/.provision-sha256
