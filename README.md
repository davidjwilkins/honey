# honey

Honey is an http cache and proxy.

In the event of a cache miss, It multiplexes requests to the same URL into a single request, and once the response has been received, writes it to all requesters, and adds it to the cache.

It will set an Etag on responses, and respond with an HTTP 304 Not Modified in the event that the `If-None-Match` header matches the Etag.

If will not cache responses that contain the `no-store` Cache-Control directive

It will always fetch fresh resources if the `no-cache` Cache-Control directive, or if Pragma: no-cache, is set in the request

## Usage:

	backend, err := url.Parse("https://www.example.com")
	if err != nil {
		panic(err)
	}

	cacher := cache.NewDefaultCacher()
	// adding site_lang_id cookie to the default
	// cacher will make it use this cookie to
	// vary the response
	cacher.AddAllowedCookie("site_lang_id")
	fetcher := fetch.Fetch(cacher, fetch.Forwarder(cacher), backend)
	http.ListenAndServe(":8080", fetcher)


### Todo

- [x] Set [`Last-Modified`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Last-Modified) header on response to the cached time if it is not already on the backend response.
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] Handle [`If-Modified-Since`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/If-Modified-Since]) and [`If-Unmodified-Since`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/If-Unmodified-Since)

- [x] If cache miss, but after refresh Etag matches, send 304 Response

- [x] Handle `only-if-cached` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control) 

- [x] Validate response or send to backend if `must-revalidate` or `proxy-revalidate` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control)
	- [ ] Configurable Site-wide whether to respect must-revalidate directive, or only if from list of IPs, or some sort of authentication mechanism
	- [ ] Configurable Per route

- [X] Add the `public` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control) unless `private` is received from backend.
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] Add [`Expires`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Expires) header to responses if not in response from backend.
	- [ ] Configurable Cache TTL
	- [ ] Configurable whether to overwrite backend Expires
	- [ ] Configure whether to serve stale content while refreshing, or multiplex requests into a single request and serve new content to all of them
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] Add ability to configure which headers to include in the Request hash (e.g. Accept-Language)
	- [ ] Configurable Site-wide
	- [ ] Configurable Per route

- [x] If cache miss, but after refresh Validate matches, send 304 Response

- [ ] Come up with a way to mark certain routes/files as [`immutable`](https://hacks.mozilla.org/2017/01/using-immutable-caching-to-speed-up-the-web/)

- [x] Check [`Vary`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Vary) header from response and handle properly

- [ ] Send cached response with a [`Warning`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Warning) header if the backend gives an error after clearing the cache. 
	- [ ] Configurable Site-wide (whether to send the warning to the end user, otherwise a webhook? or some other way to report the warning)
	- [ ] Configurable Per route

- [ ] Letsencypt SSL termination

- [ ] Handle `stale-while-revalidate` and `stale-if-error` Cache-Control extensions
	- [ ] Configurable Site-wide (whether to respect it)
	- [ ] Configurable Per route

- [ ] Web UI to configure / clear cache and view metrics

- [ ] Minify html, js, css before cacheing
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] [Canonicalize](https://www.modpagespeed.com/doc/filter-canonicalize-js#sample)  popular JavaScript libraries that can be replaced with ones hosted for free by a JavaScript library hosting service
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Brotli compress if requester supports it
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Implement [offline cache](https://developers.google.com/web/fundamentals/instant-and-offline/offline-cookbook/)
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Automatically fix mixed-content https issues
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Prevent hotlinking of images
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Use http/2 push to push assets if request doesn't have an If-None-Match header
	- [ ] Implement it
	- [ ] Make this configurable (whether to do it, site wide and per route)

- [ ] Rewrite static assets to cookieless subdomain

- [ ] Combine all google-font requests into a single one

- [ ] Implement configurable cache backends
	- [x] In Memory
	- [ ] File
	- [ ] Memcached
	- [ ] Redis
	- [ ] BoltDB

- [ ] Deploy from git
	- [ ] Clone from master, github.com webhook support
	- [ ] One click rollback
	- [ ] Branch preview

- [ ] Move to gitlab

- [ ] Don't multiplex responses if they contain the `private` Cache-Control directive.