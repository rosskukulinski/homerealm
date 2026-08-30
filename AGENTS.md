# AGENTS.md

Guidance for AI coding agents (Claude Code, and any tool that reads
`AGENTS.md`) working in this repo. Humans: see [CONTRIBUTING.md](CONTRIBUTING.md).

The detailed, canonical guidance — architecture, control flow, the
two-containers-per-world model, per-request authorization, template layout,
and testing seams — lives in **[CLAUDE.md](CLAUDE.md)**. Read it first; this
file is the short vendor-neutral entry point and points there so both Claude
and other agents get the same instructions without drift.

## Definition of done

A change is complete only when all of these pass. Run them before proposing a
diff — don't hand back work that fails CI. Go commands run from `app/`:

```bash
cd app
gofmt -l .                                              # must be empty
go vet ./...
go build ./...
go test ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...   # 0 reachable
cd .. && shellcheck setup.sh cli/mc-world               # if you touched shell
docker build ./app                                      # if you touched the build/Dockerfile
```

## Things to get right (the common traps)

- **Module root is `app/`, not the repo root.** All Go tooling runs there.
- **No new frameworks.** Vanilla `net/http`, `html/template`, and plain JS. Don't add a web framework, ORM, or JS build step.
- **Auth is derived per request** from Tailscale identity — there are no accounts. `authWorld()` guards every mutating per-world handler. Don't invent a session/cookie layer.
- **Tests run with no Docker daemon.** The `docker`/`docker compose` helpers are fire-and-forget. Stub the `requester` and `sendCommand` package-level `var` seams instead of expecting real containers. Never fake Tailscale identity against a running server.
- **Templates come in pairs.** `base.tmpl` + `list.tmpl` render the list page; `base.tmpl` + `world.tmpl` render the detail page. Read both files of a pair together — neither is complete alone.
- **`html/template` escaping is a security control**, not a formatting nicety. Never assemble HTML by concatenation.
- **Security-sensitive by nature** — the panel drives the Docker daemon and authorizes by network identity. When in doubt about an auth, injection, or CSRF question, prefer the conservative choice and call it out. See [SECURITY.md](SECURITY.md).

## Etiquette

- Keep diffs small and focused; match the surrounding style.
- Open or reference an issue for non-trivial changes.
- Don't commit secrets, generated `worlds.json`/compose files, or the built binary (they're git-ignored for a reason).
- If you update the build/test workflow or these conventions, update
  `CLAUDE.md` and `AGENTS.md` together so they stay in sync.
