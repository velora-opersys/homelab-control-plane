# Homelab Control Plane

Private, self-hosted control plane for managing a mixed homelab from one place.

## v0.1 goals

- Health dashboard
- Resource graph and interactive topology
- Linux/Windows agent enrolment over outbound-only connections
- Device discovery and managed/unmanaged state
- Live host metrics
- Docker capability discovery
- Owner setup and local authentication
- PostgreSQL-backed resource inventory
- Foundation for backups, remote access, automation, Proxmox, Synology, NPM, UniFi and Home Assistant

## Stack

- Go API
- Go cross-platform agent
- React + TypeScript web UI
- PostgreSQL
- Docker Compose

## Install

```bash
git clone https://github.com/velora-opersys/homelab-control-plane.git
cd homelab-control-plane
sudo ./install.sh
```

The repository is private, so authenticate to GitHub before cloning.

Default web port: `8787`.

## Development status

This repository is under active development. The initial implementation is intentionally focused on proving the core flow: install control plane -> create owner -> enrol host -> receive metrics -> inspect host -> view topology.
