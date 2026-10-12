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
mkdir -p work
: > work/report.md
# report writes a line of the report (to work/report.md too)
report() { printf '%s\n' "$*" | tee -a "$summary" work/report.md; }

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
    /^requests/ { rate = $5; sub(/,$/, "", rate); errors = $8 }
    # Error statuses count as errors too
    /^statuses/ { for (i = 2; i < NF; i += 2) { code = $i; n = $(i + 1); sub(/:$/, "", code); sub(/,$/, "", n); if (code + 0 >= 400) { errors += n; bad = bad " " code } } }
    /^latency/ { p50 = $3; p99 = $7; max = $9 }
    /^cache/ { $1 = ""; cache = $0 }
    END { if (bad != "") errors = errors " (" substr(bad, 2) ")"; printf "| %s | %s | %s | %s | %s | %s |%s\n", name, rate, p50, p99, max, errors, cache }'
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
  report "$(load wordpress -d "$duration" -paths "$pages" -cache-header X-WP-Super-Cache | row "WP Super Cache (simple mode)")"
  # Whether its pages really were served from its cache: its headers, its
  # cache files and settings, and Apache's log
  compose run --rm -T --entrypoint sh load -c "wget -S -O /tmp/page --header 'Host: localhost:8080' --header 'Accept: text/html' http://wordpress/post-1/ 2>&1 | grep -i 'HTTP/\|cache\|encoding'; tail -c 200 /tmp/page" > work/wpsc-check.txt 2>&1 || true
  compose run --rm -T cli sh -c "ls -R wp-content/cache/supercache | head -12; grep -E '^\\\$(cache_enabled|super_cache_enabled|wp_cache_mod_rewrite|cache_compression|wp_cache_slash_check|wpsc_served_header|cache_path)' wp-content/wp-cache-config.php" >> work/wpsc-check.txt 2>&1 || true
  compose logs --no-color --no-log-prefix --tail 5 wordpress >> work/wpsc-check.txt 2>&1 || true
  tail="see the details below"
fi
load honey:8080 -d 3s -paths "$pages" >/dev/null
report "$(load honey:8080 -d "$duration" -paths "$pages" | row "Honey")"
report ""
if [ -n "${tail:-}" ]; then
  report "<details><summary>WP Super Cache's response to /post-1/, cache files and settings</summary>"
  report ""
  report '```'
  report "$(cat work/wpsc-check.txt)"
  report '```'
  report "</details>"
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
  report "$(load wordpress -d 30s -paths /,/post-1/ -cache-header X-WP-Super-Cache | row "WP Super Cache")"
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
  # (LIGHTHOUSE_FLAGS, e.g. for throttling the browser itself rather than
  # simulating it)
  CHROME_PATH="$chrome" npx -y lighthouse@12.8.2 "$page" --quiet --only-categories=performance \
    --output=json --output-path="$1" --chrome-flags="--headless=new --no-sandbox" ${LIGHTHOUSE_FLAGS:-}
}
# median prints the median of jq expression $2 over the reports $1-*.json
median() {
  for f in work/lh-"$1"-*.json; do jq "$2" "$f"; done | sort -n | awk '{ v[NR] = $1 } END { print v[int((NR + 1) / 2)] }'
}
# scores runs Lighthouse 3 times, and reports the medians as a row, and
# what made up the median run's Largest Contentful Paint
scores() {
  curl -fsS -o /dev/null "$page"
  curl -fsS -o /dev/null "$page"
  for i in 1 2 3; do lighthouse "work/lh-$1-$i.json"; done
  score="$(median "$1" '.categories.performance.score * 100 | round')"
  ms() { median "$1" ".audits[\"$2\"].numericValue | round"; }
  report "| $2 | $score | $(ms "$1" first-contentful-paint) ms | $(ms "$1" largest-contentful-paint) ms | $(ms "$1" total-blocking-time) ms | $(ms "$1" speed-index) ms | $(median "$1" '.audits["cumulative-layout-shift"].numericValue * 1000 | round / 1000') |"
  lcp_breakdown "$1" "$2" >> work/lcp.md
}
# lcp_breakdown describes the Largest Contentful Paint of the median of
# runs $1 (labelled $2): its phases, and the page's fonts, images and
# stylesheets, as fetched (unthrottled, so for their sizes, priorities and
# order rather than their times)
lcp_breakdown() {
  f="$(for f in work/lh-"$1"-*.json; do printf '%s %s\n' "$(jq '.audits["largest-contentful-paint"].numericValue' "$f")" "$f"; done |
    sort -n | awk '{ v[NR] = $2 } END { print v[int((NR + 1) / 2)] }')"
  printf '**%s**: LCP %s ms (%s)\n\n' "$2" "$(jq '.audits["largest-contentful-paint"].numericValue | round' "$f")" \
    "$(jq -r '[.audits["largest-contentful-paint-element"].details.items[]? | select(.type == "table") | .items[]? | select(.phase != null) | "\(.phase) \(.timing | round) ms"] | join(", ")' "$f")"
  jq -r '.audits["network-requests"].details.items[] | select(.resourceType == "Font" or .resourceType == "Image" or .resourceType == "Stylesheet") |
    "- \(.resourceType) `\(.url | split("?")[0] | split("/")[-1])`: \(.transferSize) bytes, \(.priority) priority, \(.networkRequestTime | round)-\(.networkEndTime | round) ms"' "$f"
  printf '\n'
}

report "## Page speed"
report ""
report "Lighthouse 12 (mobile, simulated slow 4G and CPU) on \`/page-speed/\` - a post with a featured image, a Google Font, Google Analytics, the Meta pixel and Hotjar - through Honey, served from its cache; the median of 3 runs."
report ""
report "| | Performance | First Contentful Paint | Largest Contentful Paint | Total Blocking Time | Speed Index | Layout Shift |"
report "| --- | --- | --- | --- | --- | --- | --- |"
: > work/lcp.md
scores off "Honey"

