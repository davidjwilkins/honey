# honey

Honey is an http cache and proxy.

In the event of a cache miss, It multiplexes requests to the same URL into a single request, and once the response has been received, writes it to all requesters, and adds it to the cache.

It will set an Etag on responses, and respond with an HTTP 304 Not Modified in the event that the `If-None-Match` header matches the Etag.

If will not cache responses that contain the `no-store` Cache-Control directive, and will not multiplex responses if they contain the `private` Cache-Control directive.

It will always fetch fresh resources if the `no-cache` Cache-Control directive, or if Pragma: no-cache, is set in the request

## Usage:

	backend, err = url.Parse("http://www.example.com")
	if err != nil {
		panic(err)
	}

	cacher := cache.NewDefaultCacher()
    //adding site_lang_id cookie to the default
    //cacher will make it use this cookie to 
    //vary the response
    cacher.AddAllowedCookie("site_lang_id")
	fetcher := fetch.Fetch(cacher, backend)
	http.ListenAndServe(":8080", fetcher)


### Todo

- [ ] Check [`Vary`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Vary) header from response and handle properly

- [ ] Send cached response with a [`Warning`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Warning) header if the backend gives an error after clearing  the cache. 

- [x] Set [`Last-Modified`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Last-Modified) header on response to the cached time if it is not already on the backend response. 

- [x] Handle [`If-Modified-Since`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/If-Modified-Since]) and [`If-Unmodified-Since`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/If-Unmodified-Since)

- [x] If cache miss, but after refresh Etag matches, send 304 Response

- [ ] If cache miss, but after refresh Validate matches, send 304 Response

- [x] Handle `only-if-cached` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control) 

- [x] Validate response or send to backend if `must-revalidate` or `proxy-revalidate` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control)

- [ ] Come up with a way to mark certain routes/files as [`immutable`](https://hacks.mozilla.org/2017/01/using-immutable-caching-to-speed-up-the-web/)

- [X] Add the `public` [Cache-Control directive](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control) unless `private` is received from backend.

- [x] Add [`Expires`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Expires) header to responses if not in response from backend.
	- [ ] Configurable Cache TTL
	- [ ] Configurable whether to overwrite backend Expires
	- [ ] Configure whether to serve stale content while refreshing, or multiplex requests into a single request and serve new content to all of them

- [x] Add `cookie` to [`Vary`](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Vary) header if the site has any `AllowedCookies`

- [ ] Add ability to configure which headers to include in the Request hash (e.g. Accept-Language)

- [ ] Letsencypt SSL termination

- [ ] Handle `stale-while-revalidate` and `stale-if-error` Cache-Control extensions

- [ ] Web UI to configure / clear cache and view metrics
