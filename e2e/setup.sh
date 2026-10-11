#!/bin/sh
# Installs WordPress for the end-to-end tests (run in the cli container)
set -eu
wp core install --url=http://localhost:8080 --title="Honey test" \
  --admin_user=admin --admin_password=password --admin_email=admin@example.com --skip-email
wp rewrite structure '/%postname%/'
# Let comments appear straight away, so that they can be tested
wp option update comment_previously_approved 0
wp option update comment_moderation 0
wp term create category News --slug=news
wp post term set 1 category news
wp post create --post_type=post --post_status=publish --post_name=members-only \
  --post_title="Members only" --post_content="The secret is honey." --post_password=open-sesame
wp user create editor editor@example.com --role=editor --user_pass=password

# WooCommerce, if asked for (it's downloaded from wordpress.org)
if [ -n "${HONEY_E2E_WOOCOMMERCE:-}" ]; then
  wp plugin install woocommerce --version=11.2.1 --activate
  wp plugin list --name=woocommerce --fields=name,version
  # Skip the setup wizard, which the first admin page redirects to
  wp option update woocommerce_onboarding_profile '{"skipped":true}' --format=json
  wp transient delete _wc_activation_redirect || true
  # The shop, cart, checkout and account pages
  wp wc tool run install_pages --user=admin
  wp wc product create --user=admin --name="Honey jar" --slug=honey-jar --regular_price=9.99 --status=publish
  # Pretty product addresses (/product/honey-jar/) need the rules flushed
  wp rewrite flush
fi
