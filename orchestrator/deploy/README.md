# Deploying the orchestrator

## Account model

Two accounts, not three. There is no separate "orchestrator" identity.

- **`praxis`** -- the human operator. Has `sudo`. Runs `bootstrap/*.sh`, edits
  this repo, deploys. Never touches a running sandbox or the orchestrator
  process directly.
- **`praxis-sbx`** -- runs *both* the orchestrator daemon and, via its own
  rootless podman, every sandbox container it spawns. One account, two roles,
  because the orchestrator's entire job is "talk to this local podman
  socket," and that socket (`/run/user/<uid>/podman/podman.sock`) lives under
  a `0700` `XDG_RUNTIME_DIR` that only `praxis-sbx` can reach -- there is
  nothing to bridge across a privilege boundary that would just need
  bridging back. `praxis-sbx` has no login shell (`nologin`) and its home is
  `0700`, so every interaction with it -- deploying, restarting, inspecting --
  goes through `sudo`/`runuser` from `praxis`, never a direct login. This is
  the same pattern `bootstrap/50-verify.sh` and `60-build-base.sh` use
  throughout (`as_sbx()`), not something specific to the orchestrator.

The security boundary that matters is `praxis` vs. `praxis-sbx`, not
orchestrator vs. sandbox. A bug reachable through the orchestrator's HTTP API
(`/exec`, `/shell`) is already confined to exactly what `praxis-sbx` can do --
spawn, exec into, and destroy sandboxes -- because that is the account it runs
as. It was never holding anything more, and running it as `praxis` instead
would only make a compromise worse for no capability gained (`praxis` owns
the repo and, on the host, everything else "praxis" can reach -- see
`docs/session-01-hardening.md`'s reverse-isolation checks for what specifically
that would expose).

## One-time setup (as `praxis`, with `sudo`)

Only the parts that genuinely can't be routine: the shared secret (don't want
to regenerate it on every deploy) and first activation (`enable`, not just
`restart`, so it survives a reboot and starts once now). The `.service` file
itself is **not** installed here -- `make deploy` does that on every run, see
below, so a `.service` edit just needs a redeploy, not a separate manual step.

```bash
sudo -u praxis-sbx mkdir -p ~praxis-sbx/.config/praxis
sudo -u praxis-sbx sh -c 'echo "PRAXIS_ORCH_TOKEN=$(openssl rand -hex 32)" \
  > ~praxis-sbx/.config/praxis/orchestrator.env'
sudo chmod 0600 ~praxis-sbx/.config/praxis/orchestrator.env

sudo loginctl enable-linger praxis-sbx   # if 20-spawnbox-user.sh hasn't already

sudo make deploy                         # installs the .service file too, first time
sudo runuser -u praxis-sbx -- env XDG_RUNTIME_DIR=/run/user/"$(id -u praxis-sbx)" \
  systemctl --user enable praxis-orchestrator
```

The portal needs the same `PRAXIS_ORCH_TOKEN` value to authenticate its calls
(`X-Praxis-Token`) -- there is no portal yet, so for now this token just needs
to exist and match whatever manually drives the API during Phase D/C testing.

## Routine deploy (as `praxis`, with `sudo`)

```bash
sudo make deploy
```

Builds, installs the binary and the current `.service` file into `praxis-sbx`'s
home with correct ownership, reloads the unit, and restarts
`praxis-orchestrator` inside `praxis-sbx`'s own lingering `systemd --user`
instance. Safe to run any time the code or the `.service` file changes --
both install steps are idempotent, so nothing extra happens on a run where
neither did. `make deploy` refuses to run unprivileged rather than silently
installing nothing or restarting the wrong unit.

## Day-2 operations: `pxoctl`

`make deploy` installs `deploy/pxoctl.sh` to `/usr/local/bin/pxoctl` --
root-owned, on the default PATH for every account including `sudo`'s (see the
Makefile comment on why not `~/.local/bin`: `sudo`'s `secure_path`
deliberately excludes per-user directories, so a copy only your own PATH can
find works bare and then "command not found" the moment you put `sudo` in
front of it). One binary name, two subcommand groups, two different failure
classes -- check `unit` before `api`.

```bash
sudo pxoctl unit status
sudo pxoctl unit logs        # last 50 lines
sudo pxoctl unit logs -f     # follow
sudo pxoctl unit restart
sudo pxoctl unit stop

pxoctl api health            # no root -- just hits /healthz
sudo pxoctl api get attempt-1
```

(Before the first `sudo make deploy`, or if working straight from a checkout,
`./deploy/pxoctl.sh` works identically in place of `pxoctl`.)

**`unit`** exists for exactly one reason: every one of those is, underneath,
`runuser -u praxis-sbx -- env XDG_RUNTIME_DIR=/run/user/<uid> systemctl --user
<verb> praxis-orchestrator` -- correct, but not something anyone should have
to reconstruct from memory at the point they actually need it. Always needs
root; no way around that (only root can `runuser` into `praxis-sbx`).

**`api`** asks a different question on purpose: a unit systemd reports
`active (running)` can still be wedged or answering wrong, and a unit that's
`failed` doesn't need an API probe to explain itself -- check `unit status`
first. Mostly does **not** need root -- `health` never does, and `get` only
falls back to root if `PRAXIS_ORCH_TOKEN` isn't already exported (the more
realistic path anyway: the real portal, whenever it exists, calls this API
with just the token, no local root at all). Built to grow: as more read-only
routes land in `internal/api/server.go` (a bulk list-all-instances endpoint
doesn't exist yet, for one), add a case under `api`, not a new script.

Never self-elevates. A subcommand that needs root says so and exits with the
exact command to re-run -- `pxoctl.sh` will never prompt for your password on
its own initiative. `pxoctl.sh --help` lists every command.
