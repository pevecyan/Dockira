#!/usr/bin/env bash
# Install or upgrade Dockira on a server over SSH.
#
#   deploy/deploy.sh [user@host]          default: DM_HOST, else user@host from .env
#
# By default the server pulls the published image (ghcr.io/pevecyan/dockira:latest).
# DM_BUILD=1 builds the image from this checkout instead and ships it over SSH (no registry);
# the next run without DM_BUILD, or an update from the UI, goes back to the published image.
#
# First run on a server: creates /root/apps/docker-manager with compose.yaml and a config.yaml
# holding a new secret key and the password from DM_PASSWORD (or a generated one, printed once).
# Later runs only replace the image (and compose.yaml with DM_UPDATE_COMPOSE=1).
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ -z "${DM_HOST:-}" && -f .env ]]; then
  DM_HOST="$(grep -E '^user=' .env | cut -d= -f2)@$(grep -E '^host=' .env | cut -d= -f2)"
fi
HOST="${1:-${DM_HOST:?usage: deploy/deploy.sh user@host}}"
KEY="${DM_SSH_KEY:-$HOME/.ssh/docker-manager_deploy_ed25519}"
DIR="${DM_DIR:-/root/apps/docker-manager}"
IMAGE="${DM_IMAGE:-ghcr.io/pevecyan/dockira}"
SSH=(ssh -o BatchMode=yes -o ConnectTimeout=10)
[[ -f "$KEY" ]] && SSH+=(-i "$KEY")
remote() { "${SSH[@]}" "$HOST" "$@"; }

if [[ "${DM_BUILD:-}" == 1 ]]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
  echo "▸ building $IMAGE:$VERSION"
  docker build -q -f deploy/Dockerfile --build-arg VERSION="$VERSION" -t "$IMAGE:$VERSION" -t "$IMAGE:latest" . >/dev/null
  echo "▸ copying the image to $HOST"
  docker save "$IMAGE:$VERSION" | gzip -1 | remote 'gunzip | docker load -q'
  remote "docker tag $IMAGE:$VERSION $IMAGE:latest"
else
  echo "▸ pulling $IMAGE:latest on $HOST"
  remote "docker pull -q $IMAGE:latest" >/dev/null
fi

remote "mkdir -p $DIR/data && chmod 700 $DIR"
if [[ "${DM_UPDATE_COMPOSE:-}" == 1 ]] || ! remote "test -f $DIR/compose.yaml"; then
  echo "▸ writing $DIR/compose.yaml"
  sed "s|ghcr.io/pevecyan/dockira:latest|$IMAGE:latest|" deploy/compose.yaml | remote "cat > $DIR/compose.yaml"
fi

if ! remote "test -f $DIR/config.yaml"; then
  echo "▸ first install: creating $DIR/config.yaml"
  PW="${DM_PASSWORD:-$(head -c 18 /dev/urandom | base64 | tr -d '/+=' | cut -c1-24)}"
  HASH="$(printf '%s\n' "$PW" | remote "docker run --rm -i $IMAGE:latest password")"
  SECRET="$(remote "docker run --rm $IMAGE:latest gen-key")"
  sed -e "s|^password_hash: \"\"|password_hash: \"$HASH\"|" -e "s|^secret_key: \"\"|secret_key: \"$SECRET\"|" deploy/config.example.yaml |
    remote "umask 077 && cat > $DIR/config.yaml"
  if [[ -z "${DM_PASSWORD:-}" ]]; then
    echo
    echo "  UI password (shown once, store it in your password manager): $PW"
    echo
  fi
fi

echo "▸ starting"
remote "cd $DIR && docker compose up -d --remove-orphans 2>&1 | tail -n 3"

echo -n "▸ waiting for it to be healthy"
for _ in $(seq 1 30); do
  status="$(remote "docker inspect -f '{{.State.Health.Status}}' docker-manager 2>/dev/null" || true)"
  if [[ "$status" == healthy ]]; then
    echo " ✓"
    echo "  http://${HOST#*@}:8090  (Dockira $(remote "docker exec docker-manager docker-manager version" 2>/dev/null || echo '?'))"
    if [[ "${DM_BUILD:-}" == 1 ]]; then
      # drop older local builds (only our image; nothing else on the server is touched)
      remote "docker images $IMAGE --format '{{.Tag}}' | grep -vx -e latest -e '$VERSION' | sed 's|^|$IMAGE:|' | xargs -r docker rmi -f >/dev/null 2>&1 || true"
    fi
    exit 0
  fi
  echo -n "."
  sleep 2
done
echo
echo "not healthy after 60 s; last logs:" >&2
remote "docker logs --tail 30 docker-manager" >&2
exit 1
