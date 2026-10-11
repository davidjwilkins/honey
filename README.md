# honey

[![CI](https://github.com/davidjwilkins/honey/actions/workflows/ci.yml/badge.svg)](https://github.com/davidjwilkins/honey/actions/workflows/ci.yml)

Honey is an http cache and proxy.

In the event of a cache miss, It multiplexes requests to the same URL into a single request, and once the response has been received, writes it to all requesters, and adds it to the cache.

Cached responses are served until they expire, according to the backend's `Cache-Control` (`s-maxage`, `max-age`) or `Expires` headers, or a configurable default TTL if it sends neither.  The cache is bounded in size, evicting the least recently used responses when it is full.

If the backend sends `stale-while-revalidate`, expired responses are served immediately while they are refreshed in the background.  If it sends `stale-if-error`, expired responses are served (with a `Warning` header) when the backend errors or can't be reached.

It will set an Etag on responses, and respond with an HTTP 304 Not Modified in the event that the `If-None-Match` header matches the Etag.

Static files (images, css, js, fonts, media, documents) are cached like any other response.  Responses larger than a configurable size (10MB by default) are streamed straight to the client instead of being cached.  Range requests (e.g. seeking in a video) are served from the cache if the file is cached, and otherwise sent to the backend.

Compressible responses (html, css, js, json, svg...) are served brotli compressed to clients which accept it, gzip compressed to clients which only accept gzip, and unencoded to the rest, all from the same cache entry (the client's `Accept-Encoding` q-values decide between them).  The brotli copy is made quickly (quality 6) when a response is first cached, so that the first requester isn't kept waiting, and then remade at maximum quality (11) in the background once it has been served from the cache (so pages requested only once never pay for it).  Responses with `Cache-Control: no-transform` aren't compressed.  The backend is asked for gzip (which Honey decodes) rather than the client's `Accept-Encoding`, so that the cache holds the unencoded response.

It won't cache responses marked `no-store` or `private`, or pages for logged in users and others whose pages may be personal (see [Logged in users and sessions](#logged-in-users-and-sessions)).  Requests with an `Authorization` header aren't cached either.

It will fetch fresh resources if the `Cache-Control: no-cache` directive, or `Pragma: no-cache`, is set in a request from a [trusted caller](#purging-and-refreshing).  From anyone else, these are ignored (RFC 9111 lets a cache do this), so that they can't be used to send every request to the backend.

Every response has an `X-Honey-Cache` header saying how it was served: `HIT`, `MISS`, `MISS (MULTIPLEXED)`, `STALE` or `NO-CACHE`.

## Installing

- **Binaries:** download one for Linux, macOS or Windows from the [releases](https://github.com/davidjwilkins/honey/releases), each with this README, an example config, a systemd service and the [WordPress purge plugin](#purging-from-wordpress).  Check it against `checksums.txt`.
- **Docker:** `docker build -t honey .` builds an 18MB image (a static binary on [distroless](https://github.com/GoogleContainerTools/distroless), running as a non-root user), which reads its config from `/etc/honey/honey.toml`:

		docker run -d -p 8080:8080 -v "$PWD/honey.toml:/etc/honey/honey.toml:ro" honey

- **From source:** `go install github.com/davidjwilkins/honey/cmd/honey@latest`
- **systemd:** [`contrib/honey.service`](contrib/honey.service) runs Honey as an unprivileged, sandboxed service, reading `/etc/honey/honey.toml`.  Instructions are at the top of the file.

`honey -version` prints the version.  Pushing a `v*` tag builds and publishes a release.

## Running it

	honey -config honey.toml

A minimal config just needs a backend - the web server running WordPress, which Honey sits in front of:

	[backend]
	uri = "http://127.0.0.1:8081"

All the available settings:

	listen = ":8080"             # address to listen on

	[backend]
	uri = "http://127.0.0.1:8081"
	host = "www.example.com"     # the Host header to send it (unset: the one the client sent)
	timeout = "30s"              # how long to wait for the backend to start responding ("0" for no limit)
	stallTimeout = "30s"         # how long the backend may go silent partway through a response ("0" for no limit)

	[cache]
	maxSize = "256MB"            # memory for cached responses (KB, MB, GB)
	maxObjectSize = "10MB"       # larger responses are streamed to the client, not cached
	defaultTTL = "5m"            # freshness for responses without max-age, s-maxage or Expires
	staleIfError = "1h"          # serve expired responses this long if the backend errors or times out
	                             # (unless they have their own stale-if-error; off by default)
	persist = "/var/lib/honey/cache.snapshot"  # save the cache here, and load it at startup (off unless set)
	persistInterval = "5m"       # how often to save it ("0": only on shutdown)
	staticFiles = true           # cache images, css, js, fonts, media and documents
	brotli = true                # brotli compress html, css, js, json, svg... for clients that accept it
	gzip = true                  # gzip them for clients that accept gzip but not brotli
	compressMinSize = "1KB"      # smaller responses aren't compressed
	allowedCookies = ["site_lang_id"]  # Set-Cookie headers allowed through the cache
	bypassCookies = ["wordpress_logged_in_", "wp-postpass_", "comment_author_",
	    "woocommerce_items_in_cart", "wp_woocommerce_session_"]  # (the default) see below
	browserCacheControl = "no-cache"   # Cache-Control for browsers (unset: the cached response's own)
	queryParams = ["p", "s", "ver"]    # the only query parameters which matter (unset: all of them)

	# Rules for some requests. match is a path prefix, or with regex = true,
	# a regular expression matched against the path and query string.
	[[route]]
	match = "/wp-admin"
	cache = false                # don't cache these

	[[route]]
	match = "/wp-content/uploads/"
	defaultTTL = "7d"            # override cache settings for these: defaultTTL, staleIfError,
	# staleIfError = "0"         #   queryParams, bypassCookies and browserCacheControl
	# queryParams = ["ver"]
	# bypassCookies = []
	# browserCacheControl = "public, max-age=604800"   # or "inherit"

	# Log each request to stdout: method, uri, status, bytes, duration_ms,
	# cache (the X-Honey-Cache result), remote and forwarded_for.
	[log]
	access = true                # off by default
	format = "text"              # text|json

	# Serve the admin page at http://127.0.0.1:9090/ (off unless set), and
	# Prometheus metrics at /metrics.  Use a separate, private address.
	[admin]
	listen = "127.0.0.1:9090"

	# Or just the metrics, at http://127.0.0.1:9091/metrics (off unless set)
	[metrics]
	listen = "127.0.0.1:9091"

	# Who may purge the cache, and refresh it with no-cache.  Nobody is
	# trusted unless they are listed here.
	[control]
	allowIPs = ["10.0.0.0/8"]    # IP addresses or networks
	secret = "a long random string"   # or send this in the secretHeader header
	secretHeader = "X-Honey-Secret"

Honey refuses to start if the config has settings it doesn't support.  See [`config/wordpress.toml`](config/wordpress.toml) for an example WordPress setup.

## Hosts

Honey passes the backend the `Host` header clients send (e.g. `www.example.com`), as WordPress redirects requests for any other host to its own address.  The backend also gets `X-Forwarded-Host` (the same host, replacing any the client sent) and `X-Forwarded-Proto` (`http` or `https`, unless a proxy in front of Honey has already set it).

Responses are cached separately for each host and protocol, so e.g. a redirect from `example.com` to `www.example.com` is only served for `example.com`.

To proxy a site hosted elsewhere, whose server only answers to its own name, set `backend.host` to it: it is sent as the `Host` header instead.

## Keeping the cache across restarts

With `persist` set, Honey saves the cache to that file every `persistInterval` (5 minutes by default) and when it shuts down, and loads it when it starts, so a restart or deploy doesn't leave WordPress to take the full load while the cache fills up again.  Responses which expired while Honey was stopped aren't loaded, and ages carry on from when they were first cached.

- The file is replaced atomically, so it is never left half written, and only its owner can read it.
- A missing or unreadable file is logged, and Honey starts with an empty cache.  So is one saved by a version of Honey which keys responses differently.
- On shutdown, Honey waits for requests in progress to finish (for up to 30 seconds) before saving.
- The systemd service and the Docker image both have a writable `/var/lib/honey` for it.  With Docker, mount a volume there: `-v honey-cache:/var/lib/honey`.

## Logged in users and sessions

Some visitors get pages made just for them - a logged in user's admin bar and edit links, a commenter's name and email on the comment form, a shopper's cart - which mustn't be cached and served to everyone else.  WordPress usually marks these pages `private` or `no-store`, which Honey respects, but themes and plugins don't always.

So requests with any of the `bypassCookies` (cookie name prefixes) go straight to the backend, with `X-Honey-Cache: NO-CACHE`: they aren't served from the cache, and their responses aren't cached.  Responses which *set* one of them (e.g. a WooCommerce `?add-to-cart=` link starting a cart) aren't cached either, and the cookie reaches the visitor.

The default covers WordPress and WooCommerce:

| Cookie | Who has it |
| --- | --- |
| `wordpress_logged_in_` | Logged in users |
| `wp-postpass_` | Visitors who have entered a post's password |
| `comment_author_` | Visitors who have left a comment (the form is filled in with their name and email) |
| `woocommerce_items_in_cart`, `wp_woocommerce_session_` | Shoppers with a cart |

Add any your plugins use (e.g. a membership plugin's), or set `bypassCookies = []` to turn this off.  A route can override it - e.g. `bypassCookies = []` for `/wp-content/uploads/`, so that logged in users' images still come from the cache.

Other cookies don't stop a request being cached, and they don't change which cached response it gets (unless they are in `allowedCookies` and the backend sends `Vary: Cookie`).  Cookies that the backend sets are removed from cached responses, unless they are in `allowedCookies`.

## Browser caching

Browsers (and any CDN in front of Honey) normally get the `Cache-Control` header Honey caches the response by: the backend's, with `public` and `max-age=<defaultTTL>` added if it didn't say.  So a page cached for 5 minutes can be kept by browsers for 5 minutes too, and purging it from Honey doesn't reach them.

`browserCacheControl` replaces that header for browsers, without changing how long Honey keeps the response.  It can be set site-wide in `[cache]`, and for each `[[route]]` (where `"inherit"` sends the cached response's own).  For example:

	[cache]
	browserCacheControl = "no-cache"   # pages: browsers check back each time, and see purges straight away

	# css and js with a version, e.g. /wp-includes/js/jquery/jquery.min.js?ver=3.7.1
	[[route]]
	match = "\\.(css|js)\\?(.*&)?ver="
	regex = true
	browserCacheControl = "public, max-age=31536000, immutable"

	[[route]]
	match = "/wp-content/uploads/"
	browserCacheControl = "public, max-age=604800"

With `no-cache`, browsers still keep the page, but ask Honey whether it has changed each time; Honey answers from the cache with a `304 Not Modified` if it hasn't, so this costs a quick request, not a backend one.

Only successful (`200`), fresh responses which are served from the cache, or cached as they're fetched, get it: not errors or redirects, not stale responses, and not responses Honey doesn't cache (e.g. `private` ones, or for logged in users).  `Age` and `Expires` are left off, as the new header says how long browsers may keep the response from now.

Be careful with `immutable` and long `max-age`s: browsers won't ask for the file again, even after a purge.  Only use them where the URL changes whenever the file does.  WordPress's `ver` is the plugin's or theme's version (or WordPress's, if none was given), so a file edited without a version bump will be stale in browsers until the `max-age` runs out.

## Routes

Each `[[route]]` either stops the requests it matches from being cached (`cache = false`), or overrides some of the `[cache]` settings for them: `defaultTTL`, `staleIfError` (`"0"` turns it off), `queryParams` and `bypassCookies`.

- `cache = false` applies if any matching route says so, wherever it is in the list.
- Otherwise the first matching route's settings apply, so list more specific routes first.
- Routes are matched against the request as the client sent it, before Honey removes query parameters.

Durations can be given in days, e.g. `"7d"`, as well as e.g. `"90s"` or `"1h30m"`.

## Query parameters

By default every query parameter is part of the cache key, so `/page?nocache=123` is a different page to `/page`, and goes to the backend.  That lets anyone get around the cache, and tracking parameters like `utm_source` and `fbclid` split it.

Setting `queryParams` lists the parameters which matter.  Any others are removed from cacheable requests before they are looked up in the cache or sent to the backend, so the backend never sees them, and the cache can't serve the wrong content because of them.  Parameters are also sorted, so their order doesn't matter.  Requests which aren't cached (e.g. those matching a `[[route]]` with `cache = false`) are left alone.

For WordPress, core uses `p`, `page_id`, `s`, `paged`, `cat`, `tag`, `author`, `m`, `year`, `monthnum`, `day`, `post_type`, `cpage`, `replytocom`, `attachment_id` and `ver` (on css and js, so that an upgrade isn't served old cached files) - but check which ones your plugins use, as their parameters will stop working if they aren't listed.

## Admin page

With `[admin] listen` set, that address serves a page showing how the cache is doing - its hit ratio, how full it is, how each request was handled, and how quickly the backend responds - with a form to purge a URL, everything under a path, or the whole cache.  It refreshes itself every few seconds.  `/metrics` is served there too.

It has no login, so only listen on a private address, e.g. `127.0.0.1:9090`, and reach it with an SSH tunnel (`ssh -L 9090:127.0.0.1:9090 your-server`, then open http://localhost:9090/).  To protect it from other websites you visit, it is only served for an IP address or `localhost` (not a hostname, so that a site can't point its own hostname at it), its forms only accept posts from the page itself, and it can't be framed.  With Docker, listen on `":9090"` inside the container and publish it to the host's loopback only: `-p 127.0.0.1:9090:9090`.

## Metrics

With `[admin] listen` or `[metrics] listen` set, `/metrics` on that address has, in the Prometheus text format:

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

Purges ignore the host, so they work however they reach Honey: `curl -X PURGE http://127.0.0.1:8080/blog/*` purges `/blog/` for every host Honey caches.

### Purging from WordPress

[`contrib/wordpress/honey-purge.php`](contrib/wordpress/honey-purge.php) is a must-use plugin which purges pages when they change, so that visitors see edits straight away:

- When a post or page is published, updated, unpublished or deleted: it, its comment pages, the home page, and the archives which list it (its categories and tags, author, date and post type), and the feeds and sitemaps.  If its address changed, the old one too.
- When a comment is approved, edited or removed: its post.
- When a category or tag is renamed or deleted: its archive.
- When the theme, its customizations, menus, widgets, plugins, or the site's title, tagline, reading or permalink settings change: everything.

It also adds a "Purge cache" button to the admin bar for administrators.

To install it, copy it to `wp-content/mu-plugins/honey-purge.php`, and add to `wp-config.php`:

	define( 'HONEY_SECRET', 'a long random string' );   // Honey's control.secret
	define( 'HONEY_URL', 'http://127.0.0.1:8080' );     // optional: where to send purges, if not the site's address

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


## Ideas

Things which might be worth doing next:

- **Revalidating with the backend:** refresh expired responses with a conditional request (`If-None-Match` / `If-Modified-Since`), so that an unchanged page costs the backend a `304` rather than a full render.
- **A shared cache** (e.g. Redis), so that several Honey instances behind a load balancer share one cache, and purges reach all of them.
- **TLS termination** with automatic certificates (e.g. Let's Encrypt), so that Honey can face the internet without another proxy in front.
- **A login for the admin page**, so that it can be served somewhere other than a private address.
- **Per-route compression settings** (brotli and gzip are site-wide for now).

Dropped from the original list: HTTP/2 server push (browsers have removed it), rewriting popular JavaScript libraries to public CDNs (no faster since browsers stopped sharing caches between sites, and it leaks visitors to the CDN), minifying HTML, CSS and JS (risky, and brotli gets most of the benefit), and other page rewriting (fixing mixed content, combining Google Fonts requests, a cookieless asset domain, hotlink protection, an offline cache), which is better done in WordPress or a CDN.
