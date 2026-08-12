#!/usr/bin/env bash
set -euo pipefail

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "Run this installer with sudo/root." >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

if ! command -v docker >/dev/null 2>&1; then
  if command -v curl >/dev/null 2>&1; then
    echo "Docker was not found; installing Docker Engine..."
    curl -fsSL https://get.docker.com | sh
  else
    echo "Docker is required and curl is not installed." >&2
    exit 1
  fi
fi

if ! docker compose version >/dev/null 2>&1; then
  echo "Docker Compose v2 is required." >&2
  exit 1
fi

if [[ ! -f .env ]]; then
  DB_PASSWORD="$(openssl rand -hex 24 2>/dev/null || head -c 48 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat > .env <<ENV
HCP_PORT=8787
POSTGRES_DB=homelab
POSTGRES_USER=homelab
POSTGRES_PASSWORD=${DB_PASSWORD}
ENV
  chmod 600 .env
  echo "Generated .env with a unique database password."
fi

docker compose up -d --build

IP="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
PORT="$(grep '^HCP_PORT=' .env | cut -d= -f2 || echo 8787)"
echo
echo "Homelab Control Plane is starting."
echo "Open: http://${IP:-localhost}:${PORT}"
echo "Create the owner account on first launch."
