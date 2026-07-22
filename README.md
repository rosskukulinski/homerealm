# 🏔 homerealm

**A self-hosted, Realm-style panel for Minecraft Bedrock worlds — built for
Synology NAS, works anywhere Docker runs.**

Run as many always-on Bedrock worlds as you like on hardware you own. Manage
them from a phone-friendly web panel: create worlds from presets or seeds,
clone them, tune settings, grant kids (or revoke cousins) permissions, stop
idle worlds to save RAM, and delete without fear — worlds archive instead of
disappearing. On your home network, worlds **appear automatically under
"LAN Games"** on iPads, consoles, and phones — no addresses to type.

Each world runs as its own container of the excellent
[itzg/minecraft-bedrock-server](https://github.com/itzg/docker-minecraft-bedrock-server).
homerealm is a thin management layer: one JSON manifest, one generated
compose file, one small Flask app. No database, no accounts, no cloud.

## Features

- **One world = one server** — switching worlds is just picking a different
  server on your device; worlds hold state whether anyone's online or not
- **Create** from presets (Survival easy/peaceful/hard, Creative sandbox,
  Adventure), with optional seed and flat-world toggle
- **Clone** any world — settings, builds, and permissions included
- **Settings** per world: mode, difficulty, cheats, max players, view
  distance, default permission for new players
- **Player permissions**: everyone who has ever joined is listed by gamertag
  (auto-harvested from server logs); promote to operator or demote to
  visitor from a dropdown
- **Start / stop / restart** — stopped worlds keep their data, free their
  RAM, and stay stopped across reboots
- **Delete = archive** — worlds move to `_archive/` with a timestamp
- **LAN auto-discovery** (optional): each world gets its own LAN IP via
  macvlan, so all of them show up under "LAN Games" at home
- **Auto version match**: worlds run `VERSION: LATEST`; when the Minecraft
  app updates, hit Restart and the server re-downloads to match
- **CLI companion** (`cli/mc-world`) for terminal folks — a thin client of
  the panel's JSON API

## Requirements

- Docker + Docker Compose (on Synology: the **Container Manager** package)
- SSH access with sudo (Synology: enable in Control Panel → Terminal & SNMP)
- Players need Microsoft/Xbox accounts (standard Bedrock multiplayer)

## Quick start

```bash
git clone https://github.com/rosskukulinski/homerealm.git
cd homerealm
./setup.sh          # first run creates .env — edit it, then:
./setup.sh          # creates data dir (+ macvlan if enabled), builds, starts
```

Open `http://<your-nas>:8090` and create your first world. Add it on devices
via **Play → Servers → Add Server** with your NAS address and the port shown
on the world card.

`.env` essentials:

| Variable | What it is |
|---|---|
| `HOST_DATA_DIR` | Where world data lives on the host |
| `PUID` / `PGID` | Owner for world files — run `id` to find yours |
| `REMOTE_ADDRESS` | Address shown as the "away" connect hint (see Remote play) |
| `DISCOVERY_ENABLED` | LAN auto-discovery via macvlan (read below first) |

## LAN auto-discovery (optional, recommended)

Bedrock clients only auto-discover servers on the **default port (19132) via
subnet broadcast** — so multiple port-mapped worlds can never all appear in
"LAN Games". homerealm solves this by giving each world its own LAN IP
(Docker macvlan), all answering on 19132.

Before enabling `DISCOVERY_ENABLED=true`:

1. **Reserve a few LAN IPs outside your DHCP pool** (e.g. shrink your pool to
   end at `.249`, use `.250–.254` for worlds). Collisions cause weird pain.
2. Know your host's LAN interface (`ip -br addr` — e.g. `eth0`, `bond0`; on
   Synology with Open vSwitch it's `ovs_bond0` / `ovs_eth0`).
3. Run `./setup.sh` — it creates the macvlan network interactively.

Note: macvlan means the *host itself* can't reach the world IPs (kernel
isolation) — devices on the LAN can. That's fine for normal use.

## Remote play

Auto-discovery is LAN-only physics (broadcasts don't route). For away-from-
home play, the clean path is [Tailscale](https://tailscale.com): put it on
the NAS and your devices, set `REMOTE_ADDRESS` to the NAS's tailnet name, and
join via address + port. The panel itself is also pleasant to expose as a
tailnet HTTPS service:

```bash
tailscale serve --service=svc:minecraft --https=443 8090
# approve the service + add a grant in your tailnet admin console
```

(Game traffic is UDP; Tailscale Services are TCP-only today, so worlds
connect via the node address, not the service name.)

For friends without a VPN, a tunnel like playit.gg works; if you instead
port-forward, **enable the allow-list** for that world — homerealm sets
`ALLOW_LIST: "false"` by default, which is right for LAN/VPN and wrong for
the open internet.

## Security model

The panel has **no authentication** and can start/stop/delete worlds.
It must only be reachable from networks where everyone is trusted: your LAN
and/or your VPN. **Never port-forward the panel.** (The archive-on-delete
design limits the blast radius of curious children.)

## CLI

```bash
cp cli/mc-world /usr/local/bin/ && chmod +x /usr/local/bin/mc-world
mc-world list
mc-world new skyblock creative
mc-world stop skyblock
```

## Troubleshooting

- **"You're not invited to play on this server"** — recent Bedrock versions
  ship with the allow-list enabled by default; homerealm disables it, but a
  hand-made world may not have it set. Check the world's `server.properties`.
- **Can't join after a Minecraft app update** — Restart the world; it
  re-downloads the matching server version.
- **Synology: containers crash-loop with permission errors** — DSM's ACLs
  can make POSIX modes lie. Fix ownership from *inside* a container:
  `docker run --rm -v /your/data:/d alpine chown -R <PUID>:<PGID> /d`
- **Settings apply but nothing changes** — if you run compose by hand, use
  the project name `-p homerealm-worlds`, or you'll orphan the containers.
- **Child account can't join anything** — enable "join multiplayer games"
  in Xbox family settings.

## Credits

- [itzg/docker-minecraft-bedrock-server](https://github.com/itzg/docker-minecraft-bedrock-server)
  does the heavy lifting of running Bedrock in a container. Consider
  [supporting itzg](https://github.com/sponsors/itzg).

## License

[MIT](LICENSE)
