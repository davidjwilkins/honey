#!/bin/sh
# Benchmarks Honey in front of a real WordPress (the end-to-end tests'
# Docker setup): cache hits, logged in visitors (whose requests all go to
# WordPress), a mix with requests the cache can't answer, and a soak test
# with purges, reporting throughput, latency and Honey's memory.  Needs
# Docker and Go.
#
#   bench/run.sh                      # each scenario for 20s, a 2m soak
#   DURATION=60s SOAK=10m bench/run.sh
#   HONEY_E2E_NO_BUILD=1 bench/run.sh # with an honey:e2e image you built
set -eu
cd "$(dirname "$0")/.."
root="$(pwd)"
compose() { docker compose -f "$root/e2e/docker-compose.yml" "$@"; }
duration="${DURATION:-20s}"
soak="${SOAK:-2m}"
concurrency="${CONCURRENCY:-50}"

if [ -z "${HONEY_E2E_NO_BUILD:-}" ]; then
  docker build -q -t honey:e2e . >/dev/null
fi
compose down -v --remove-orphans >/dev/null 2>&1 || true
trap 'compose down -v --remove-orphans >/dev/null 2>&1' EXIT
compose up -d --wait db >/dev/null 2>&1
compose up -d wordpress honey >/dev/null 2>&1
for i in $(seq 1 60); do
  if curl -fsS -o /dev/null -H "Host: localhost:8080" http://127.0.0.1:8080/wp-login.php; then break; fi
  sleep 2
done
compose run --rm cli sh /setup.sh >/dev/null 2>&1
go build -o "$root/bench/load/load" ./bench/load
load() { "$root/bench/load/load" -url http://127.0.0.1:8080 -host localhost:8080 -c "$concurrency" "$@"; }
memory() { docker stats --no-stream --format '{{.MemUsage}}' "$(compose ps -q honey)"; }

pages="/,/hello-world/,/sample-page/,/category/uncategorized/,/2026/"
# Fill the cache
load -d 5s -paths "$pages" >/dev/null

echo "## Cache hits ($concurrency at once, $duration)"
load -d "$duration" -paths "$pages"
echo
echo "## Cache hits, uncompressed"
load -d "$duration" -paths "$pages" -encoding identity
echo
echo "## Logged in visitors: every request goes to WordPress"
load -d "$duration" -paths "$pages" -cookie "wordpress_logged_in_abc=bench"
echo
echo "## Visitors, with 10% searches the cache can't answer"
load -d "$duration" -paths "$pages" -uncached 0.1
echo
echo "## Soak: $soak, purging everything (softly) every 10s"
echo "Honey's memory before: $(memory)"
load -d "$soak" -paths "$pages" -uncached 0.05 -purge-every 10s -secret e2e-secret-0123456789
echo "Honey's memory after: $(memory)"
