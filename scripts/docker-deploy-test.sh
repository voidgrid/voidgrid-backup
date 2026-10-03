#!/bin/sh
# Boots the actual built image the way examples/server/ and examples/agent/
# describe it, and confirms both containers' health checks pass: the
# server's nonroot user + writable ./data, and the agent's cap_drop/cap_add
# set under its own reduced capabilities. This is the one thing CI's
# `go vet`/`go test`-in-the-build doesn't cover.
#
# Runs from throwaway copies of the example directories under .cache/, with
# a locally built image tag; examples/ itself is never touched, and nothing
# here reaches a real backup destination (the agent's Docker socket is the
# real one so it can start, but no job is ever created). Refuses to run if
# a container already has either example's fixed name, so it can't
# interfere with a real deployment on this machine.
set -eu
cd "$(dirname "$0")/.."

for n in voidgrid-backup-server voidgrid-backup-agent; do
  if docker inspect "$n" >/dev/null 2>&1; then
    echo "a container named $n already exists; stop it first -- this script" >&2
    echo "uses the examples' own fixed container names and won't touch it" >&2
    exit 1
  fi
done

id=$$
img="vb-deploytest:$id"
scratch="$PWD/.cache/deploytest-$id"
server_compose="$scratch/server/docker-compose.yaml"
agent_compose="$scratch/agent/docker-compose.yaml"

cleanup() {
  docker compose -f "$server_compose" --env-file "$scratch/server/.env" -p "hbdt-server-$id" down -v --remove-orphans >/dev/null 2>&1 || true
  docker compose -f "$agent_compose" --env-file "$scratch/agent/.env" -p "hbdt-agent-$id" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$scratch"
  docker rmi "$img" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "==> building $img (runs go vet && go test, same as CI)"
docker build -t "$img" . >/dev/null

mkdir -p "$scratch/server" "$scratch/agent"
cp examples/server/docker-compose.yaml examples/server/.env.example "$scratch/server/"
cp examples/agent/docker-compose.yaml examples/agent/.env.example "$scratch/agent/"

# --- server: nonroot, VB_UID/GID set to whoever runs this so ./data is
# writable without sudo-chowning it to the example's default 65532. ---
port_server=18080
sed -e "s#^VB_IMAGE=.*#VB_IMAGE=$img#" \
    -e "s#^VB_PORT=.*#VB_PORT=$port_server#" \
    -e "s#^VB_UID=.*#VB_UID=$(id -u)#" \
    -e "s#^VB_GID=.*#VB_GID=$(id -g)#" \
    "$scratch/server/.env.example" >"$scratch/server/.env"
mkdir -p "$scratch/server/data"

# --- agent: real Docker socket (so it starts cleanly under its cap set),
# throwaway everything else. A missing libvirt socket is handled gracefully
# (VM backups just disable themselves with a warning), so it's left
# pointing at a path that doesn't exist. ---
port_agent=19443
mkdir -p "$scratch/agent/data" "$scratch/agent/srv-stacks" "$scratch/agent/srv-data" \
  "$scratch/agent/srv-vms" "$scratch/agent/srv-restore"
: >"$scratch/agent/sftp_key"
sed -e "s#^VB_IMAGE=.*#VB_IMAGE=$img#" \
    -e "s#^VB_PORT=.*#VB_PORT=$port_agent#" \
    -e "s#^DOCKER_SOCKET=.*#DOCKER_SOCKET=/var/run/docker.sock#" \
    -e "s#^LIBVIRT_SOCKET=.*#LIBVIRT_SOCKET=$scratch/agent/no-libvirt-sock#" \
    -e "s#^STACKS_DIR=.*#STACKS_DIR=$scratch/agent/srv-stacks#" \
    -e "s#^DATA_DIR=.*#DATA_DIR=$scratch/agent/srv-data#" \
    -e "s#^VM_IMAGES_DIR=.*#VM_IMAGES_DIR=$scratch/agent/srv-vms#" \
    -e "s#^RESTORE_DIR=.*#RESTORE_DIR=$scratch/agent/srv-restore#" \
    -e "s#^SFTP_KEY=.*#SFTP_KEY=$scratch/agent/sftp_key#" \
    "$scratch/agent/.env.example" >"$scratch/agent/.env"

echo "==> starting server"
docker compose -f "$server_compose" --env-file "$scratch/server/.env" -p "hbdt-server-$id" up -d
echo "==> starting agent"
docker compose -f "$agent_compose" --env-file "$scratch/agent/.env" -p "hbdt-agent-$id" up -d

wait_healthy() {
  name="$1"
  tries=45
  status="starting"
  while [ "$tries" -gt 0 ]; do
    status=$(docker inspect --format '{{.State.Health.Status}}' "$name" 2>/dev/null || echo "missing")
    case "$status" in
    healthy)
      echo "$name: healthy"
      return 0
      ;;
    unhealthy)
      echo "$name: unhealthy"
      docker logs "$name" 2>&1 | tail -40
      return 1
      ;;
    esac
    tries=$((tries - 1))
    sleep 2
  done
  echo "$name: timed out waiting for healthy (last status: $status)"
  docker logs "$name" 2>&1 | tail -40
  return 1
}

ok=1
wait_healthy voidgrid-backup-server || ok=0
wait_healthy voidgrid-backup-agent || ok=0

if [ "$ok" = 1 ]; then
  echo "==> confirming the published port actually answers, not just the in-container probe"
  if ! curl -fsS "http://127.0.0.1:$port_server/healthz" >/dev/null; then
    echo "server: published port $port_server did not answer /healthz"
    ok=0
  fi
fi

if [ "$ok" = 1 ]; then
  echo "PASS: both containers booted from the example configs and are healthy"
else
  echo "FAIL"
  exit 1
fi
