#!/bin/sh
# Turns WP Super Cache on (or, with "off", off) in its simple mode, in which
# PHP serves its cached pages before WordPress starts - as its settings page
# would (run in the cli container; it's downloaded from wordpress.org)
set -eu
cd /var/www/html
config=wp-content/wp-cache-config.php
# Sets a variable in its config, adding it after "<?php" if it isn't there
set_var() {
  if grep -q "^\$$1 *=" "$config"; then
    sed -i "s|^\$$1 *=.*;|\$$1 = $2;|" "$config"
  else
    sed -i "1a \$$1 = $2;" "$config"
  fi
}
if [ "${1:-on}" = off ]; then
  wp plugin deactivate wp-super-cache >/dev/null
  wp config set WP_CACHE false --raw >/dev/null
  rm -f wp-content/advanced-cache.php
  rm -rf wp-content/cache/supercache wp-content/cache/wp-cache-*
  echo "WP Super Cache is off"
  exit 0
fi
for attempt in 1 2 3 4; do
  if wp plugin install wp-super-cache --activate >/dev/null; then break; fi
  if [ "$attempt" = 4 ]; then exit 1; fi
  sleep $((attempt * 5))
done
home=/var/www/html/wp-content/plugins/wp-super-cache/
wp config set WPCACHEHOME "$home" >/dev/null
wp config set WP_CACHE true --raw >/dev/null
# What the plugin writes when it's set up
cat > wp-content/advanced-cache.php <<'PHP'
<?php
// WP SUPER CACHE (written by Honey's comparison)
if ( defined( 'WPCACHEHOME' ) ) {
	include_once WPCACHEHOME . 'wp-cache-phase1.php';
}
PHP
if [ ! -f "$config" ]; then
  if [ -f "$home/wp-cache-config-sample.php" ]; then
    cp "$home/wp-cache-config-sample.php" "$config"
  else
    echo '<?php' > "$config"
  fi
fi
set_var cache_enabled true
set_var super_cache_enabled true
set_var wp_cache_mod_rewrite 0
# As its settings page sets them: permalinks end in a slash (or it never
# serves cached pages for them), and say when a page is served from the
# cache
set_var wp_cache_slash_check 1
set_var wpsc_served_header true
set_var cache_path "WP_CONTENT_DIR . '/cache/'"
mkdir -p wp-content/cache
echo "WP Super Cache $(wp plugin get wp-super-cache --field=version) is on (simple mode)"
