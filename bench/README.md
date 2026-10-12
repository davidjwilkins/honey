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
