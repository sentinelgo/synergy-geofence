#!/usr/bin/env bash
#
# Builds, pushes and deploys synergy-geofence to a Docker Swarm cluster.
#
# docker-stack.yml declares the config as an *external* Swarm secret, and
# Swarm secrets are immutable, so this script does what
# `docker stack deploy -c docker-stack.yml` alone can't:
#
#   1. Build the service image from this repo and push it to the registry.
#   2. Package config.<env>.yml into a content-addressed secret (an edit
#      gets a new secret name; an unchanged config reuses the old one).
#   3. Deploy/update the stack, pointed at the new image and secret.
#
#
set -euo pipefail

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  sed -n '2,29p' "$0"
  exit 0
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# Preserve caller-supplied overrides before loading env files, so a
# .env.<env> can set defaults without clobbering an explicit `TAG=x ./deploy.sh`.
USER_REGISTRY_OVERRIDE="${REGISTRY:-}"
USER_TAG_OVERRIDE="${TAG:-}"
USER_CONFIG_FILE_OVERRIDE="${CONFIG_FILE:-}"
USER_STACK_NAME_OVERRIDE="${STACK_NAME:-}"
USER_REPLICAS_OVERRIDE="${REPLICAS:-}"
USER_SERVER_PORT_OVERRIDE="${SERVER_PORT:-}"

# ── Environment selection ──────────────────────────────────────────────────
ENVIRONMENT="${1:-${DEPLOY_ENV:-prod}}"
case "$ENVIRONMENT" in
  dev|qa|demo|prod) ;;
  *)
    echo "error: environment must be one of: dev, qa, demo, prod (got '$ENVIRONMENT')" >&2
    exit 1
    ;;
esac

# ── Load env files (highest to lowest priority) ────────────────────────────
ENV_FILE_CONFIG="$HOME/.synergy/config/$ENVIRONMENT/.env"
ENV_FILE_ENV="$SCRIPT_DIR/.env.$ENVIRONMENT"
ENV_FILE_ROOT="$SCRIPT_DIR/.env"

if [[ -f "$ENV_FILE_CONFIG" ]]; then
  echo "==> loading env from $ENV_FILE_CONFIG"
  # shellcheck disable=SC1090
  . "$ENV_FILE_CONFIG"
elif [[ -f "$ENV_FILE_ENV" ]]; then
  echo "==> loading env from $ENV_FILE_ENV"
  # shellcheck disable=SC1090
  . "$ENV_FILE_ENV"
elif [[ -f "$ENV_FILE_ROOT" ]]; then
  echo "==> loading env from $ENV_FILE_ROOT"
  # shellcheck disable=SC1090
  . "$ENV_FILE_ROOT"
fi

# Re-apply caller overrides so they win over anything an env file set.
[[ -n "$USER_REGISTRY_OVERRIDE" ]] && REGISTRY="$USER_REGISTRY_OVERRIDE"
[[ -n "$USER_TAG_OVERRIDE" ]] && TAG="$USER_TAG_OVERRIDE"
[[ -n "$USER_CONFIG_FILE_OVERRIDE" ]] && CONFIG_FILE="$USER_CONFIG_FILE_OVERRIDE"
[[ -n "$USER_STACK_NAME_OVERRIDE" ]] && STACK_NAME="$USER_STACK_NAME_OVERRIDE"
[[ -n "$USER_REPLICAS_OVERRIDE" ]] && REPLICAS="$USER_REPLICAS_OVERRIDE"
[[ -n "$USER_SERVER_PORT_OVERRIDE" ]] && SERVER_PORT="$USER_SERVER_PORT_OVERRIDE"

# ── Defaults ────────────────────────────────────────────────────────────────
REGISTRY="${REGISTRY:-registry.example.com/synergy}"
TAG="${TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo latest)}"
CONFIG_FILE="${CONFIG_FILE:-config.$ENVIRONMENT.yml}"
SERVER_PORT="${SERVER_PORT:-7010}"

case "$ENVIRONMENT" in
  dev)  DEFAULT_STACK_NAME="synergy-geofence-dev";  DEFAULT_REPLICAS="1" ;;
  qa)   DEFAULT_STACK_NAME="synergy-geofence-qa";   DEFAULT_REPLICAS="2" ;;
  demo) DEFAULT_STACK_NAME="synergy-geofence-demo"; DEFAULT_REPLICAS="1" ;;
  prod) DEFAULT_STACK_NAME="synergy-geofence-prod"; DEFAULT_REPLICAS="2" ;;
esac
STACK_NAME="${STACK_NAME:-$DEFAULT_STACK_NAME}"
REPLICAS="${REPLICAS:-$DEFAULT_REPLICAS}"

IMAGE="$REGISTRY/synergy-geofence"
SECRET_BASENAME="geofence_config_${ENVIRONMENT}"

DOCKER_CMD="${DOCKER_CMD:-docker}"
case "$DOCKER_CMD" in
  sudo\ docker) DOCKER_CMD="sudo -E docker" ;;
  sudo*) printf '%s' "$DOCKER_CMD" | grep -q -- '-E' || DOCKER_CMD="$(printf '%s' "$DOCKER_CMD" | sed 's/^sudo/sudo -E/')" ;;
