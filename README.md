# honey

[![CI](https://github.com/davidjwilkins/honey/actions/workflows/ci.yml/badge.svg)](https://github.com/davidjwilkins/honey/actions/workflows/ci.yml)

Honey is an http cache and proxy.

In the event of a cache miss, It multiplexes requests to the same URL into a single request, and once the response has been received, writes it to all requesters, and adds it to the cache.

Cached responses are served until they expire, according to the backend's `Cache-Control` (`s-maxage`, `max-age`) or `Expires` headers, or a configurable default TTL if it sends neither.  The cache is bounded in size, evicting the least recently used responses when it is full.

If the backend sends `stale-while-revalidate`, expired responses are served immediately while they are refreshed in the background.  If it sends `stale-if-error`, expired responses are served (with a `Warning` header) when the backend errors or can't be reached.  WordPress sends neither, so Honey can add them: see [Never waiting for WordPress](#never-waiting-for-wordpress).

It will set an Etag on responses, and respond with an HTTP 304 Not Modified in the event that the `If-None-Match` header matches the Etag.

Static files (images, css, js, fonts, media, documents) are cached like any other response.  Responses larger than a configurable size (10MB by default) are streamed straight to the client instead of being cached.  Range requests (e.g. seeking in a video) are served from the cache if the file is cached, and otherwise sent to the backend (for static files; other range requests are fetched whole, like any other request).

Compressible responses (html, css, js, json, svg...) are served brotli compressed to clients which accept it, gzip compressed to clients which only accept gzip, and unencoded to the rest, all from the same cache entry (the client's `Accept-Encoding` q-values decide between them).  The brotli copy is made quickly (quality 6) when a response is first cached, so that the first requester isn't kept waiting, and then remade at maximum quality (11) in the background once it has been served from the cache (so pages requested only once never pay for it).  Responses with `Cache-Control: no-transform` aren't compressed.  The backend is asked for gzip (which Honey decodes) rather than the client's `Accept-Encoding`, so that the cache holds the unencoded response.

It won't cache responses marked `no-store` or `private`, or pages for logged in users and others whose pages may be personal (see [Logged in users and sessions](#logged-in-users-and-sessions)).  Requests with an `Authorization` header aren't cached either.  Nor are errors which depend on the request rather than the page, such as a 400, 403 or 429 (see [Security](#security)).

It will fetch fresh resources if the `Cache-Control: no-cache` directive, or `Pragma: no-cache`, is set in a request from a [trusted caller](#purging-and-refreshing).  From anyone else, these are ignored (RFC 9111 lets a cache do this), so that they can't be used to send every request to the backend.

Every response has an `X-Honey-Cache` header saying how it was served: `HIT`, `MISS`, `MISS (MULTIPLEXED)`, `STALE` or `NO-CACHE`.

## Installing

- **Binaries:** download one for Linux, macOS or Windows from the [releases](https://github.com/davidjwilkins/honey/releases), each with this README, an example config, a systemd service and the [WordPress purge plugin](#purging-from-wordpress).  Check it against `checksums.txt`.
- **Docker:** `docker build -t honey .` builds an 18MB image (a static binary on [distroless](https://github.com/GoogleContainerTools/distroless), running as a non-root user), which reads its config from `/etc/honey/honey.toml`:

		docker run -d -p 8080:8080 -v "$PWD/honey.toml:/etc/honey/honey.toml:ro" honey

- **From source** (with Go 1.26 or later): `go install github.com/davidjwilkins/honey/cmd/honey@latest`
- **systemd:** [`contrib/honey.service`](contrib/honey.service) runs Honey as an unprivileged, sandboxed service, reading `/etc/honey/honey.toml`.  Instructions are at the top of the file.
- **A whole site:** [`contrib/deploy`](contrib/deploy) runs WordPress behind Honey, behind Caddy for HTTPS, with Docker Compose.  Honey can also serve [HTTPS and HTTP/3](#https-and-http3) itself.

`honey -version` prints the version.  Pushing a `v*` tag builds and publishes a release.

## Running it

	honey -config honey.toml

A minimal config just needs a backend - the web server running WordPress, which Honey sits in front of:

	[backend]
	uri = "http://127.0.0.1:8081"

All the available settings:

	listen = ":8080"             # address to listen on (plain HTTP)
	trustedProxies = ["127.0.0.1", "10.0.0.0/8"]  # proxies in front of Honey (default: loopback and private addresses)
	hosts = ["www.example.com", "*.example.com"]  # the only hosts served; others get a 421 (unset: any)

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
	staleWhileRevalidate = "1d"  # serve expired responses this long while they're refreshed in the
	                             # background, so visitors don't wait (unless they say otherwise; off by default)
	persist = "/var/lib/honey/cache.snapshot"  # save the cache here, and load it at startup (off unless set)
	persistInterval = "5m"       # how often to save it ("0": only on shutdown)
	staticFiles = true           # cache images, css, js, fonts, media and documents
	earlyHints = true            # send browsers waiting for WordPress the page's stylesheets and scripts
	                             # first, as 103 Early Hints (off by default; see below)
	brotli = true                # brotli compress html, css, js, json, svg... for clients that accept it
	gzip = true                  # gzip them for clients that accept gzip but not brotli
	compressMinSize = "1KB"      # smaller responses aren't compressed
	bypassCookies = ["wordpress_logged_in_", "wp-postpass_", "comment_author_",
	    "woocommerce_items_in_cart", "wp_woocommerce_session_"]  # (the default) see below
	esiCookies = ["comment_author_"]   # bypass cookies whose visitors get cached pages anyway, if they
	                                   # use Edge Side Includes for the personal parts (see below)
	allowedCookies = ["site_lang_id"]  # cookies passed to the backend on cached requests, and part of the cache key
	stripCookies = ["pll_language"]    # cookies removed from responses, which are then cached (see below)
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
	# staleIfError = "0"         #   staleWhileRevalidate, queryParams, bypassCookies and browserCacheControl
	# queryParams = ["ver"]
	# bypassCookies = []
	# browserCacheControl = "public, max-age=604800"   # or "inherit"

	# Fill the cache with the pages in the sitemap when Honey starts, and
	# after everything is purged (off unless sitemap is set)
	[warm]
	sitemap = "https://www.example.com/wp-sitemap.xml"
	rate = 2                     # pages a second
	maxPages = 10000
	interval = "6h"              # also this often ("0": only at startup and after purging everything)

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

	# Serve HTTPS (HTTP/1.1 and HTTP/2) and HTTP/3 too (off unless auto or
	# certFile is set; see below)
	[tls]
	auto = true                  # certificates for the hosts from Let's Encrypt
	email = "you@example.com"    # given to Let's Encrypt (optional)
	certDir = "/var/lib/honey/certs"   # where they're kept
	# acmeCA = "https://acme-staging-v02.api.letsencrypt.org/directory"  # another ACME CA
	# certFile = "/etc/ssl/example.com/fullchain.pem"   # or your own certificate
	# keyFile = "/etc/ssl/example.com/privkey.pem"
	listen = ":443"              # TCP for HTTPS, and UDP for HTTP/3
	http3 = true
	redirect = true              # redirect plain HTTP (listen) to HTTPS, except purges

Honey refuses to start if the config has settings it doesn't support.  See [`config/wordpress.toml`](config/wordpress.toml) for an example WordPress setup.

## Hosts

Honey passes the backend the `Host` header clients send (e.g. `www.example.com`), as WordPress redirects requests for any other host to its own address.  The backend also gets `X-Forwarded-Host` (the same host, replacing any the client sent) and `X-Forwarded-Proto` (`http` or `https`, unless a proxy in front of Honey has already set it).

Responses are cached separately for each host and protocol, so e.g. a redirect from `example.com` to `www.example.com` is only served for `example.com`.

To proxy a site hosted elsewhere, whose server only answers to its own name, set `backend.host` to it: it is sent as the `Host` header instead.

By default any host is served, so a request with a made-up `Host` reaches the backend and is cached.  Set `hosts` to the site's host names to turn the rest away with a `421 Misdirected Request` (`"*.example.com"` allows any subdomain).  Purges are allowed whatever their `Host`, as they apply to every host.  If `[warm] sitemap` is set, its host must be one of them.

## HTTPS and HTTP/3

Honey can face the internet itself, without Caddy or nginx in front for HTTPS.  With `[tls] auto = true`, it gets certificates from [Let's Encrypt](https://letsencrypt.org) for each of the `hosts` (which must be set, without wildcards) when they're first asked for, keeps them in `certDir`, and renews them before they expire.  Or set `certFile` and `keyFile` to a certificate of your own (e.g. from certbot), which Honey loads again when the files change.

	listen = ":80"
	hosts = ["www.example.com", "example.com"]

	[tls]
	auto = true
	email = "you@example.com"

Then Honey serves:

- **HTTPS** on `tls.listen` (`:443`), with HTTP/2 for browsers which support it.
- **HTTP/3** on the same port over UDP (unless `http3 = false`).  Browsers are told about it in an `Alt-Svc` header, and switch to it for later requests.  [Early Hints](#early-hints) work over both.
- **Plain HTTP** on `listen`, which redirects to HTTPS (unless `redirect = false`), except for Let's Encrypt's challenges and `PURGE` requests, so that the [WordPress plugin](#purging-from-wordpress) can still send purges to e.g. `http://127.0.0.1:80`.  Requests for hosts which aren't in `hosts` aren't redirected, and get a `421`.

Requests which come over HTTPS or HTTP/3 have `X-Forwarded-Proto: https`, so WordPress knows the visitor used HTTPS, and they're cached separately from plain HTTP ones (see [Hosts](#hosts)).

Ports 80 and 443 must be reachable from the internet (including 443 over UDP, for HTTP/3), and the hosts' DNS must point to Honey.  Let's Encrypt checks you control a host over port 443 (or port 80), so certificates can be had as soon as Honey is reachable.  To try it without using up Let's Encrypt's rate limits, set `acmeCA` to its [staging](https://letsencrypt.org/docs/staging-environment/) directory, whose certificates browsers don't trust.

Listening on ports below 1024 needs root or `CAP_NET_BIND_SERVICE`: the [systemd service](contrib/honey.service) explains how, and Docker allows it.  The service's `/var/lib/honey` (`StateDirectory`) is where certificates are kept by default.  On Linux, HTTP/3 is faster if the UDP buffers may be larger: `sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000`.

## Proxies in front of Honey

If Honey is behind another proxy - e.g. Caddy or nginx handling HTTPS - every request comes from the proxy's address, and the proxy says who the visitor is (`X-Forwarded-For`) and whether they used HTTPS (`X-Forwarded-Proto`).  Honey believes these from `trustedProxies`, which are loopback and private addresses by default (where such a proxy usually is: on the same host, or in the same Docker or private network).  Then:

- the visitor's address is the nearest one in `X-Forwarded-For` which isn't another trusted proxy, so a visitor can't claim to be someone else by sending their own;
- `allowIPs` and the access log use it, and WordPress gets it in `X-Real-IP`;
- the proxy's `X-Forwarded-Proto` is passed on, so WordPress knows the visitor used HTTPS.

Requests from anywhere else have their forwarding headers (`X-Forwarded-For`, `X-Forwarded-Proto`, `X-Real-IP`, `Forwarded` and so on) removed, so that they can't pretend to have used HTTPS, or to be from an address in `allowIPs`.

If the proxy is somewhere else (e.g. a CDN, or a load balancer with a public address), list its addresses in `trustedProxies`.  If anything else on the private network can reach Honey (e.g. other containers in the same Docker network), list only the proxy's address, so that they can't pretend to be visitors from elsewhere - as the [deployment example](contrib/deploy) does.  If nothing is in front of Honey and it's reachable from a private network you don't trust, set `trustedProxies = []`.

## Keeping the cache across restarts

With `persist` set, Honey saves the cache to that file every `persistInterval` (5 minutes by default) and when it shuts down, and loads it when it starts, so a restart or deploy doesn't leave WordPress to take the full load while the cache fills up again.  Responses which expired while Honey was stopped aren't loaded, and ages carry on from when they were first cached.

- The file is replaced atomically, so it is never left half written, and only its owner can read it.
- A missing or unreadable file is logged, and Honey starts with an empty cache.  So is one saved by a version of Honey which keys responses differently.
- On shutdown, Honey waits for requests in progress to finish (for up to 30 seconds) before saving.
- The systemd service and the Docker image both have a writable `/var/lib/honey` for it.  With Docker, mount a volume there: `-v honey-cache:/var/lib/honey`.

## Never waiting for WordPress

Once a page expires, the next visitor to ask for it would normally wait while Honey fetches it from WordPress again.  With `staleWhileRevalidate` set (e.g. `"1d"`), they get the expired page straight away instead, with `X-Honey-Cache: STALE`, while Honey fetches the new one in the background for the visitors after them.  So, as long as a page is asked for at least once a day, nobody waits for WordPress to build it - and a page is never more than one refresh out of date.

Purges can work the same way.  A *soft* purge marks pages as expired, rather than removing them: the next visitor gets the old version once more, while the new one is fetched.  The [WordPress plugin](#purging-from-wordpress) purges softly when posts are edited, comments are added and settings change, and hard - so that the old version is never served again - when a post is unpublished, deleted or given a password, a comment is removed, or the "Purge cache" button is pressed.  Without `staleWhileRevalidate`, a soft purge is just like a hard one (except that the old version can still be served if WordPress is down, per `staleIfError`).

### Warming the cache

With `[warm] sitemap` set to the site's sitemap (WordPress's is `/wp-sitemap.xml`), Honey requests every page it lists - as a visitor with no cookies would - when it starts, and again whenever everything is purged (e.g. by the plugin's "Purge cache" button, or when the theme changes), so that visitors find them cached.  Pages already in the cache (e.g. loaded from `persist`) aren't fetched again.  It goes gently, at `rate` pages a second (2 by default), and stops at `maxPages`.  With `interval` set, it does it again that often, so that pages nobody has asked for lately are refreshed too.

Warming isn't counted in the metrics or the access log.  Only pages on the sitemap's scheme and host are requested.

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

Add any your plugins use (e.g. a membership plugin's), or set `bypassCookies = []` to turn this off.  Commenters can get cached pages too, if the pages use [Edge Side Includes](#edge-side-includes) for their comments.  A route can override it - e.g. `bypassCookies = []` for `/wp-content/uploads/`, so that logged in users' images still come from the cache.

Other cookies don't stop a request being cached - most visitors have some, e.g. analytics cookies - but they are removed from it before it is sent to the backend, so that one visitor's cookies (a theme switcher's, say) can't change the page which is cached for everyone.  The exceptions are `allowedCookies`, which are passed on, and which responses are cached separately for each value of (whether or not the backend sends `Vary: Cookie`).  So if a plugin remembers something about visitors who aren't logged in in a cookie - a cart, or a currency - add it to `bypassCookies` (their pages aren't cached) or `allowedCookies` (a copy of each page is cached for each value), or the plugin won't see it.

Responses which set a cookie which isn't in `allowedCookies` aren't cached either, as the page may go with it (e.g. a form with a token for the session the cookie starts): the visitor gets the page and the cookie, with `X-Honey-Cache: NO-CACHE`, and Honey logs the cookie's name the first time.  Cookies which don't matter to the page, such as Polylang's `pll_language`, can be listed in `stripCookies` (name prefixes) instead: they are removed from responses, which are then cached.  A response which marks its `Set-Cookie` header private (`Cache-Control: private="set-cookie"` or `no-cache="set-cookie"`) is cached without it, and the visitor still gets the cookie.

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

## Early Hints

A browser can't start fetching a page's stylesheets, fonts and scripts until the page starts to arrive.  A page from the cache arrives straight away, but one which has to wait for WordPress - for a logged in user, a shopper, a page which isn't cached yet, or one with [Edge Side Includes](#edge-side-includes) - can take a second.  With `earlyHints = true`, Honey sends the browser a `103 Early Hints` response first, listing what the page needs, so that it can fetch them in the meantime:

	HTTP/1.1 103 Early Hints
	Link: </wp-includes/css/dist/block-library/style.min.css?ver=7.0>; rel=preload; as=style
	Link: </wp-content/themes/twentytwentyfive/style.css?ver=1.3>; rel=preload; as=style

	HTTP/1.1 200 OK
	...the page, when WordPress has rendered it...

Honey learns them from the `<head>` of the pages it caches: its stylesheets (not those for print), the scripts which hold it up (not `async` or `defer` ones), and its preloads (e.g. fonts), preconnects and module preloads - up to 10.  A request gets those of the page cached for its URL (for visitors without its cookies), or, for a page which isn't cached (e.g. a cart or checkout), those which every one of its host's recently cached pages have, which are usually the theme's.  Requests served from the cache, and those which don't accept HTML, don't get them.  The admin page's cache browser shows each page's.

They're off by default, as browsers only use them over HTTP/2 or HTTP/3: if Honey serves [HTTPS](#https-and-http3) itself, turn them on.  Otherwise the proxy in front of Honey has to pass them on: Caddy does, and nginx does from 1.29, with its `early_hints` directive.  Check that yours does before turning them on, as older proxies may not expect them.

## Routes

Each `[[route]]` either stops the requests it matches from being cached (`cache = false`), or overrides some of the `[cache]` settings for them: `defaultTTL`, `staleIfError` and `staleWhileRevalidate` (`"0"` turns them off), `queryParams`, `bypassCookies` and `browserCacheControl`.

- `cache = false` applies if any matching route says so, wherever it is in the list.
- Otherwise the first matching route's settings apply, so list more specific routes first.
- Routes are matched against the request as the client sent it, before Honey removes query parameters.

Durations can be given in days, e.g. `"7d"`, as well as e.g. `"90s"` or `"1h30m"`.

## Query parameters

By default every query parameter is part of the cache key, so `/page?nocache=123` is a different page to `/page`, and goes to the backend.  That lets anyone get around the cache, and tracking parameters like `utm_source` and `fbclid` split it.

Setting `queryParams` lists the parameters which matter.  Any others are removed from cacheable requests before they are looked up in the cache or sent to the backend, so the backend never sees them, and the cache can't serve the wrong content because of them.  Parameters are also sorted, so their order doesn't matter.  Requests which aren't cached (e.g. those matching a `[[route]]` with `cache = false`) are left alone.

For WordPress, core uses `p`, `page_id`, `s`, `paged`, `cat`, `tag`, `author`, `m`, `year`, `monthnum`, `day`, `post_type`, `cpage`, `replytocom`, `attachment_id` and `ver` (on css and js, so that an upgrade isn't served old cached files) - but check which ones your plugins use, as their parameters will stop working if they aren't listed.

## Security

A cache serves one visitor's response to everyone, so anything in a request which changes the backend's response, but which isn't part of the cache key, could let one visitor change a page for everyone else (cache poisoning).  Honey's cache key is the method, protocol, host, path and query, the allowed cookies, and the headers the response `Vary`s on.  For requests which may be cached, it also:

- removes cookies other than the `allowedCookies` (see [Logged in users and sessions](#logged-in-users-and-sessions))
- removes the headers which make some backends handle a request as another method or URL: `X-HTTP-Method-Override` (which WordPress's REST API obeys), `X-HTTP-Method`, `X-Method-Override`, `X-Original-URL` and `X-Rewrite-URL`
- only believes `X-Forwarded-For`, `X-Forwarded-Proto` and the like from `trustedProxies` (see [Proxies in front of Honey](#proxies-in-front-of-honey))
- only caches the status codes RFC 9111 allows without being told to (200, 203, 204, 300, 301, 308, 404, 405, 410 and 414), and 302, 303 and 307 if the backend says how long they're fresh for.  Other errors, such as a 400 from a request with a header too large for the backend, or a 403 or 429 from a firewall, depend on the request rather than the page, so they only go to that visitor - they aren't cached, nor given to visitors waiting for the same page, and a stale page being refreshed is kept.  Server errors (5xx) aren't cached either, but are given to visitors waiting for the same page, so that a struggling backend isn't sent all their requests at once.

Some things are up to you:

- **Hosts and query strings.**  By default, a request with a made-up `Host` or query parameter (`?x=random`) misses the cache, reaches the backend, and is cached.  Set `hosts` and `queryParams` to stop that.  (A cache can't protect the backend from everything - requests which aren't cached, such as `POST`s, always reach it.)
- **Headers your site depends on.**  If a theme or plugin changes pages according to another request header without sending `Vary` for it - e.g. a mobile theme using `wp_is_mobile()`, which reads `User-Agent` - Honey caches whichever version is requested first.  Make it send `Vary`, or don't cache those pages.
- **The admin page and metrics** have no login: keep them on a private address (see [Admin page](#admin-page)).  `/metrics` is served for any `Host` (so that Prometheus can scrape it by name), so it is readable by a page in a visitor's browser using DNS rebinding, but it only has request counts and cache stats.

## Admin page

With `[admin] listen` set, that address serves a page showing how the cache is doing, which refreshes itself every few seconds:

- how many requests are served from the cache, and how that has gone each minute over the last hour
- how quickly the backend responds (median, 95th and 99th percentiles, and a histogram), and its errors
- how full the cache is, and how each request was handled
- recent events: purges (with where they came from, a burst of them from WordPress shown as one), warming the cache, backend errors, requests for other hosts turned away, and pages which aren't cached because they set a cookie

and a bar to purge a URL or everything under a path - softly or not - which says how many cached responses it will remove as you type, or to purge the whole cache.  `/metrics` is served there too.

**Cache** lists what's cached - each response's path, status, whether it's fresh or stale, how often it has been served from the cache, its size and when it was fetched - filtered by path (starting with a path, or containing some text), by kind (pages, static files, redirects and errors) and by state, and sorted by how recently it was used, how often it was served, its size or its age.  Everything under a path can be expired or purged from there.  Each response's page shows its freshness, sizes (and compressed sizes), the headers it's served with and the start of its body, with buttons to expire or purge it and a link to test its URL.

**Test a URL** shows how Honey would handle a request - a URL (or a path), and optionally the visitor's cookies - step by step, without fetching anything: whether its host is served, which route applies, which query parameters and cookies are removed, whether it's cacheable (and if not, why), its cache key, how long it stays fresh, and whether it's in the cache now.

**Settings** shows Honey's settings as a form, with each one's default.  Change them there and it shows the config file they make - what changed, and whether it's valid - and exports it as a new `honey.toml`.  Honey's own config file is never changed: replace it with the export and restart Honey to use the new settings.  The purge secret is never shown on the page, but it is in the export.

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
	# softly: the old version is served once more while the new one is fetched
	curl -X PURGE -H "X-Honey-Secret: $SECRET" -H "X-Honey-Purge: soft" https://www.example.com/2018/02/my-post/

They can also make Honey fetch a fresh copy, and cache it, by sending `Cache-Control: no-cache`.  Untrusted `PURGE` requests get a `403 Forbidden`.

Purges ignore the host, so they work however they reach Honey: `curl -X PURGE http://127.0.0.1:8080/blog/*` purges `/blog/` for every host Honey caches.

A `PURGE` with an `X-Honey-Fragment` header purges [shared fragments](#shared-fragments) instead of a URL: a name, a name ending with `*` for every fragment starting with what precedes it, or several separated by commas.

	curl -X PURGE -H "X-Honey-Secret: $SECRET" -H "X-Honey-Fragment: posts-*" https://www.example.com/

## Shared fragments

Some parts of a page are the same on every page - a list of recent posts in a sidebar, say.  When one changes, every page showing it is out of date, but purging every page would make WordPress render the whole site again.  Instead, the backend can mark such a part as a shared fragment:

	<!--honey:fragment name="posts-widget-recent-posts-2" token="TOKEN"-->
	  ...the list...
	<!--/honey:fragment-->

Honey keeps each fragment's content separately.  Whenever it caches a page with a fragment, which the backend has just rendered, that becomes the fragment's current content, and every other cached page showing the fragment is rebuilt with it the next time it's served - once per change, with a new `Etag`, not every time.  Purging a fragment (above) refreshes one page which shows it, to get its new content, so a change reaches every page while WordPress renders only one.

`TOKEN` is the value of the `X-Honey-Fragments` header Honey sends the backend with each request for a page it may cache.  It is different for every request, so markers in a page's content (e.g. typed into a post) aren't fragments, and can't change other pages.  Honey removes it from the page.  Only HTML pages have fragments, a page has at most 50, of up to 64KB each, and Honey keeps up to 1,000.  A fragment must be the same on every page which shows it, and each host's are kept separately.  Fragments aren't kept across restarts: pages from a [snapshot](#keeping-the-cache-across-restarts) are served as they were cached until they're refreshed.

The [WordPress plugin](#purging-from-wordpress) marks the Latest Posts block and the Recent Posts widget as fragments (other blocks can be added with its `honey_fragment_blocks` filter), and purges them when a post is published, updated, unpublished or deleted.  Lists which differ from page to page, such as one marking the current post, aren't marked.  The admin page's Cache tab lists the fragments, and can purge them.

## Edge Side Includes

Some pages differ between visitors in just one part - the comment form, filled in with a commenter's name and email, say.  With [Edge Side Includes](https://www.w3.org/TR/esi-lang/), the backend leaves that part out, with a tag saying where to get it:

	<esi:include src="/hello-world/?honey-esi=comments.4f2a"/>

Honey caches the page, and fills in its includes for each visitor when it serves it, by requesting each `src` from itself, with the visitor's cookies.  An include which is the same for everyone comes from the cache like any other page, and one for a visitor with bypass cookies comes from the backend.  So the page is cached once, and only the part which differs is fetched for each visitor.

Honey tells the backend it supports them by sending `Surrogate-Capability: honey="ESI/1.0"` with requests for pages it may cache, and only processes an HTML page's tags if it responds with `Surrogate-Control: content="ESI/1.0"` (which visitors don't get).  It supports `<esi:include src="..." alt="..."/>`, `<esi:remove>` and `<!--esi ... -->`.  Includes must be on the same site, are fetched at the same time, up to 20 a page and 3 deep, within 10 seconds; one which fails leaves a gap rather than failing the page.  Their `honey-esi` query parameter is always kept, whatever `queryParams` says.  A page with includes differs between visitors, so browsers get it with `Cache-Control: private, no-cache`, and without an `Etag`, and it is compressed as it's served.

Visitors with bypass cookies don't get cached pages, but those whose bypass cookies are all `esiCookies` (cookie name prefixes) do, if the page cached for visitors without the cookies is fresh and uses ESI: the personal parts are the includes, fetched from the backend with their cookies.  Otherwise their requests go to the backend as usual.  Set `esiCookies = ["comment_author_"]` for commenters, with the [WordPress plugin](#purging-from-wordpress): with a block theme, it leaves the Comments block out of the post's page, in favour of an include of the same address with `?honey-esi=comments.<id>`, which shows just the Comments block.  So commenters get the post from the cache, and only its comments from WordPress, with their name filled in and their comments awaiting moderation shown.  Purging the post purges its comments too, as they're under its address.  Classic themes aren't supported yet.

The admin page's Test a URL says whether a page uses ESI, and whether a visitor with the cookies you give would get it.

### Purging from WordPress

[`contrib/wordpress/honey-purge.php`](contrib/wordpress/honey-purge.php) is a must-use plugin which purges pages when they change, so that visitors see edits straight away:

- When a post or page is published, updated, unpublished or deleted: it, its comment pages, the home page, and the archives which list it (its categories and tags, author, date and post type), and the feeds and sitemaps.  If its address changed, the old one too.
- When a comment is approved, edited or removed: its post.
- When a category or tag is renamed or deleted: its archive.
- When the theme, its customizations, menus, widgets, plugins, or the site's title, tagline, reading or permalink settings change: everything.

It also marks lists of recent posts as [shared fragments](#shared-fragments), and purges them when a post changes, so that every page shows new posts straight away.  With block themes, it serves posts' comments as [Edge Side Includes](#edge-side-includes), so that commenters can get cached pages.  And it adds a "Purge cache" button to the admin bar for administrators.  Most of its purges are soft, and some are hard (see [Never waiting for WordPress](#never-waiting-for-wordpress)).

To install it, copy it to `wp-content/mu-plugins/honey-purge.php`, and add to `wp-config.php`:

	define( 'HONEY_SECRET', 'a long random string' );   // Honey's control.secret
	define( 'HONEY_URL', 'http://127.0.0.1:8080' );     // optional: where to send purges, if not the site's address

If Honey is behind another proxy (e.g. Caddy or nginx for TLS), `allowIPs` is checked against the visitor's address, as long as the proxy is one of the `trustedProxies` (see [Proxies in front of Honey](#proxies-in-front-of-honey)).

## Testing

`go test ./...` runs the unit tests.  [`e2e/run.sh`](e2e/run.sh) runs end-to-end tests against a real WordPress, with the purge plugin, behind Honey - checking that pages are cached, that logged in users, commenters and visitors to password-protected posts get their own pages, and that edits, comments and setting changes are purged.  It needs Docker and Go, and CI runs it on every pull request.  With `HONEY_E2E_WOOCOMMERCE=1`, it also installs WooCommerce (from wordpress.org) and checks that shop pages are cached, that adding to the cart starts each shopper's own session, that shoppers' pages and Store API carts are their own, and that the cart and checkout are never cached; CI does this too.  `HONEY_E2E_KEEP=1 e2e/run.sh` leaves it all running afterwards, on http://localhost:8080/ (log in as `admin`, password `password`) with Honey's admin page on http://127.0.0.1:9090/.

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
- **DNS challenges**, so that Honey can get wildcard certificates (`*.example.com`) automatically.
- **A login for the admin page**, so that it can be served somewhere other than a private address.
- **Per-route compression settings** (brotli and gzip are site-wide for now).
- **More Edge Side Includes:** commenters on classic themes, and parts of pages for logged in users and shoppers (e.g. a cart summary, or the admin bar), so that their pages can mostly be served from the cache too.

Dropped from the original list: HTTP/2 server push (browsers have removed it), rewriting popular JavaScript libraries to public CDNs (no faster since browsers stopped sharing caches between sites, and it leaks visitors to the CDN), minifying HTML, CSS and JS (risky, and brotli gets most of the benefit), and other page rewriting (fixing mixed content, combining Google Fonts requests, a cookieless asset domain, hotlink protection, an offline cache), which is better done in WordPress or a CDN.
