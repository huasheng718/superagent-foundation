#!/usr/bin/env bash
# Build and start this SuperAgent checkout by default; prebuilt images are explicit.
#
# Usage:
#   ./start.sh            install/start in the background (same as -d)
#   ./start.sh -d         install/start in the background
#   ./start.sh logs       follow platform logs
#   ./start.sh stop       stop containers, keep them and their data
#   ./start.sh down       remove containers, keep data volumes
#   ./start.sh restart    restart the platform container
#   ./start.sh pull       rebuild source, or refresh the explicitly selected image
#   ./start.sh dev        build this checkout (compose.dev.yaml) and start it
#   ./start.sh release    select LOOPFORGE_IMAGE from the environment or .env
#   ./start.sh check      check production configuration without starting services
#   ./start.sh production check production configuration, then install/start
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source scripts/docker-access.sh

compose() {
  [[ -f .env ]] || docker_access_fail 'Not installed yet. Run ./start.sh first.'
  select_docker_access || docker_access_fail 'Docker is not running, or this user cannot access it.'
  docker_access compose --profile platform "$@"
}

cmd="${1:--d}"
shift || true
case "$cmd" in
  up|-d|detach) bash scripts/install.sh ;;
  dev) LOOPFORGE_BUILD_FROM_SOURCE=1 bash scripts/install.sh ;;
  release) LOOPFORGE_BUILD_FROM_SOURCE=0 bash scripts/install.sh ;;
  production) LOOPFORGE_PRODUCTION=1 bash scripts/install.sh ;;
  check)
    [[ -f .env ]] || docker_access_fail 'Not configured yet. Run bash scripts/configure.sh first.'
    set -a; source .env; set +a
    compose config --format json | python3 scripts/check-deployment.py --public-url "${LOOPFORGE_PUBLIC_URL:-}"
    ;;
  pull) LOOPFORGE_PULL_POLICY=always bash scripts/install.sh ;;
  logs) compose logs -f "${@:-platform}" ;;
  stop) compose stop ;;
  down) compose down --remove-orphans ;;
  restart) compose restart platform ;;
  build)
    echo 'To build and start this checkout: ./start.sh dev' >&2
    exit 1
    ;;
  *)
    echo "unknown: $cmd" >&2
    echo 'usage: ./start.sh [-d|logs|stop|down|restart|pull|dev|release|check|production]' >&2
    exit 1
    ;;
esac