# optimized reports the scores with [optimize] settings $2 (TOML lines), as
# $1 (labelled $3), once Honey has learned the page's main image (and
# fetched its stylesheets, and Google's fonts)
optimized() {
  { cat honey.toml; printf '\n[optimize]\n%s\n' "$2"; } > work/honey-optimize.toml
  HONEY_CONFIG=./work/honey-optimize.toml compose up -d --force-recreate honey >/dev/null 2>&1
  for i in $(seq 1 30); do
    if curl -fsS -o /dev/null "$page" 2>/dev/null; then break; fi
    sleep 1
  done
  # A phone and a desktop report the page's main image
  for size in 412,915 1350,940; do
    timeout 60 "$chrome" --headless=new --no-sandbox --window-size="$size" --virtual-time-budget=10000 --dump-dom "$page" >/dev/null 2>&1 || true
  done
  learned=no
  for i in $(seq 1 20); do
    # (The script which reports it goes, once it's learned)
    if ! curl -fsS "$page" 2>/dev/null | grep -q 'id="honey-main-image"'; then learned=yes; break; fi
    sleep 1
  done
  case "$2" in *removeUnusedCSS*)
    for i in $(seq 1 20); do
      if curl -fsS "$page" 2>/dev/null | grep -q data-honey-used-css; then break; fi
      sleep 1
    done
  esac
  case "$2" in *convertImages*)
    # Every size of the featured image, converted (as a browser which
    # takes AVIF asks for it)
    for image in $(curl -fsS "$page" 2>/dev/null | grep -o 'http://localhost:8080/wp-content/uploads/[^" ,]*\.jpg' | sort -u); do
      for i in $(seq 1 30); do
        type="$(curl -fsS -o /dev/null -w '%{content_type}' -H 'Accept: image/avif,image/webp,*/*' "$image" 2>/dev/null || true)"
        if [ "$type" = image/avif ]; then break; fi
        sleep 1
      done
      printf '%s\n' "- \`$(basename "$image")\`: $(curl -fsS -o /dev/null -w '%{size_download}' "$image") bytes as a JPEG, $(curl -fsS -o /dev/null -w '%{size_download}' -H 'Accept: image/avif' "$image") as $type" >> work/images.md
    done
  esac
  scores "$1" "$3"
}
all="delayScripts = true
preloadMainImage = true
selfHostFonts = true"
: > work/images.md
optimized on "$all" "Honey, with [optimize]: delayScripts, preloadMainImage, selfHostFonts"
learned_on=$learned
all="$all
removeUnusedCSS = true"
optimized css "$all" "... and removeUnusedCSS"
all="$all
convertImages = true"
optimized images "$all" "... and convertImages"
report ""
report "The main image was learned before Lighthouse ran: $learned_on, then $learned.  The featured image, converted:"
report ""
report "$(cat work/images.md)"
report ""

# With Chrome throttled for real, which is closer to what a phone sees:
# Lighthouse's simulation counts whatever started before the main image was
# shown, in a fast load, as holding it up
report "### With Chrome throttled for real, rather than simulated"
report ""
report "| | Performance | First Contentful Paint | Largest Contentful Paint | Total Blocking Time | Speed Index | Layout Shift |"
report "| --- | --- | --- | --- | --- | --- | --- |"
LIGHTHOUSE_FLAGS=--throttling-method=devtools
optimized devtools-off "" "Honey, throttled by Chrome"
optimized devtools-on "$all" "Honey, with all of [optimize], throttled by Chrome"
LIGHTHOUSE_FLAGS=
report ""

# The same page with a classic theme, whose stylesheets are linked (block
# themes like Twenty Twenty-Five put most of their CSS in the page itself,
# where removeUnusedCSS leaves it), to show what removing unused CSS does
theme="${CLASSIC_THEME:-oceanwp}"
for attempt in 1 2 3 4; do
  if compose run --rm -T cli wp theme install "$theme" --activate >/dev/null 2>&1; then break; fi
  if [ "$attempt" = 4 ]; then echo "couldn't install $theme" >&2; exit 1; fi
  sleep $((attempt * 5))
done
report "### With a classic theme ($theme $(compose run --rm -T cli wp theme get "$theme" --field=version 2>/dev/null))"
report ""
report "| | Performance | First Contentful Paint | Largest Contentful Paint | Total Blocking Time | Speed Index | Layout Shift |"
report "| --- | --- | --- | --- | --- | --- | --- |"
optimized classic-off "" "Honey"
optimized classic-on "$all" "Honey, with all of [optimize]"
report ""
report "The CSS the page uses, of each stylesheet it links:"
report ""
report "$(python3 - "$page" <<'PY'
import html, re, sys, urllib.parse, urllib.request
page = sys.argv[1]
body = urllib.request.urlopen(page).read().decode()
for href, used in re.findall(r'<style data-honey-used-css="([^"]*)">(.*?)</style>', body, re.S):
    href = html.unescape(href)
    whole = len(urllib.request.urlopen(urllib.request.Request(urllib.parse.urljoin(page, href))).read())
    print(f"- `{href.split('?')[0].split('/')[-1]}`: {len(used.encode()):,} of {whole:,} bytes ({100 * (1 - len(used.encode()) / whole):.0f}% left out)")
PY
)"
report ""
report "<details><summary>What made up each Largest Contentful Paint (the median run's), and the fonts, images and stylesheets fetched</summary>"
report ""
report "$(cat work/lcp.md)"
report "</details>"
