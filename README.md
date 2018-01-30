# honey

Honey is an http cache and proxy.

In the event of a cache miss, It multiplexes requests to the same URL into a single request, and once the response has been received, writes it to all requesters, and adds it to the cache.

It will set an Etag on responses, and respond with an HTTP 301 Not Modified in the event that the `If-None-Match` header matches the Etag.

If will not cache responses that contain the `no-store` Cache-Control directive, and will not multiplex responses if they contain the `private` Cache-Control directive.

It will always fetch fresh resources if the `no-cache` Cache-Control directive, or if Pragma: no-cache, is set in the request

## Usage:

	backend, err = url.Parse("http://www.example.com")
	if err != nil {
		panic(err)
	}

	cacher := cache.NewDefaultCacher()
	fetcher := fetch.Fetch(cacher, backend)
	http.ListenAndServe(":8080", fetcher)


