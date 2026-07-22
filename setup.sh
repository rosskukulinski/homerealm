#!/bin/bash
# homerealm setup — tested on Synology DSM 7.x; works on any Linux with Docker.
set -euo pipefail

DOCKER=$(command -v docker || echo /usr/local/bin/docker)
COMPOSE="$DOCKER compose"
$DOCKER compose version >/dev/null 2>&1 || COMPOSE=$(command -v docker-compose || echo /usr/local/bin/docker-compose)

if [ ! -f .env ]; then
  cp .env.example .env
  echo ">> Created .env from .env.example — edit it (at minimum HOST_DATA_DIR, PUID/PGID),"
  echo ">> then re-run ./setup.sh"
  echo ">> Tip: run 'id' to find your PUID/PGID values."
  exit 0
fi

# shellcheck disable=SC1091
source .env

echo ">> Creating data dir $HOST_DATA_DIR"
sudo mkdir -p "$HOST_DATA_DIR"
# Synology gotcha: root-created dirs + ACLs break container writes. Fix ownership
# from inside a container so it works regardless of host chmod/ACL quirks.
sudo $DOCKER run --rm -v "$HOST_DATA_DIR:/d" alpine \
  sh -c "chown ${PUID}:${PGID} /d && chmod 775 /d"

if [ "${DISCOVERY_ENABLED:-false}" = "true" ]; then
  if ! sudo $DOCKER network inspect "$MACVLAN_NETWORK" >/dev/null 2>&1; then
    read -rp ">> LAN parent interface for macvlan (e.g. eth0, bond0, ovs_bond0): " PARENT
    read -rp ">> LAN subnet CIDR (e.g. 192.168.1.0/24): " SUBNET
    read -rp ">> LAN gateway (e.g. 192.168.1.1): " GW
    sudo $DOCKER network create -d macvlan --subnet="$SUBNET" --gateway="$GW" \
      -o parent="$PARENT" "$MACVLAN_NETWORK"
    echo ">> Created macvlan '$MACVLAN_NETWORK'. Make sure ${WORLD_IP_PREFIX}${WORLD_IP_FIRST}-${WORLD_IP_LAST}"
    echo ">> is OUTSIDE your router's DHCP pool."
  fi
fi

echo ">> Building and starting the panel"
sudo $COMPOSE up -d --build
echo ">> Done. Panel: http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo localhost):${PANEL_PORT:-8090}"
