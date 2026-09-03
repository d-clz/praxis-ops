BIN := orchestrator
PKG := ./cmd/orchestrator
SBX_USER := praxis-sbx
SBX_HOME := $(shell getent passwd $(SBX_USER) | cut -d: -f6)

.PHONY: build vet test deploy

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

vet:
	go vet ./...

test:
	go test ./...

# Must run as root (sudo make deploy). praxis-sbx has no login shell and its
# home is 0700 -- the same reason every bootstrap/ script uses runuser instead
# of assuming it can log in as that user. `install` un-elevated would fail
# outright; `systemctl --user restart` un-elevated would silently restart
# whichever user ran `make`'s own unit (there isn't one), not praxis-sbx's --
# see docs/session-02-plan.md Phase D and orchestrator/deploy/README.md.
#
# Re-installs the .service file and daemon-reloads on every run, not just the
# binary. That's deliberate: a unit-file fix that isn't part of routine deploy
# is a unit-file fix someone has to remember to sync by hand, which is exactly
# what happened with the DOCKER_HOST slash bug -- three extra manual commands
# to actually apply a one-line fix. Both steps are idempotent, so making them
# routine costs nothing on the runs where nothing changed. What's genuinely
# one-time-only and stays out of this target: creating the env file (don't
# want to regenerate the shared token on every deploy) and the first
# `enable --now` (restart is correct once it's already enabled) -- see
# orchestrator/deploy/README.md.
deploy: build
	@if [ "$$(id -u)" -ne 0 ]; then echo "run as root: sudo make deploy" >&2; exit 1; fi
	install -D -o $(SBX_USER) -g $(SBX_USER) -m 0755 $(BIN) $(SBX_HOME)/bin/$(BIN)
	install -D -o $(SBX_USER) -g $(SBX_USER) -m 0644 deploy/praxis-orchestrator.service \
		$(SBX_HOME)/.config/systemd/user/praxis-orchestrator.service
	runuser -u $(SBX_USER) -- env XDG_RUNTIME_DIR=/run/user/$$(id -u $(SBX_USER)) \
		systemctl --user daemon-reload
	runuser -u $(SBX_USER) -- env XDG_RUNTIME_DIR=/run/user/$$(id -u $(SBX_USER)) \
		systemctl --user restart praxis-orchestrator
	# /usr/local/bin, not ~/.local/bin: sudo's own secure_path deliberately
	# excludes per-user directories (that's sudo refusing to trust a
	# user-writable PATH entry when escalating, not a bug), so a copy that
	# only a regular user's PATH can find works unprivileged and then
	# "command not found" the moment you type sudo in front of it.
	# /usr/local/bin is root-owned and in the default PATH for every account,
	# sudo included -- one install location that both invocations agree on.
	install -o root -g root -m 0755 deploy/pxoctl.sh /usr/local/bin/pxoctl
