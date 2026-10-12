#!/bin/sh
# Installs the comparison's WordPress (run in the cli container): a dozen
# posts, and a page like many real ones - a big featured image, a Google
# Font, and Google Analytics, the Meta pixel and Hotjar - for the page
# speed tests.
set -eu
wp core install --url=http://localhost:8080 --title="Honey comparison" \
  --admin_user=admin --admin_password=password --admin_email=admin@example.com --skip-email
wp rewrite structure '/%postname%/'
paragraph="<!-- wp:paragraph --><p>Honey is a caching reverse proxy for WordPress. It keeps pages in memory, serves them straight away, refreshes them in the background when they change, and only sends WordPress the requests it has to. This paragraph is here so that posts are about the size of real ones.</p><!-- /wp:paragraph -->"
content=""
for i in 1 2 3 4 5 6 7 8 9 10 11 12; do content="$content$paragraph"; done
for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
  wp post create --post_status=publish --post_name="post-$i" --post_title="Post number $i" --post_content="$content" >/dev/null
done
image=$(wp media import /work/hero.jpg --porcelain --title="Hero")
page=$(wp post create /work/page-speed.html --user=admin --post_type=post --post_status=publish --post_name=page-speed \
  --post_title="A typical post" --porcelain)
wp post meta set "$page" _thumbnail_id "$image" >/dev/null
echo "set up: 12 posts, and /page-speed/ (featured image $image)"
