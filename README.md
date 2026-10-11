# honey

[![CI](https://github.com/davidjwilkins/honey/actions/workflows/ci.yml/badge.svg)](https://github.com/davidjwilkins/honey/actions/workflows/ci.yml)

Honey is an http cache and proxy.

In the event of a cache miss, It multiplexes requests to the same URL into a single request, and once the response has been received, writes it to all requesters, and adds it to the cache.

Cached responses are served until they expire, according to the backend's `Cache-Control` (`s-maxage`, `max-age`) or `Expires` headers, or a configurable default TTL if it sends neither.  The cache is bounded in size, evicting the least recently used responses when it is full.

If the backend sends `stale-while-revalidate`, expired responses are served immediately while they are refreshed in the background.  If it sends `stale-if-error`, expired responses are served (with a `Warning` header) when the backend errors or can't be reached.

It will set an Etag on responses, and respond with an HTTP 304 Not Modified in the event that the `If-None-Match` header matches the Etag.

Static files (images, css, js, fonts, media, documents) are cached like any other response.  Responses larger than a configurable size (10MB by default) are streamed straight to the client instead of being cached.  Range requests (e.g. seeking in a video) are served from the cache if the file is cached, and otherwise sent to the backend.

Compressible responses (html, css, js, json, svg...) are served brotli compressed to clients which accept it, gzip compressed to clients which only accept gzip, and unencoded to the rest, all from the same cache entry (the client's `Accept-Encoding` q-values decide between them).  The brotli copy is made quickly (quality 6) when a response is first cached, so that the first requester isn't kept waiting, and then remade at maximum quality (11) in the background once it has been served from the cache (so pages requested only once never pay for it).  Responses with `Cache-Control: no-transform` aren't compressed.  The backend is asked for gzip (which Honey decodes) rather than the client's `Accept-Encoding`, so that the cache holds the unencoded response.

If will not cache responses that contain the `no-store` Cache-Control directive

It will fetch fresh resources if the `Cache-Control: no-cache` directive, or `Pragma: no-cache`, is set in a request from a [trusted caller](#purging-and-refreshing).  From anyone else, these are ignored (RFC 9111 lets a cache do this), so that they can't be used to send every request to the backend.

Every response has an `X-Honey-Cache` header saying how it was served: `HIT`, `MISS`, `MISS (MULTIPLEXED)`, `STALE` or `NO-CACHE`.

## Installing

- **Binaries:** download one for Linux, macOS or Windows from the [releases](https://github.com/davidjwilkins/honey/releases), each with this README, an example config and a systemd service.  Check it against `checksums.txt`.
- **Docker:** `docker build -t honey .` builds an 18MB image (a static binary on [distroless](https://github.com/GoogleContainerTools/distroless), running as a non-root user), which reads its config from `/etc/honey/honey.toml`:

		docker run -d -p 8080:8080 -v "$PWD/honey.toml:/etc/honey/honey.toml:ro" honey

- **From source:** `go install github.com/davidjwilkins/honey/cmd/honey@latest`
- **systemd:** [`contrib/honey.service`](contrib/honey.service) runs Honey as an unprivileged, sandboxed service, reading `/etc/honey/honey.toml`.  Instructions are at the top of the file.

`honey -version` prints the version.  Pushing a `v*` tag builds and publishes a release.

## Running it

	honey -config honey.toml

A minimal config just needs a backend:

	[backend]
	uri = "https://www.example.com"

All the available settings:

	listen = ":8080"             # address to listen on

	[backend]
	uri = "https://www.example.com"
	timeout = "30s"              # how long to wait for the backend to start responding ("0" for no limit)
	stallTimeout = "30s"         # how long the backend may go silent partway through a response ("0" for no limit)

	[cache]
	maxSize = "256MB"            # memory for cached responses (KB, MB, GB)
	maxObjectSize = "10MB"       # larger responses are streamed to the client, not cached
	defaultTTL = "5m"            # freshness for responses without max-age, s-maxage or Expires
	staleIfError = "1h"          # serve expired responses this long if the backend errors or times out
	                             # (unless they have their own stale-if-error; off by default)
	staticFiles = true           # cache images, css, js, fonts, media and documents
	brotli = true                # brotli compress html, css, js, json, svg... for clients that accept it
	gzip = true                  # gzip them for clients that accept gzip but not brotli
	compressMinSize = "1KB"      # smaller responses aren't compressed
	allowedCookies = ["site_lang_id"]  # Set-Cookie headers allowed through the cache
	queryParams = ["p", "s", "ver"]    # the only query parameters which matter (unset: all of them)

	# Rules for some requests. match is a path prefix, or with regex = true,
	# a regular expression matched against the path and query string.
	[[route]]
	match = "/wp-admin"
	cache = false                # don't cache these

	[[route]]
	match = "/wp-content/uploads/"
	defaultTTL = "7d"            # override cache settings for these:
	# staleIfError = "0"         #   defaultTTL, staleIfError and queryParams
	# queryParams = ["ver"]

	# Log each request to stdout: method, uri, status, bytes, duration_ms,
	# cache (the X-Honey-Cache result), remote and forwarded_for.
	[log]
	access = true                # off by default
	format = "text"              # text|json

	# Serve Prometheus metrics at http://127.0.0.1:9090/metrics (off unless set).
	# Use a separate, private address: they shouldn't be public.
	[metrics]
	listen = "127.0.0.1:9090"

	# Who may purge the cache, and refresh it with no-cache.  Nobody is
	# trusted unless they are listed here.
	[control]
	allowIPs = ["10.0.0.0/8"]    # IP addresses or networks
	secret = "a long random string"   # or send this in the secretHeader header
	secretHeader = "X-Honey-Secret"

Honey refuses to start if the config has settings it doesn't support.  See [`config/wordpress.toml`](config/wordpress.toml) for an example WordPress setup.

## Routes

Each `[[route]]` either stops the requests it matches from being cached (`cache = false`), or overrides some of the `[cache]` settings for them: `defaultTTL`, `staleIfError` (`"0"` turns it off) and `queryParams`.

- `cache = false` applies if any matching route says so, wherever it is in the list.
- Otherwise the first matching route's settings apply, so list more specific routes first.
- Routes are matched against the request as the client sent it, before Honey removes query parameters.

Durations can be given in days, e.g. `"7d"`, as well as e.g. `"90s"` or `"1h30m"`.

## Query parameters

By default every query parameter is part of the cache key, so `/page?nocache=123` is a different page to `/page`, and goes to the backend.  That lets anyone get around the cache, and tracking parameters like `utm_source` and `fbclid` split it.

Setting `queryParams` lists the parameters which matter.  Any others are removed from cacheable requests before they are looked up in the cache or sent to the backend, so the backend never sees them, and the cache can't serve the wrong content because of them.  Parameters are also sorted, so their order doesn't matter.  Requests which aren't cached (e.g. those matching a `[[route]]` with `cache = false`) are left alone.

For WordPress, core uses `p`, `page_id`, `s`, `paged`, `cat`, `tag`, `author`, `m`, `year`, `monthnum`, `day`, `post_type`, `cpage`, `replytocom`, `attachment_id` and `ver` (on css and js, so that an upgrade isn't served old cached files) - but check which ones your plugins use, as their parameters will stop working if they aren't listed.

## Metrics

With `[metrics] listen` set, `/metrics` on that address has, in the Prometheus text format:

- `honey_requests_total{cache="hit|miss|multiplexed|stale|bypass|none"}`: requests, by how the cache handled them (`none` is e.g. purges and errors)
- `honey_responses_total{code="2xx|..."}`: responses, by status class
- `honey_backend_requests_total{code="2xx|...|error"}`, `honey_backend_response_seconds` (histogram) and `honey_backend_requests_in_flight`: requests to the backend, and how long it took to start responding
- `honey_cache_entries`, `honey_cache_bytes` and `honey_cache_max_bytes`: how full the cache is

The hit ratio is `rate(honey_requests_total{cache=~"hit|multiplexed|stale"}[5m]) / rate(honey_requests_total[5m])`.

## Purging and refreshing

Trusted callers (see `[control]` above) can remove pages from the cache with the `PURGE` method, which is never sent to the backend:

	# one page, every variant of it (encodings, Vary values, GET and HEAD)
	curl -X PURGE -H "X-Honey-Secret: $SECRET" https://www.example.com/2018/02/my-post/
	# everything under a path
	curl -X PURGE -H "X-Honey-Secret: $SECRET" "https://www.example.com/category/news/*"
	# everything
	curl -X PURGE -H "X-Honey-Secret: $SECRET" "https://www.example.com/*"

They can also make Honey fetch a fresh copy, and cache it, by sending `Cache-Control: no-cache`.  Untrusted `PURGE` requests get a `403 Forbidden`.

For example, to purge posts from WordPress when they're updated, put this in a must-use plugin (e.g. `wp-content/mu-plugins/honey.php`):

	<?php
	add_action('save_post', function ($post_id) {
		foreach ([get_permalink($post_id), home_url('/')] as $url) {
			wp_remote_request($url, [
				'method' => 'PURGE',
				'headers' => ['X-Honey-Secret' => HONEY_SECRET], // define() this in wp-config.php
			]);
		}
	});

If Honey is behind another proxy on the same host (e.g. Caddy or nginx for TLS), every request comes from that proxy's address, so don't put it in `allowIPs`: use the secret instead.

## Using it as a library

	backend, err := url.Parse("https://www.example.com")
	if err != nil {
		panic(err)
	}

	// NewDefaultCacher skips the WordPress admin, login, feed and previews;
	// cache.NewCacher(cache.Options{...}) has no site-specific rules.
	cacher := cache.NewDefaultCacher()
	// adding site_lang_id cookie to the default
	// cacher will allow it through the cache
	cacher.AddAllowedCookie("site_lang_id")
	// fetch.FetchWithControl takes a fetch.Control saying who may purge
	// the cache; fetch.Fetch trusts nobody.
	fetcher := fetch.Fetch(cacher, fetch.Forwarder(cacher), backend)
	http.ListenAndServe(":8080", fetcher)


### Todo

- [x] Handle `stale-if-error`
	- [x] Add unit tests
	- [x] Serve stale content when the backend can't be reached at all
	- [ ] Configurable Site-wide (whether to respect it if present, or whether to always act as if this header were present)
	- [ ] Configurable Per route
	- [x] Send cached response with a [`Warning`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Warning) header if the backend gives an error after clearing the cache. 

- [ ] Implement configuration via TOML file (honey.toml?)
	- [x] Backend, listen address, cache size, default TTL, allowed cookies, uncached routes
	- [ ] The planned settings in `config/wordpress.toml`

- [ ] Come up with a way to mark certain routes/files as [`immutable`](https://hacks.mozilla.org/2017/01/using-immutable-caching-to-speed-up-the-web/)

- [ ] Web UI to configure / clear cache and view metrics

- [ ] Minify html, js, css before cacheing
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] [Canonicalize](https://www.modpagespeed.com/doc/filter-canonicalize-js#sample)  popular JavaScript libraries that can be replaced with ones hosted for free by a JavaScript library hosting service
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Use http/2 push to push assets if request doesn't have an If-None-Match header
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Rewrite static assets to cookieless subdomain

- [ ] Combine all google-font requests into a single one

- [ ] Implement other cache backends
	- [x] In Memory (size-bounded LRU)
	- [ ] File
	- [ ] Memcached
	- [ ] Redis
	- [ ] BoltDB

	- [x] Brotli compress if requester supports it
	- [x] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)
		- [x] Site wide
		- [ ] Per route

- [ ] Implement [offline cache](https://developers.google.com/web/fundamentals/instant-and-offline/offline-cookbook/)
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Automatically fix mixed-content https issues
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Prevent hotlinking of images
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Letsencypt SSL termination

- [x] Set [`Last-Modified`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Last-Modified) header on response to the cached time if it is not already on the backend response.
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] Handle [`If-Modified-Since`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/If-Modified-Since]) and [`If-Unmodified-Since`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/If-Unmodified-Since)

- [x] If cache miss, but after refresh Etag matches, send 304 Response

- [x] Handle `only-if-cached` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control) 

- [x] Validate response or send to backend if `must-revalidate` or `proxy-revalidate` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control)
	- [x] Only let trusted callers (by IP or secret header) refresh or purge the cache
	- [ ] Configurable Per route

- [X] Add the `public` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control) unless `private` is received from backend.
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] Add [`Expires`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Expires) header to responses if not in response from backend.
	- [x] Configurable Cache TTL
	- [ ] Configurable whether to overwrite backend Expires
	- [ ] Configure whether to serve stale content while refreshing, or multiplex requests into a single request and serve new content to all of them
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] Add ability to configure which headers to include in the Request hash (e.g. Accept-Language)
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] If cache miss, but after refresh Validate matches, send 304 Response

- [x] Check [`Vary`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Vary) header from response and handle properly

- [x] Handle `stale-while-revalidate`
	- [ ] Configurable Site-wide (whether to respect it)
	- [ ] Configurable Per route

- [x] After fetching a route with multiplexer, check vary headers, bucket queued requests based on the header,
	  and do a fetch for each variant.