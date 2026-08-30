# Security Policy

homerealm sits in a sensitive spot: it authorizes actions by Tailscale
identity and drives the Docker daemon on the host. We take reports seriously
and appreciate responsible disclosure.

## Reporting a vulnerability

**Please do not open a public issue for a security vulnerability.**

Use GitHub's private reporting: go to the repository's
**[Security tab → Report a vulnerability](https://github.com/rosskukulinski/homerealm/security/advisories/new)**.
This opens a private advisory visible only to the maintainers.

Please include:

- what the issue is and the impact you see (e.g. privilege escalation, auth bypass, RCE),
- steps or a proof-of-concept to reproduce,
- the version/commit and how the panel is deployed (tailnet-only, LAN listener on, discovery on, `PANEL_ADMINS` set or empty).

We'll acknowledge within a few days and keep you updated. Once a fix ships,
we're happy to credit you in the release notes unless you'd rather stay
anonymous.

## Supported versions

This is a solo/community project without long-term support branches. Security
fixes land on `main` and ship in the next tagged release. Please run a recent
release; **always deploy over your tailnet** and never port-forward the panel.

## Scope & threat model

Authorization is derived per request from Tailscale identity (`WhoIs`) — there
are no accounts or passwords. Reports we're especially interested in:

- **Auth/role bypass** — a request acting above its role (`reader` < `user` < `admin`), or managing a world the caller doesn't own.
- **CSRF / confused-deputy** — getting a browser to perform a state-changing action as a signed-in tailnet user (the panel guards against this; gaps are in scope).
- **Injection** — command, template/XSS, path traversal, or docker-compose interpolation via world names, seeds, player data, or backup filenames.
- **Secret exposure** — leaking `TS_AUTHKEY`, OAuth client secrets, or minted keys.
- **Unauthenticated disclosure** — the LAN listener and `/api/worlds` are intentionally reachable without identity; report anything sensitive they expose beyond world names/status/game type.

### Known residual (not a vuln report)

homerealm must create and start containers to do its job, so a **full
compromise of the panel process is host-privileged by design**. The
[docker-socket-proxy sidecar](README.md#docker-access-is-brokered-not-raw)
shrinks the blast radius (read-only socket, whitelisted API sections) but is
**not a complete containment boundary** — a create-capable client can still
craft an escaping container. Hardening this further (a broker scoped to
homerealm's exact operations) is welcome as a design discussion.

Also by design: with `PANEL_ADMINS` empty, **every** identified tailnet user is
an admin. That's a documented default, not a vulnerability — set `PANEL_ADMINS`
to lock it down.
