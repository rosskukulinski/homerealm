<!-- Thanks for contributing! Keep PRs small and focused. -->

## What & why

<!-- What does this change, and what problem does it solve? Link the issue: "Closes #123". -->

## How it was tested

<!-- What did you run/click to confirm it works? -->

## Checklist

- [ ] Ran the full check suite locally (from `app/` unless noted):
  - [ ] `gofmt -l .` is empty
  - [ ] `go vet ./...`
  - [ ] `go build ./...`
  - [ ] `go test ./...`
  - [ ] `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` — 0 reachable
  - [ ] `shellcheck setup.sh cli/mc-world` (if shell changed)
  - [ ] `docker build ./app` (if the build/Dockerfile changed)
- [ ] Added or updated tests for the change
- [ ] No secrets, generated files, or the built binary committed
- [ ] Updated docs (README / CLAUDE.md / AGENTS.md) if behavior or conventions changed
- [ ] Security-relevant? Considered auth/role, CSRF, and injection (see [SECURITY.md](../SECURITY.md))
