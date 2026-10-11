# Deploying WordPress with Honey

[`docker-compose.yml`](docker-compose.yml) runs a WordPress site behind Honey, behind [Caddy](https://caddyserver.com/), which gets an HTTPS certificate for it from Let's Encrypt:

	visitors ─► Caddy (HTTPS) ─► Honey (cache) ─► WordPress (Apache) ─► MariaDB

- WordPress has the [purge plugin](../wordpress/honey-purge.php), which tells Honey when pages change.  Honey allows purges from WordPress's address, so there's no secret to share.
- Honey trusts Caddy, and only Caddy (by its fixed address), so it knows each visitor's real address and that they used HTTPS, and visitors can't pretend otherwise.
- Honey only serves your domain: requests for any other host name are turned away, so they can't fill the cache.
- Expired pages are served while they're refreshed, the cache is kept across restarts, and it's warmed from the sitemap.
- Honey's admin page is on port 9090 of this host only.

To run it, on a server with Docker, whose DNS points your domain at it:

	cp .env.example .env              # and set your domain and a database password
	sed -i 's/www.example.com/your.domain/' honey.toml
	docker compose up -d

Then visit your site to install WordPress, and set its permalinks (Settings → Permalinks) to anything but "Plain", so that it has a sitemap to warm the cache from.

To reach the admin page from your computer: `ssh -L 9090:127.0.0.1:9090 your-server`, then open http://localhost:9090/.

To try it on your own computer, set `SITE_ADDRESS=localhost` (Caddy uses its own certificate authority for that, which your browser won't trust), and, if ports 80 and 443 are in use, `HTTP_PORT` and `HTTPS_PORT`.
