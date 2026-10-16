# The devcontainer is a CI dependency, not a convenience

`build-push` runs on `[self-hosted, devcontainer-runner]`, and those runners **are this
repository's devcontainer**: pks-cli's provisioner clones the repo, runs `devcontainer up`
against `devcontainer.json`, installs a JIT runner inside the resulting container and lets it
take the job. Whatever this folder says the runner is, that is what the runner is.

So the `docker-in-docker` feature and `postStartCommand: sudo /usr/local/share/docker-init.sh`
are not developer ergonomics — they are why `docker buildx build` can run at all. The Dockerfile
grants `node` passwordless sudo for exactly that script (and `dockerd`/`containerd`), which is
the precise path the workflow's guard looks for.

## This is the house devcontainer — keep it that way

These files are a copy of `projects/pks-agent-doorman/.devcontainer/` with one edit, the `name`.
The `Dockerfile` is byte-identical to the workspace root's, and every `pks-agent-*` repository
carries the same four files. **Do not hand-roll a lighter one for this repo.** That was tried on
`0.1.0` and cost two failed builds:

- No `.devcontainer/` at all → the CLI falls back to a stock base image with no docker in it. The
  runner registers with the right labels, takes the job, and dies on the first step with
  `No docker daemon and no /usr/local/share/docker-init.sh on this runner` — the guard telling
  you exactly this.
- A hand-written `image` + `features` config → the provisioner respawned a runner every ~39s
  without one ever coming online. Far too fast to be a feature build, so it was failing before
  it started.

If a build needs something extra, add it to the shared base image or to this Dockerfile in the
same shape the other repos use — do not diverge the structure.

## What is in here

| File | Role |
|---|---|
| `Dockerfile` | `ghcr.io/pksorensen/pks-fullstack-base` plus the sudoers line for `docker-init.sh`. Identical across the fleet. |
| `devcontainer.json` | dind + powershell features, the firewall `runArgs`, `remoteUser: node`, the Claude config volumes. |
| `init-firewall.sh` | Egress allowlist, run with `NET_ADMIN`/`NET_RAW`. |
| `.gitignore` | Keeps `.env` (the GitHub PAT the `initializeCommand` scaffolds) out of git. |

The release build does not use any of it — `Dockerfile` at the repo root pulls its own
`sdk:10.0` — so changes here cannot alter what ships, only whether it ships at all.
