#!/bin/sh
# Compares Honey with other ways of caching WordPress, on one WordPress
# (in Docker), with the same load:
#
#   - WordPress on its own (Apache and mod_php)
#   - nginx's FastCGI cache, in front of WordPress on PHP-FPM
#   - WP Super Cache, in its simple mode (PHP serves cached pages)
#   - Honey
#
# for throughput and latency, and while a post is edited every 2 seconds;
# then Lighthouse's page speed scores for a page with a featured image, a
# Google Font, Google Analytics, the Meta pixel and Hotjar, through Honey
# without and with its [optimize] settings.  It writes a Markdown report
# (to $GITHUB_STEP_SUMMARY too, in GitHub Actions).  Needs Docker and Go,
# Node and Chrome for Lighthouse, and wordpress.org for WP Super Cache.
#
#   bench/compare/run.sh
#   DURATION=60s CONCURRENCY=100 bench/compare/run.sh
#   LIGHTHOUSE=0 WP_SUPER_CACHE=0 bench/compare/run.sh   # skip them
set -eu
cd "$(dirname "$0")"
here="$(pwd)"
root="$(cd ../.. && pwd)"
compose() { docker compose -f "$here/docker-compose.yml" "$@"; }
duration="${DURATION:-20s}"
concurrency="${CONCURRENCY:-50}"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"
report() { printf '%s\n' "$*" | tee -a "$summary"; }

mkdir -p work
(cd "$root" && CGO_ENABLED=0 go build -o "$here/work/load" ./bench/load && go run ./bench/compare/hero "$here/work/hero.jpg")
cp page-speed.html work/
if [ -z "${HONEY_COMPARE_NO_BUILD:-}" ]; then
  docker build -q -t honey:compare "$root" >/dev/null
fi
compose down -v --remove-orphans >/dev/null 2>&1 || true
trap 'compose down -v --remove-orphans >/dev/null 2>&1' EXIT
compose up -d --wait db >/dev/null 2>&1
compose up -d wordpress fpm nginx honey >/dev/null 2>&1
for i in $(seq 1 60); do
  if curl -fsS -o /dev/null -H "Host: localhost:8080" http://127.0.0.1:8080/wp-login.php 2>/dev/null; then break; fi
  sleep 2
done
compose run --rm -T cli sh /setup.sh

pages="/,/post-1/,/post-2/,/post-3/,/post-4/,/post-5/,/hello-world/,/page-speed/"
# load sends requests to target (a host in the Docker network)
load() {
  target="$1"
  shift
  compose run --rm -T load -url "http://$target" -host localhost:8080 -c "$concurrency" "$@"
}
# row turns the load generator's output into a table row named $1
row() {
  awk -v name="$1" '
    /^requests/ { rate = $5; sub(/,$/, "", rate); errors = $9 }
    /^latency/ { p50 = $3; p99 = $7; max = $9 }
    /^cache/ { $1 = ""; cache = $0 }
    END { printf "| %s | %s | %s | %s | %s | %s |%s\n", name, rate, p50, p99, max, errors, cache }'
}
# edits edits post-1 every 2 seconds, n times, in the background
edits() {
  compose run --rm -T cli sh -c "id=\$(wp post list --name=post-1 --field=ID); for i in \$(seq 1 $1); do wp post update \$id --post_content='<p>Edited, time '\$i'.</p>' >/dev/null; sleep 2; done" &
}

report "## Throughput and latency"
report ""
report "$concurrency requests at a time for $duration (WordPress on its own: 10s), from inside Docker's network, to 8 pages (and their static files aren't counted)."
report ""
report "| | Requests/s | p50 | p99 | Max | Errors | Cache |"
report "| --- | --- | --- | --- | --- | --- | --- |"
report "$(load wordpress -d 10s -paths "$pages" -cache-header X-None | row 'WordPress, no cache')"
load nginx -d 3s -paths "$pages" >/dev/null
report "$(load nginx -d "$duration" -paths "$pages" -cache-header X-Cache | row "nginx FastCGI cache")"
if [ "${WP_SUPER_CACHE:-1}" = 1 ]; then
  compose run --rm -T cli sh /wp-super-cache.sh >&2
  load wordpress -d 3s -paths "$pages" >/dev/null
  report "$(load wordpress -d "$duration" -paths "$pages" -cache-header X-None | row "WP Super Cache (simple mode)")"
  # Whether its pages really were cached
  tail="$(compose run --rm -T --entrypoint wget load -qO- --header 'Host: localhost:8080' http://wordpress/post-1/ | tail -c 200 | tr '\n' ' ')"
