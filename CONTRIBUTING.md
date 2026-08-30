# Contributing to homerealm

Thanks for your interest! homerealm is a single Go binary that manages
Minecraft Bedrock worlds as Docker containers, each wrapped in its own
Tailscale identity. It's small on purpose — one `main.go`, three templates, no
database — so it's an easy codebase to get into.

By participating you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).
Found a security issue? **Don't** open a public issue — see [SECURITY.md](SECURITY.md).

## Ways to contribute

- **Bugs & features:** open an [issue](https://github.com/rosskukulinski/homerealm/issues/new/choose) first for anything non-trivial, so we can agree on the approach before you write code.
- **Docs:** fixes to the README, this guide, or code comments are always welcome.
- **Code:** see the workflow below.

## Development setup

You need **Go 1.26+** (the toolchain is pinned in `app/go.mod`) and, for the
Docker/shell parts of CI, `docker` and `shellcheck`. All Go commands run from
`app/` — that's the module root, not the repo root.

```bash
cd app
go build ./...        # compile
go run .              # run locally (no Docker or real Tailscale needed — see below)
```

`go run .` serves the plain-HTTP LAN listener (default `:8090`) with no Docker
and no tailnet. Every request there resolves to the read-only `roleReader`
(real identity needs a live tailnet), so it's perfect for checking that
templates render and routes exist, but it can't exercise anything behind
`CanManage`. **Set `DATA_DIR` and `HOST_DATA_DIR` to a scratch directory first**
so you don't touch a real deployment:

```bash
DATA_DIR=/tmp/hr HOST_DATA_DIR=/tmp/hr go run .
```

To exercise `CanManage`-gated paths (settings, console, backups, danger zone)
without a live tailnet, write a test using the `requester` seam — don't try to
fake Tailscale identity against a running server (there's no way to, short of a
real tailnet connection). See **Testing** below.

## Before you open a PR

Run the same checks CI runs — from `app/` unless noted:

```bash
gofmt -l .            # must print nothing; `gofmt -w .` to fix
go vet ./...
go build ./...
go test ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # reachable CVEs
```

And from the repo root:

```bash
shellcheck setup.sh cli/mc-world
docker build ./app                                       # must build clean
```

A change isn't done until all of these pass. CI enforces every one of them.

## Testing conventions

Tests live in `app/main_test.go` and run with **no Docker daemon present** —
the `docker`/`docker compose` helpers are fire-and-forget (they log and move
on), so most handler tests only assert on auth/validation status codes
(403/400/302), not real container state. Don't add assertions that depend on a
real `docker` call succeeding.

Two package-level `var` seams let tests stand in for the environment:

- **`requester`** — stub identity/role with the `as(login, role)` helper (restored via `t.Cleanup`).
- **`sendCommand`** — stub the console `docker exec` call.

`setupTest(t)` wires a fresh temp `DATA_DIR` and returns the router; `post()` /
`get()` are thin `httptest` wrappers. Copy the shape of an existing test.

## Architecture & conventions

The deep dive — state/control flow, the two-containers-per-world model,
per-request authorization, and the template layout — lives in
**[CLAUDE.md](CLAUDE.md)**. Read it before a non-trivial change; it's written
for both humans and AI coding agents (see [AGENTS.md](AGENTS.md)). A few house
rules:

- **Vanilla everything** — no JS framework, no ORM, no web framework beyond the stdlib `net/http` mux. Keep it that way unless there's a strong reason.
- **Match the surrounding code** — comment density, naming, and idiom. Comments explain *why* / constraints, not *what*.
- **`html/template` auto-escaping is load-bearing** — never build HTML by string concatenation; the one untrusted external input (player gamertags from logs) is safe only because templates escape it.

## Commit & PR style

- Keep commits focused; write a clear subject line in the imperative-ish style of the existing history (e.g. "Add live console, CPU/RAM stats, and on-demand backups").
- Fill in the PR template checklist.
- Reference the issue you're addressing.
- Small, reviewable PRs merge faster than large ones.

## Releases (maintainers)

Images publish only on a `v*` tag (not on merge to `main`). To cut a release:
bump `appVersion` in `app/main.go`, push it to `main`, then create the
`vX.Y.Z` GitHub Release — `publish.yml` builds `:latest` + the version tag.
Follow semver: a change that requires self-hosters to edit their
`docker-compose.yml` (not just pull) is at least a minor.
