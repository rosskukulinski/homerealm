# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

homerealm is a single Go binary that manages Minecraft Bedrock worlds as Docker containers, wrapping each one in its own Tailscale identity so it's reachable on the tailnet with zero port-forwarding. No database, no accounts of its own — state is one `worlds.json` file, and authorization is derived entirely from Tailscale identity at request time.

## Commands

All Go commands run from `app/` (that's the Go module root, not the repo root).

```bash
cd app
go build ./...                        # compile
go vet ./...                          # static checks
gofmt -l .                            # list unformatted files (should be empty); gofmt -w . to fix
go test ./...                         # full test suite
go test ./... -run TestName -v        # single test, verbose
go run .                              # run locally (see "Running locally" below)
```

CI (`.github/workflows/ci.yml`) runs exactly: `gofmt -l .` (must be empty), `go vet`, `go build`, `go test`, then `shellcheck setup.sh cli/mc-world`, then a plain `docker build ./app`. Match all of that before considering a change done.

### Running locally

`go run .` needs no Docker and no real Tailscale to serve the plain-HTTP LAN listener (`panelLAN`, default port 8090) — that always comes up first and independently of tsnet. Requests hitting it always resolve to the read-only `roleReader` (Tailscale identity requires a real tailnet connection), so it's fine for checking that templates render and routes exist, but it can't exercise anything gated behind `CanManage`. Set `DATA_DIR` and `HOST_DATA_DIR` to a scratch directory before running so it doesn't touch a real deployment's data.

To exercise `CanManage`-gated code paths (settings, console, backups, danger zone) without a live tailnet, write a test using the `requester` seam instead (see Testing below) — don't try to fake Tailscale identity against a running server.

### Building the Docker image with build provenance

The footer's build-info line (branch/commit/PR, separate from the hand-bumped `appVersion` constant) only populates when built with `-ldflags -X`, wired through Dockerfile `ARG`s:

```bash
docker build ./app \
  --build-arg BUILD_COMMIT=$(git rev-parse HEAD) \
  --build-arg BUILD_BRANCH=$(git branch --show-current) \
  --build-arg BUILD_PR=<number, if testing a PR>
```

`publish.yml` passes these automatically from GitHub Actions context on every build; a plain `docker build` with no args still works, it just shows no build-info line.

## Architecture

### State and control flow

- `worlds.json` (in `DATA_DIR`) is the only persistent record of what worlds exist — one `world` struct per world (name, port, mode, difficulty, owner, etc). `load()`/`save()` are the only accessors; nearly every mutating handler follows the same shape: `mu.Lock()` → `load()` → mutate → `save()` → `regen()` → `compose("up", "-d", ...)`.
- `regen()` rewrites a **generated** `docker-compose.yml` (also in `DATA_DIR`) from `worlds.json` — this file is never hand-edited, and its header says so. It defines compose project `homerealm-worlds`, a project entirely separate from the panel's own docker-compose.yml/project (recreating the panel container never touches world containers, and vice versa).
- Player rosters and permissions are *not* in `worlds.json` — they're harvested from `docker logs mc-<name>` (regex on `"Player connected: ..."`) into per-world `players_seen.json`/`permissions.json` files under each world's own data directory.

### Every world is two containers, not one

Each world gets a `tailscale/tailscale` sidecar (`ts-<name>`, its own tailnet node, hostname `mc-<name>`) plus the `itzg/minecraft-bedrock-server` container (`mc-<name>`) running with `network_mode: service:ts-<name>` — sharing the sidecar's network namespace rather than getting its own IP. This is why every world answers on the *same* Bedrock port (19132) over Tailscale: each has its own tailnet IP via its own sidecar, so there's no port collision to avoid. `ensureSidecar()` exists specifically to backfill this sidecar for worlds created before this model existed (Start/Restart call it defensively; New/Settings/Clone already regenerate the sidecar as part of normal creation).

The panel itself is *also* a tsnet node (joins the tailnet directly via the `tailscale.com/tsnet` library — no `tailscaled` needed on the host). `localClient` (a `*tailscale.com/client/local.Client`) is how the panel talks to its own embedded tailscaled: `WhoIs()` for request authorization, `Status()` for enumerating tailnet peers (used by `tailscaleIPs()` to look up each world's assigned IP by hostname).

### Authorization has no accounts — it's derived per-request

`requester(r) (login string, role)` resolves identity via `localClient.WhoIs(ctx, r.RemoteAddr)`. Three roles: `roleAdmin` (in `PANEL_ADMINS`, or everyone if that's empty), `roleUser` (any other identified tailnet login), `roleReader` (LAN listener, tagged nodes, or WhoIs failure — always read-only). `canManage(login, role, world)` is the one function that decides if a request may mutate a *specific* world (admin, or the owner). `authWorld()` is the standard guard at the top of every mutating per-world handler: 404 if the world doesn't exist, 403 if the caller can't manage it, otherwise returns the world.

`requester` is a package-level `var`, not a plain function — that's deliberate, so tests can stub identity without a live tailnet (see Testing below). Never try to fake Tailscale identity by hitting a *running* server from a test or script; there's no way to do that short of a real tailnet connection.

### Templates: three files, two page types, shared shell

`base.tmpl` + `list.tmpl` are parsed together as `listPage` (the `/` world list); `base.tmpl` + `world.tmpl` as `worldPage` (`/world/{name}`). Both page files define a `content` template that `base.tmpl` invokes via `{{template "content" .}}` — this is why you can't just open one `.tmpl` file and understand it; the two files being combined for a given page must be read together. `world.tmpl` additionally defines a `scripts` block (for the live-console/command JS) that overrides `base.tmpl`'s empty default `{{block "scripts" .}}{{end}}`; `list.tmpl` has no reason to define one.

No JS framework — vanilla JS throughout. Live status/CPU-RAM refresh is a 5s `fetch('/api/worlds')` poll (`poll()` in `base.tmpl`); the live console is Server-Sent Events (`GET /console/{name}`, tailing `docker logs -f`) rather than polling, since log tailing is inherently a push model.

### Testing conventions

`main_test.go`'s `setupTest(t)` wires a fresh temp `DATA_DIR` and returns `routes()`; `as(login, role)` stubs `requester` for the duration of the test (restored via `t.Cleanup`); `post()`/`get()` are thin `httptest` wrappers returning just the status code (write a raw `httptest.NewRequest`/`NewRecorder` directly, as `TestWorldDetailOwnerView` does, when you need the response body). `sendCommand` (the `docker exec ... send-command` call) is stubbed the same way as `requester` for tests that need to assert on console-command behavior without a real container.

Handlers that only shell out to `docker`/`docker compose` and don't check the result (`docker()`, `compose()` helpers) are deliberately fire-and-forget — they log and move on, never fail the HTTP request. This means most handler tests only need to assert on auth/validation status codes (403/400/302), not on real container state; `go test` runs fine with no Docker daemon present at all. Don't add an assertion that depends on a real `docker` call succeeding or failing in a specific way — stub the relevant seam (`sendCommand`, `requester`) instead, or the test will be flaky depending on the environment's Docker install.