esac

echo "=== synergy-geofence Swarm deploy ($ENVIRONMENT) ==="
echo "  Image:       ${IMAGE}:${TAG}"
echo "  Config file: $CONFIG_FILE"
echo "  Stack:       $STACK_NAME"
echo "  Replicas:    $REPLICAS"
echo "  Port:        $SERVER_PORT"
echo

command -v docker >/dev/null 2>&1 || { echo "error: docker is not on PATH" >&2; exit 1; }

if [[ ! -f "$CONFIG_FILE" ]]; then
  echo "error: $CONFIG_FILE not found. Copy config.$ENVIRONMENT.yml.example to $CONFIG_FILE and fill in the real values." >&2
  exit 1
fi

if grep -q CHANGEME "$CONFIG_FILE"; then
  echo "error: $CONFIG_FILE still has CHANGEME placeholders" >&2
  exit 1
fi

if [[ "$($DOCKER_CMD info --format '{{.Swarm.ControlAvailable}}' 2>/dev/null)" != "true" ]]; then
  echo "error: this node is not a Swarm manager" >&2
  exit 1
fi

# ── Optional registry login ─────────────────────────────────────────────────
REGISTRY_LOGIN_URL="${REGISTRY_LOGIN_URL:-}"
REGISTRY_USER="${REGISTRY_USER:-}"
REGISTRY_TOKEN="${REGISTRY_TOKEN:-}"
if [[ -n "$REGISTRY_LOGIN_URL" || -n "$REGISTRY_USER" || -n "$REGISTRY_TOKEN" ]]; then
  if [[ -z "$REGISTRY_LOGIN_URL" || -z "$REGISTRY_USER" || -z "$REGISTRY_TOKEN" ]]; then
    echo "error: registry login requires all of REGISTRY_LOGIN_URL, REGISTRY_USER, REGISTRY_TOKEN" >&2
    exit 1
  fi
  echo "==> logging in to registry $REGISTRY_LOGIN_URL"
  printf '%s' "$REGISTRY_TOKEN" | $DOCKER_CMD login "$REGISTRY_LOGIN_URL" --username "$REGISTRY_USER" --password-stdin
fi

# ── 1. Build & push the image ───────────────────────────────────────────────
# Only pass BuildKit's SSH mount if an agent socket is available (Dockerfile
# uses --mount=type=ssh to fetch private github.com/sentinelgo/* modules).
DOCKER_SSH_ARG=""
if [[ -n "${SSH_AUTH_SOCK:-}" && -S "${SSH_AUTH_SOCK:-}" ]]; then
  DOCKER_SSH_ARG="--ssh default"
fi

GIT_BRANCH="${GIT_BRANCH:-$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)}"
GIT_COMMIT="${GIT_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
BUILD_NUMBER="${BUILD_NUMBER:-local}"

echo "==> building ${IMAGE}:${TAG}"
DOCKER_BUILDKIT=1 $DOCKER_CMD build \
  $DOCKER_SSH_ARG \
  --build-arg BUILD_NUMBER="$BUILD_NUMBER" \
  --build-arg GIT_BRANCH="$GIT_BRANCH" \
  --build-arg GIT_COMMIT="$GIT_COMMIT" \
  --build-arg BUILD_DATE="$BUILD_DATE" \
  -t "${IMAGE}:${TAG}" \
  -t "${IMAGE}:latest" \
  "$SCRIPT_DIR"

echo "==> pushing ${IMAGE}:${TAG}"
$DOCKER_CMD push "${IMAGE}:${TAG}"
$DOCKER_CMD push "${IMAGE}:latest"

# ── 2. Version the config secret by content hash ────────────────────────────
CONFIG_HASH="$(sha256sum "$CONFIG_FILE" | cut -c1-12)"
CONFIG_SECRET="${SECRET_BASENAME}_${CONFIG_HASH}"

if $DOCKER_CMD secret inspect "$CONFIG_SECRET" >/dev/null 2>&1; then
  echo "==> secret $CONFIG_SECRET already exists, reusing"
else
  echo "==> creating secret $CONFIG_SECRET from $CONFIG_FILE"
  $DOCKER_CMD secret create "$CONFIG_SECRET" "$CONFIG_FILE" >/dev/null
fi



# ── 4. Deploy the stack ──────────────────────────────────────────────────────
echo "==> deploying $ENVIRONMENT stack '$STACK_NAME'"
IMAGE="$IMAGE" IMAGE_TAG="$TAG" SERVER_PORT="$SERVER_PORT" CONFIG_SECRET="$CONFIG_SECRET" REPLICAS="$REPLICAS" \
  $DOCKER_CMD stack deploy \
    -c docker-stack.yml \
    --with-registry-auth \
    --prune \
    "$STACK_NAME"

echo
echo "=== deploy complete ($ENVIRONMENT) ==="
echo "Monitor with: $DOCKER_CMD stack services $STACK_NAME"
echo "Logs:         $DOCKER_CMD service logs -f ${STACK_NAME}_geofence-service"