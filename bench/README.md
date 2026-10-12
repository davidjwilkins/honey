# Benchmarks

Two kinds:

- **Go benchmarks** of a cache hit through the whole of Honey (client
  address, security, metrics and the cache), in-process, so that they
  measure Honey alone:

      go test ./cmd/honey -run '^$' -bench BenchmarkHit -benchmem -cpu 1

  (With more than one CPU, the time per request is shared between them.)

  and of putting together a page which uses [Edge Side
  Includes](../README.md#edge-side-includes):

      go test ./fetch -run '^$' -bench BenchmarkESIPage

- **[`run.sh`](run.sh)**, which runs Honey in front of a real WordPress (the
  end-to-end tests' Docker setup) and sends it requests with
  [`load`](load/main.go): cache hits, logged in visitors (whose requests all
  go to WordPress), a mix with searches the cache can't answer, and a soak
  test with purges, reporting throughput, latency and Honey's memory.

      bench/run.sh
      DURATION=60s SOAK=10m CONCURRENCY=100 bench/run.sh

  Docker's port forwarding limits the number of requests a second it can
  measure, so compare its results with each other rather than with other
  servers.  `load` can be pointed at any Honey (`go run ./bench/load -h`).

- **[`compare/run.sh`](compare/run.sh)**, which serves one WordPress four
  ways - on its own (Apache and mod_php), behind nginx's FastCGI cache (with
  PHP-FPM), with WP Super Cache, and behind Honey - and sends each the same
  requests from inside Docker's network, then measures a page's speed in
  Chrome with Lighthouse, through Honey with and without
  [`[optimize]`](../README.md#optimizing-pages).  It runs in GitHub Actions
  when it changes (see [`compare.yml`](../.github/workflows/compare.yml)),
  and puts its results in the run's summary.

      bench/compare/run.sh
      DURATION=60s CONCURRENCY=100 bench/compare/run.sh

## Results

On a 4 vCPU (2.1GHz Xeon) virtual machine, shared with the load generator,
in October 2026.  "Before" is master before this work (`f95b264`).

| | Before | After |
| --- | --- | --- |
| A cache hit, in-process, CPU time (`-cpu 1`, browser request) | 23.7µs, 90 allocations, 8.1KB | 11.6µs, 47 allocations, 3.6KB |
| Cache hits over HTTP, 50 at once (not in Docker) | 22,500/s, p50 1.4ms | 27,900/s, p50 1.15ms |
| A WordPress post with ESI comments, gzip/br\* | 993/s, p50 28ms | 3,831/s, p50 7.6ms |
| The same post, uncompressed\* | 2,494/s | 4,163/s |
| The same page for logged in users (WordPress itself) | 41-56/s, p50 0.8-1.1s | (unchanged) |
| Memory after a soak with purges every 10s (256MB cache)\*\* | 910MiB | 398MiB |

\* Before the ESI changes, but after the others.  \*\* 2 minutes before, 3 after (with more traffic).

What changed:

- **Requests aren't copied for ESI** unless their page uses it (it was twice
  for every request).
- **Cached headers are shared** with each response rather than copied, and
  the headers of compressed copies are made once per cached page.
- **The security checks avoid regular expressions** for WordPress's usual
  addresses, which have no file names.
- **`X-Real-IP` is set as requests go to WordPress**, rather than on every
  request (which made Go grow the request's headers on every hit).
- **Pages which use ESI are parsed once**, and **gzipped from parts
  compressed once**: only their includes are compressed for each visitor,
  instead of the whole page with brotli (5ms for a typical post).
- **Go's garbage collector has a memory limit** to suit the cache's size
  (see [Memory](../README.md#memory)).

## Against other caches

From [`compare/run.sh`](compare/run.sh) on a GitHub Actions runner (4 vCPUs,
shared by WordPress, the caches and the load generator), in October 2026:
50 requests at a time, for 20 seconds, to 8 pages of a WordPress 7.0 site
with Twenty Twenty-Five, asking for compressed responses as browsers do.

| | Requests/s | p50 | p99 |
| --- | --- | --- | --- |
| WordPress, no cache | 47 | 815ms | 7.2s |
| nginx's FastCGI cache (gzip) | 1,846 | 28ms | 47ms |
| WP Super Cache 3.1.4 (simple mode) | 1,287 | 36ms | 108ms |
| Honey | 19,858 | 1.9ms | 11ms |

While a post is edited every 2 seconds (requests for it and the home page,
which lists it, for 30 seconds):

| | Requests/s | p50 | p99 | Slowest |
| --- | --- | --- | --- | --- |
| WP Super Cache | 746 | 40ms | 1.18s | 3.5s |
| Honey | 12,023 | 2.8ms | 22ms | 141ms |

WP Super Cache throws its copies of an edited post away, so the requests
which come next wait for WordPress; Honey's plugin purges them softly, so
the old copy is served while the new one is fetched.  Stock nginx can't
purge, so edits only show when its copies expire.

Runners vary: across five runs, Honey served 8,800-24,000 requests a
second and nginx 1,800-3,300, with Honey 5-8 times nginx's throughput
each time (WP Super Cache's figure is from the one run after its setup
was fixed).  Much
of the difference is compression: Honey keeps compressed copies, while
nginx gzips every response.  Without compression nginx is faster - on a
4 vCPU virtual machine, 12,200 requests a second against Honey's 9,700 -
but browsers always ask for it.

Lighthouse 12 (a phone, with simulated slow 4G and CPU), on a post with a
featured image, a Google Font, Google Analytics, the Meta pixel and
Hotjar, served from Honey's cache; the median of 3 runs:

| | Performance | First Contentful Paint | Largest Contentful Paint | Total Blocking Time |
| --- | --- | --- | --- | --- |
| Honey | 94-98 | 1.35s | 1.80s | 113-275ms |
| Honey, with `delayScripts`, `preloadMainImage` and `selfHostFonts` | 99 | 1.35s | 1.80-2.10s | 0ms |

Delaying the third-party scripts takes Total Blocking Time to nothing.
The Largest Contentful Paint was 150-300ms later with `[optimize]` in
four runs out of five - probably because the self-hosted font now loads
alongside the image, where from Google it loaded later, though that isn't
confirmed.  The page's main image hadn't been learned when
Lighthouse ran, so it isn't preloaded; on this page (an `<img>` the browser
finds straight away) it would only be given `fetchpriority="high"`.
