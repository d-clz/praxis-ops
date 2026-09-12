#!/usr/bin/env bash
# Build-time seeding for CPT-01. Three coupled Targets plus two decoys.
set -euo pipefail

mkdir -p /var/www/praxis /opt/praxis

cat > /var/www/praxis/index.html <<'HTML'
<!doctype html>
<html><head><title>Praxis Edge</title></head>
<body><h1>Praxis Edge Node</h1><p>build 2026.04.11-a7f3</p></body></html>
HTML

sha256sum /var/www/praxis/index.html | awk '{print $1}' > /opt/praxis/.docroot-sha256
chmod 0400 /opt/praxis/.docroot-sha256

cat > /etc/nginx/sites-available/praxis <<'CONF'
server {
    listen 80 default_server;
    server_name _;
    root /var/www/praxis;
    index index.html;

    access_log /var/log/nginx/praxis.access.log;
    error_log  /var/log/nginx/praxis.error.log;

    location / {
        try_files $uri $uri/ =404;
    }
}
CONF
ln -sf /etc/nginx/sites-available/praxis /etc/nginx/sites-enabled/praxis
rm -f /etc/nginx/sites-enabled/default

# ---- Target 1: invalid directive. nginx -t fails, so the unit never starts. ----
sed -i 's/    index index.html;/    index index.html;\n    sendfile_max_chunk 512k on;/' \
  /etc/nginx/sites-available/praxis

# ---- Target 2: a squatter already holding :80, so a config-only fix still fails.
cat > /etc/systemd/system/ts-metrics.service <<'UNIT'
[Unit]
Description=Telemetry shim (legacy)
After=network.target

[Service]
ExecStart=/usr/bin/python3 -m http.server 80 --directory /var/lib/ts-metrics
Restart=always

[Install]
WantedBy=multi-user.target
UNIT
mkdir -p /var/lib/ts-metrics
echo "ts-metrics shim" > /var/lib/ts-metrics/index.html
systemctl enable ts-metrics.service

# ---- Target 3: docroot unreadable by the nginx worker user. ----
chmod 0700 /var/www/praxis
chown root:root /var/www/praxis

# ---- Decoy 1: a disabled-but-harmless unit that looks suspicious. ----
cat > /etc/systemd/system/praxis-cache.service <<'UNIT'
[Unit]
Description=Praxis cache warmer (disabled)
[Service]
ExecStart=/bin/sleep infinity
[Install]
WantedBy=multi-user.target
UNIT
systemctl disable praxis-cache.service || true

# ---- Decoy 2: a stale, unreferenced vhost with its own (correct) config. ----
cat > /etc/nginx/sites-available/legacy <<'CONF'
server {
    listen 8080;
    root /var/www/legacy;
}
CONF
mkdir -p /var/www/legacy && echo "legacy" > /var/www/legacy/index.html

systemctl disable nginx || true