fi
load honey:8080 -d 3s -paths "$pages" >/dev/null
report "$(load honey:8080 -d "$duration" -paths "$pages" | row "Honey")"
report ""
if [ -n "${tail:-}" ]; then
  report "WP Super Cache's pages end: \`$tail\`"
  report ""
fi

report "## While a post is edited every 2 seconds"
report ""
report "The edited post and the home page (which lists it), $concurrency at a time for 30s, while the post is edited 12 times.  WP Super Cache throws its cached copies away, so the next requests wait for WordPress; Honey's plugin purges them softly, so the old ones are served while they're fetched again.  (Stock nginx can't purge, so it isn't here: edits show when its copies expire.)"
report ""
report "| | Requests/s | p50 | p99 | Max | Errors | Cache |"
report "| --- | --- | --- | --- | --- | --- | --- |"
if [ "${WP_SUPER_CACHE:-1}" = 1 ]; then
  edits 12
  report "$(load wordpress -d 30s -paths /,/post-1/ -cache-header X-None | row "WP Super Cache")"
  wait
  compose run --rm -T cli sh /wp-super-cache.sh off >&2
fi
edits 12
report "$(load honey:8080 -d 30s -paths /,/post-1/ | row "Honey")"
wait
report ""

if [ "${LIGHTHOUSE:-1}" != 1 ]; then
  exit 0
fi
chrome="${CHROME_PATH:-$(command -v google-chrome || command -v chromium || command -v chromium-browser || true)}"
page=http://localhost:8080/page-speed/
lighthouse() {
  CHROME_PATH="$chrome" npx -y lighthouse@12.8.2 "$page" --quiet --only-categories=performance \
    --output=json --output-path="$1" --chrome-flags="--headless=new --no-sandbox"
}
# median prints the median of jq expression $2 over the reports $1-*.json
median() {
  for f in work/lh-"$1"-*.json; do jq "$2" "$f"; done | sort -n | awk '{ v[NR] = $1 } END { print v[int((NR + 1) / 2)] }'
}
# scores runs Lighthouse 3 times, and reports the medians as a row
scores() {
  curl -fsS -o /dev/null "$page"
  curl -fsS -o /dev/null "$page"
  for i in 1 2 3; do lighthouse "work/lh-$1-$i.json"; done
  score="$(median "$1" '.categories.performance.score * 100 | round')"
  ms() { median "$1" ".audits[\"$2\"].numericValue | round"; }
  report "| $2 | $score | $(ms "$1" first-contentful-paint) ms | $(ms "$1" largest-contentful-paint) ms | $(ms "$1" total-blocking-time) ms | $(ms "$1" speed-index) ms | $(median "$1" '.audits["cumulative-layout-shift"].numericValue * 1000 | round / 1000') |"
}

report "## Page speed"
report ""
report "Lighthouse 12 (mobile, simulated slow 4G and CPU) on \`/page-speed/\` - a post with a featured image, a Google Font, Google Analytics, the Meta pixel and Hotjar - through Honey, served from its cache; the median of 3 runs."
report ""
report "| | Performance | First Contentful Paint | Largest Contentful Paint | Total Blocking Time | Speed Index | Layout Shift |"
report "| --- | --- | --- | --- | --- | --- | --- |"
scores off "Honey"

# Again with [optimize] on
{ cat honey.toml; printf '\n[optimize]\ndelayScripts = true\npreloadMainImage = true\nselfHostFonts = true\n'; } > work/honey-optimize.toml
HONEY_CONFIG=./work/honey-optimize.toml compose up -d --force-recreate honey >/dev/null 2>&1
for i in $(seq 1 30); do
  if curl -fsS -o /dev/null "$page" 2>/dev/null; then break; fi
  sleep 1
done
# A phone and a desktop report the page's main image
for size in 412,915 1350,940; do
  "$chrome" --headless=new --no-sandbox --window-size="$size" --virtual-time-budget=10000 --dump-dom "$page" >/dev/null 2>&1 || true
done
learned=no
for i in $(seq 1 30); do
  if curl -fsS "$page" | grep -q data-honey-main-image; then learned=yes; break; fi
  sleep 1
done
scores on "Honey, with [optimize]: delayScripts, preloadMainImage, selfHostFonts"
report ""
report "The main image was learned before Lighthouse ran: $learned."
