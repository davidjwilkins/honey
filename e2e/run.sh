#!/bin/sh
# Runs the end-to-end tests: a real WordPress (with the purge plugin) behind
# Honey.  Needs Docker and Go.  Set HONEY_E2E_KEEP=1 to leave it running
# afterwards (on http://localhost:8080, with the admin page on :9090).
set -eu
cd "$(dirname "$0")"
dir="$(pwd)"
compose() { docker compose -f "$dir/docker-compose.yml" "$@"; }

if [ -z "${HONEY_E2E_NO_BUILD:-}" ]; then
  docker build -t honey:e2e ..
fi
compose down -v --remove-orphans >/dev/null 2>&1 || true
if [ -z "${HONEY_E2E_KEEP:-}" ]; then
  # Keep the logs, for working out why a test failed
  trap 'compose logs honey wordpress > "$dir/logs.txt" 2>&1; compose down -v --remove-orphans' EXIT
fi
compose up -d --wait db
compose up -d wordpress honey

echo "waiting for WordPress"
for i in $(seq 1 60); do
  # the installer is served once WordPress has written its config
  if curl -fsS -o /dev/null http://127.0.0.1:8080/wp-admin/install.php; then break; fi
  sleep 2
done
compose run --rm cli sh /setup.sh

cd .. && go test -tags e2e -count=1 -v ./e2e
