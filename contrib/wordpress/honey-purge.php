<?php
/**
 * Plugin Name: Honey cache purging
 * Description: Purges pages from the Honey cache when they change, so that visitors see edits straight away.
 * Version:     1.0.0
 * Plugin URI:  https://github.com/davidjwilkins/honey
 * License:     MIT
 *
 * Install it as a must-use plugin: copy this file to
 * wp-content/mu-plugins/honey-purge.php.  Then, in wp-config.php:
 *
 *     // Honey's control.secret
 *     define( 'HONEY_SECRET', 'a long random string' );
 *     // Where to send purges, if not the site's own address (e.g. if
 *     // Honey is behind another proxy, or WordPress can't reach the
 *     // site's public address)
 *     define( 'HONEY_URL', 'http://127.0.0.1:8080' );
 *     // control.secretHeader, if it isn't X-Honey-Secret
 *     define( 'HONEY_SECRET_HEADER', 'X-Honey-Secret' );
 *     // Log what is purged, and why, to the PHP error log
 *     define( 'HONEY_DEBUG', true );
 *
 * What it purges:
 *
 * - When a post (of any public type) is published, updated, unpublished or
 *   deleted: the post (and its comment pages), the home page, and the
 *   archives which list it - its categories, tags and other terms, its
 *   author, its date and its post type - and the feeds and sitemaps.  If
 *   its address changed, the old address too.
 * - When a comment on a post is approved, edited or removed: the post.
 * - When a term (e.g. a category) is edited or deleted: its archive.
 * - When anything shown on every page changes - the theme, its
 *   customizations, menus, widgets, plugins, or the site's title,
 *   tagline, reading or permalink settings: everything.
 *
 * Administrators also get a "Purge cache" button in the admin bar.
 *
 * Shared fragments: the Latest Posts block and the Recent Posts widget
 * appear on many pages, so a new post would otherwise leave all of those
 * out of date.  When Honey asks (with its X-Honey-Fragments header), they
 * are marked as shared fragments, which Honey keeps separately.  When a
 * post is published, updated, unpublished or deleted, the fragments are
 * purged, and Honey updates every cached page showing them without
 * WordPress rendering each one again.  Lists which differ from page to
 * page (e.g. marking the current post) aren't marked.  Other blocks can
 * be marked with the honey_fragment_blocks filter.
 *
 * Commenters: WordPress fills in the comment form for visitors who have
 * commented before (from their comment_author_ cookies), and shows them
 * their comments awaiting moderation, so their pages usually can't be
 * cached.  With block themes, when Honey says it supports Edge Side
 * Includes, the Comments block is left out of the page, in favour of an
 * <esi:include> of the same address with ?honey-esi=comments.<id>, which
 * shows just the Comments block.  Honey caches the page, and fetches the
 * comments for each visitor: from the cache for most, but from WordPress
 * for commenters, if Honey's cache.esiCookies includes "comment_author_".
 * Classic themes aren't supported yet.
 *
 * Shoppers: WooCommerce's pages show shoppers their cart (in the "1 in
 * cart" buttons and the mini cart, from the cart in the page's
 * Interactivity API state), and messages such as "added to your cart"
 * (store notices), so their pages usually can't be cached either.  When
 * Honey says it supports Edge Side Includes, pages other than the cart,
 * checkout and account pages say they're for shoppers too, and if they
 * show the cart or notices, end with an include of
 * ?honey-esi=woocommerce.cart (and/or .notices) in an ESI comment.  Only
 * shoppers get it, if Honey's cache.esiCookies includes
 * "woocommerce_items_in_cart" and "wp_woocommerce_session_": it puts their
 * cart into the page's state before the page starts, and their notices
 * into its notices area.  Everyone else gets the page as it is.  The
 * include is from WordPress for each page a shopper sees, but WordPress
 * only loads their cart for it, rather than rendering the whole page.
 * Block themes only, for now.
 *
 * Hiding users: with Honey's security.hideUsers, Honey asks (with its
 * X-Honey-Hide-Users header) for the site's users to be hidden from
 * visitors who aren't logged in, so that their usernames can't be listed
 * for guessing passwords: ?author=N (which redirects to the author's
 * archive) is a 404, the REST API's users can't be listed, and the
 * sitemap of authors is left out.
 *
 * Purges are sent when WordPress has finished handling the request, once
 * each, as PURGE requests (see Honey's README).  Most are soft: if Honey's
 * staleWhileRevalidate allows, the old page is served once more while the
 * new one is fetched, so nobody waits for WordPress.  Pages which must
 * disappear straight away are purged hard: those of a post which is
 * unpublished, deleted or given a password, of a comment which is removed,
 * and of a deleted term - and everything, when the "Purge cache" button is
 * pressed.
 */

namespace Honey;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

final class Purge {
	/**
	 * @var array<string, array{prefix: bool, hard: bool}> paths to purge,
	 * whether to purge everything under them, and whether to purge them hard
	 */
	private static $paths = array();

	/** @var string|null whether to purge everything: "soft", "hard" or null */
	private static $everything = null;

	/** @var array<string, true> shared fragments to purge */
	private static $fragments = array();

	/** @var bool whether the shutdown hook has been added */
	private static $scheduled = false;

	public static function init() {
		add_action( 'transition_post_status', array( __CLASS__, 'post_status_changed' ), 10, 3 );
		add_action( 'post_updated', array( __CLASS__, 'post_updated' ), 10, 3 );
		add_action( 'before_delete_post', array( __CLASS__, 'post_deleted' ), 10, 2 );

		add_action( 'comment_post', array( __CLASS__, 'comment_posted' ), 10, 2 );
		add_action( 'edit_comment', array( __CLASS__, 'comment_edited' ) );
		add_action( 'transition_comment_status', array( __CLASS__, 'comment_status_changed' ), 10, 3 );
		add_action( 'deleted_comment', array( __CLASS__, 'comment_deleted' ), 10, 2 );

		add_action( 'edited_term', array( __CLASS__, 'term_changed' ), 10, 3 );
		add_action( 'pre_delete_term', array( __CLASS__, 'term_deleted' ), 10, 2 );

		foreach ( array(
			'switch_theme',
			'customize_save_after',
			'wp_update_nav_menu',
			'wp_delete_nav_menu',
			'activated_plugin',
			'deactivated_plugin',
			'upgrader_process_complete',
		) as $hook ) {
			add_action( $hook, array( __CLASS__, 'everything' ) );
		}
		add_action( 'updated_option', array( __CLASS__, 'option_updated' ) );

		add_action( 'admin_bar_menu', array( __CLASS__, 'admin_bar' ), 100 );
		add_action( 'admin_post_honey_purge_all', array( __CLASS__, 'purge_all_clicked' ) );
		add_action( 'admin_notices', array( __CLASS__, 'notice' ) );
	}

	/**
	 * Purges $url - or, with $prefix, every URL starting with it - softly,
	 * or with $hard, hard.  Only the path and query string are sent: Honey
	 * purges them for every host.
	 */
	public static function url( $url, $prefix = false, $hard = false ) {
		if ( ! is_string( $url ) || '' === $url ) {
			return;
		}
		$parts = wp_parse_url( $url );
		if ( false === $parts ) {
			return;
		}
		$path = isset( $parts['path'] ) && '' !== $parts['path'] ? $parts['path'] : '/';
		if ( ! empty( $parts['query'] ) ) {
			// e.g. plain permalinks (?p=123): Honey can only purge
			// everything under a path, not a query string
			$path  .= '?' . $parts['query'];
			$prefix = false;
		}
		// A prefix purge of / would purge everything
		if ( $prefix && '/' === $path ) {
			$prefix = false;
		}
		$previous             = isset( self::$paths[ $path ] ) ? self::$paths[ $path ] : array( 'prefix' => false, 'hard' => false );
		self::$paths[ $path ] = array(
			'prefix' => $prefix || $previous['prefix'],
			'hard'   => $hard || $previous['hard'],
		);
		self::schedule();
	}

	/** Purges everything in the cache, softly or, with $hard, hard. */
	public static function everything( $hard = false ) {
		// Hooks pass their own arguments, so only true means hard
		self::$everything = true === $hard || 'hard' === self::$everything ? 'hard' : 'soft';
		self::debug( 'purging everything for ' . ( doing_action() ? current_action() : 'the purge button' ) );
		self::schedule();
	}

	private static function debug( $message ) {
		if ( defined( 'HONEY_DEBUG' ) && HONEY_DEBUG ) {
			error_log( 'Honey: ' . $message . ' (' . ( isset( $_SERVER['REQUEST_URI'] ) ? sanitize_text_field( wp_unslash( $_SERVER['REQUEST_URI'] ) ) : 'cli' ) . ')' );
		}
	}

	private static function schedule() {
		if ( ! self::$scheduled ) {
			self::$scheduled = true;
			add_action( 'shutdown', array( __CLASS__, 'send' ) );
		}
	}

	/** Purges the pages showing $post, softly or, with $hard, hard. */
	public static function post( $post, $hard = false ) {
		$post = get_post( $post );
		if ( ! $post || wp_is_post_revision( $post ) || wp_is_post_autosave( $post ) ) {
			return;
		}
		if ( ! is_post_type_viewable( $post->post_type ) || 'nav_menu_item' === $post->post_type ) {
			return;
		}
		// Its comment pages and paged parts are under it, e.g.
		// /hello-world/comment-page-2/
		self::url( get_permalink( $post ), true, $hard );
		self::home( $hard );
		$archive = get_post_type_archive_link( $post->post_type );
		if ( $archive ) {
			self::url( $archive, true, $hard );
		}
		foreach ( get_object_taxonomies( $post->post_type, 'objects' ) as $taxonomy ) {
			if ( ! $taxonomy->public ) {
				continue;
			}
			$terms = get_the_terms( $post, $taxonomy->name );
			if ( ! is_array( $terms ) ) {
				continue;
			}
			foreach ( $terms as $term ) {
				$link = get_term_link( $term );
				if ( ! is_wp_error( $link ) ) {
					self::url( $link, true, $hard );
				}
			}
		}
		if ( post_type_supports( $post->post_type, 'author' ) ) {
			self::url( get_author_posts_url( (int) $post->post_author ), true, $hard );
		}
		if ( 'post' === $post->post_type ) {
			$time = strtotime( $post->post_date );
			if ( $time ) {
				// Not under the year: with date-based permalinks, that
				// would be every post of the year
				self::url( get_year_link( gmdate( 'Y', $time ) ), false, $hard );
				self::url( get_month_link( gmdate( 'Y', $time ), gmdate( 'm', $time ) ), false, $hard );
				self::url( get_day_link( gmdate( 'Y', $time ), gmdate( 'm', $time ), gmdate( 'd', $time ) ), false, $hard );
			}
		}
		self::url( get_feed_link(), false, $hard );
		self::url( get_feed_link( 'comments_' . get_default_feed() ), false, $hard );
		self::url( home_url( '/wp-sitemap' ), true, $hard );
		if ( 'post' === $post->post_type ) {
			// The lists of recent posts on every page (see Fragments)
			self::fragment( 'posts-*' );
		}
	}

	/**
	 * Purges the shared fragments called $name (or, ending with *, whose
	 * names start with what precedes it).  Honey updates every cached page
	 * showing them.
	 */
	public static function fragment( $name ) {
		self::$fragments[ $name ] = true;
		self::schedule();
	}

	/** Purges the home page (and its other pages), and the posts page. */
	private static function home( $hard ) {
		self::url( home_url( '/' ), false, $hard );
		self::url( home_url( '/page/' ), true, $hard );
		$posts_page = (int) get_option( 'page_for_posts' );
		if ( $posts_page ) {
			self::url( get_permalink( $posts_page ), true, $hard );
		}
	}

	public static function post_status_changed( $new_status, $old_status, $post ) {
		if ( 'publish' === $new_status || 'publish' === $old_status ) {
			// Hard if it is no longer public: it mustn't be served again
			self::post( $post, 'publish' !== $new_status || '' !== $post->post_password );
		}
	}

	public static function post_updated( $post_id, $after, $before ) {
		// The post's new address is purged by post_status_changed; this
		// purges its old one, if it moved
		if ( 'publish' !== $before->post_status ) {
			return;
		}
		$old = get_permalink( $before );
		if ( $old && get_permalink( $after ) !== $old ) {
			self::url( $old, true, true );
		}
	}

	public static function post_deleted( $post_id, $post = null ) {
		$post = $post ? $post : get_post( $post_id );
		if ( $post && 'publish' === $post->post_status ) {
			self::post( $post, true );
		}
	}

	private static function comment_changed( $comment, $hard = false ) {
		$comment = get_comment( $comment );
		if ( $comment && $comment->comment_post_ID ) {
			self::url( get_permalink( (int) $comment->comment_post_ID ), true, $hard );
		}
	}

	public static function comment_posted( $comment_id, $approved ) {
		if ( 1 === $approved || '1' === $approved ) {
			self::comment_changed( $comment_id );
		}
	}

	public static function comment_edited( $comment_id ) {
		if ( 'approved' === wp_get_comment_status( $comment_id ) ) {
			self::comment_changed( $comment_id );
		}
	}

	public static function comment_status_changed( $new_status, $old_status, $comment ) {
		if ( 'approved' === $new_status || 'approved' === $old_status ) {
			// Hard if it was removed (e.g. as spam)
			self::comment_changed( $comment, 'approved' !== $new_status );
		}
	}

	public static function comment_deleted( $comment_id, $comment = null ) {
		if ( $comment && '1' === $comment->comment_approved ) {
			self::comment_changed( $comment, true );
		}
	}

	public static function term_changed( $term_id, $tt_id = 0, $taxonomy = '' ) {
		self::term( $term_id, $taxonomy, false );
	}

	public static function term_deleted( $term_id, $taxonomy ) {
		self::term( $term_id, $taxonomy, true );
	}

	private static function term( $term_id, $taxonomy, $hard ) {
		$link = get_term_link( (int) $term_id, $taxonomy );
		if ( ! is_wp_error( $link ) ) {
			self::url( $link, true, $hard );
		}
	}

	/** Purges everything when a setting shown on every page changes. */
	public static function option_updated( $option ) {
		static $everywhere = array(
			'blogname',
			'blogdescription',
			'permalink_structure',
			'category_base',
			'tag_base',
			'show_on_front',
			'page_on_front',
			'page_for_posts',
			'posts_per_page',
			'date_format',
			'time_format',
			'site_icon',
			'sidebars_widgets',
			'stylesheet',
			'template',
		);
		if ( in_array( $option, $everywhere, true ) ||
			0 === strpos( $option, 'widget_' ) ||
			0 === strpos( $option, 'theme_mods_' ) ) {
			self::debug( "the $option option changed" );
			self::everything();
		}
	}

	/** Sends the purges.  It runs once WordPress has handled the request. */
	public static function send() {
		// path => whether to purge it softly
		$purges = array();
		if ( 'hard' !== self::$everything ) {
			foreach ( self::$paths as $path => $purge ) {
				if ( 'soft' === self::$everything && ! $purge['hard'] ) {
					continue; // purging everything softly covers it
				}
				// Honey purges everything starting with what precedes a *
				$purges[ $purge['prefix'] ? $path . '*' : $path ] = ! $purge['hard'];
			}
		}
		if ( self::$everything ) {
			$purges['/*'] = 'soft' === self::$everything;
		}
		// Purging everything covers the fragments too
		$fragments        = self::$everything ? array() : array_keys( self::$fragments );
		self::$paths      = array();
		self::$fragments  = array();
		self::$everything = null;

		$base    = untrailingslashit( defined( 'HONEY_URL' ) ? HONEY_URL : self::origin() );
		$headers = array();
		if ( defined( 'HONEY_SECRET' ) && HONEY_SECRET ) {
			$header             = defined( 'HONEY_SECRET_HEADER' ) && HONEY_SECRET_HEADER ? HONEY_SECRET_HEADER : 'X-Honey-Secret';
			$headers[ $header ] = HONEY_SECRET;
		}
		foreach ( $purges as $path => $soft ) {
			self::debug( ( $soft ? 'soft' : 'hard' ) . " purging $path" );
			$response = wp_remote_request(
				$base . $path,
				array(
					'method'      => 'PURGE',
					'headers'     => $soft ? array_merge( $headers, array( 'X-Honey-Purge' => 'soft' ) ) : $headers,
					'timeout'     => 3,
					'redirection' => 0,
				)
			);
			if ( is_wp_error( $response ) ) {
				error_log( sprintf( 'Honey: purging %s failed: %s', $path, $response->get_error_message() ) );
				// Honey is probably unreachable: don't wait on it again
				return;
			}
			$code = wp_remote_retrieve_response_code( $response );
			if ( 200 !== $code ) {
				error_log( sprintf( 'Honey: purging %s failed: HTTP %d (is HONEY_SECRET set to Honey\'s control.secret?)', $path, $code ) );
				return;
			}
		}
		if ( $fragments ) {
			$names = implode( ',', $fragments );
			self::debug( "purging the shared fragments $names" );
			$response = wp_remote_request(
				$base . '/',
				array(
					'method'      => 'PURGE',
					'headers'     => array_merge( $headers, array( 'X-Honey-Fragment' => $names ) ),
					'timeout'     => 3,
					'redirection' => 0,
				)
			);
			if ( is_wp_error( $response ) || 200 !== wp_remote_retrieve_response_code( $response ) ) {
				error_log( sprintf( 'Honey: purging the shared fragments %s failed', $names ) );
			}
		}
	}

	/** The scheme and host of the site, e.g. https://www.example.com */
	private static function origin() {
		$parts  = wp_parse_url( home_url() );
		$origin = $parts['scheme'] . '://' . $parts['host'];
		if ( ! empty( $parts['port'] ) ) {
			$origin .= ':' . $parts['port'];
		}
		return $origin;
	}

	public static function admin_bar( $bar ) {
		if ( ! current_user_can( 'manage_options' ) ) {
			return;
		}
		$url = wp_nonce_url( admin_url( 'admin-post.php?action=honey_purge_all' ), 'honey_purge_all' );
		$bar->add_node(
			array(
				'id'    => 'honey-purge',
				'title' => __( 'Purge cache' ),
				'href'  => $url,
				'meta'  => array( 'title' => __( 'Purge everything from the Honey cache' ) ),
			)
		);
	}

	public static function purge_all_clicked() {
		if ( ! current_user_can( 'manage_options' ) ) {
			wp_die( esc_html__( 'Sorry, you are not allowed to do that.' ), 403 );
		}
		check_admin_referer( 'honey_purge_all' );
		self::everything( true );
		$back = wp_get_referer();
		if ( ! $back || false === strpos( $back, admin_url() ) ) {
			$back = admin_url();
		}
		wp_safe_redirect( add_query_arg( 'honey-purged', '1', $back ) );
		exit;
	}

	public static function notice() {
		// phpcs:ignore WordPress.Security.NonceVerification.Recommended -- only shows a message
		if ( isset( $_GET['honey-purged'] ) && current_user_can( 'manage_options' ) ) {
			echo '<div class="notice notice-success is-dismissible"><p>' . esc_html__( 'The Honey cache has been purged.' ) . '</p></div>';
		}
	}
}

/**
 * Marks the parts of pages which are the same on every page, such as lists
 * of recent posts, as shared fragments, for Honey (see Purge).
 */
final class Fragments {
	/** @var string the token Honey sent, which the markers must have */
	private static $token = '';

	public static function init() {
		// Only when Honey asks, for a page it's going to cache
		$token = isset( $_SERVER['HTTP_X_HONEY_FRAGMENTS'] ) ? sanitize_text_field( wp_unslash( $_SERVER['HTTP_X_HONEY_FRAGMENTS'] ) ) : '';
		if ( ! preg_match( '/^[0-9]{1,20}-[0-9a-f]{32}$/', $token ) ) {
			return;
		}
		self::$token = $token;
		add_filter( 'render_block', array( __CLASS__, 'block' ), 10, 2 );
		add_filter( 'widget_display_callback', array( __CLASS__, 'widget' ), 10, 3 );
	}

	/** Marks the Latest Posts block (and any others in the filter). */
	public static function block( $content, $block ) {
		$blocks = apply_filters( 'honey_fragment_blocks', array( 'core/latest-posts' ) );
		if ( empty( $block['blockName'] ) || ! in_array( $block['blockName'], (array) $blocks, true ) ) {
			return $content;
		}
		// Blocks with the same settings show the same posts
		$attrs = isset( $block['attrs'] ) ? $block['attrs'] : array();
		return self::mark( 'posts-block-' . substr( md5( $block['blockName'] . wp_json_encode( $attrs ) ), 0, 12 ), $content );
	}

	/** Marks the Recent Posts widget, by showing it itself. */
	public static function widget( $instance, $widget, $args ) {
		if ( false === $instance || ! is_object( $widget ) || 'recent-posts' !== $widget->id_base ) {
			return $instance;
		}
		ob_start();
		$widget->widget( $args, $instance );
		// phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped -- the widget's own output
		echo self::mark( 'posts-widget-' . sanitize_key( $widget->id ), ob_get_clean() );
		return false; // it has been shown
	}

	private static function mark( $name, $html ) {
		// A list which differs from page to page (e.g. marking the current
		// post) can't be shared
		if ( '' === trim( $html ) || false !== strpos( $html, 'aria-current' ) || ! preg_match( '/^[a-z0-9][a-z0-9_.-]{0,99}$/', $name ) ) {
			return $html;
		}
		return '<!--honey:fragment name="' . $name . '" token="' . self::$token . '"-->' . $html . '<!--/honey:fragment-->';
	}
}

/**
 * Serves the Comments block as an Edge Side Include, so that pages with
 * comments can be cached for commenters too (see above).
 */
final class ESI {
	/** The query parameter naming what an include shows (see Honey's fetch.ESIParam) */
	const PARAM = 'honey-esi';

	/** @var bool whether includes are being rendered, not replaced */
	private static $rendering = false;

	public static function init() {
		add_action( 'template_redirect', array( __CLASS__, 'serve' ), 0 );
		if ( self::capable() ) {
			add_filter( 'render_block', array( __CLASS__, 'block' ), 10, 3 );
		}
	}

	/**
	 * Whether Honey has asked for a page which uses ESI: only for a page
	 * it's going to cache, rather than an include.
	 */
	public static function capable() {
		$capability = isset( $_SERVER['HTTP_SURROGATE_CAPABILITY'] ) ? sanitize_text_field( wp_unslash( $_SERVER['HTTP_SURROGATE_CAPABILITY'] ) ) : '';
		return false !== strpos( $capability, 'ESI/1.0' ) && '' === self::name();
	}

	/** The name of the include being requested, if one is. */
	public static function name() {
		return isset( $_GET[ self::PARAM ] ) ? sanitize_text_field( wp_unslash( $_GET[ self::PARAM ] ) ) : ''; // phpcs:ignore WordPress.Security.NonceVerification.Recommended
	}

	/** Replaces the Comments block of the page's own post with an include. */
	public static function block( $content, $block, $instance = null ) {
		if ( self::$rendering || empty( $block['blockName'] ) || 'core/comments' !== $block['blockName'] ) {
			return $content;
		}
		// The include shows the comments of the page's post: so only on the
		// post's own page, and for the block showing its comments
		$post_id = ( $instance instanceof \WP_Block && isset( $instance->context['postId'] ) ) ? (int) $instance->context['postId'] : get_the_ID();
		if ( ! is_singular() || $post_id !== get_queried_object_id() || headers_sent() ) {
			return $content;
		}
		// Kept to show it later: the same blocks have the same id
		$markup = serialize_block( $block );
		$id     = substr( md5( $markup ), 0, 16 );
		if ( false === get_option( 'honey_esi_' . $id ) ) {
			add_option( 'honey_esi_' . $id, $markup, '', false );
		}
		header( 'Surrogate-Control: content="ESI/1.0"' );
		// For commenters (see Honey's X-Honey-Esi-Cookies)
		header( 'X-Honey-Esi-Cookies: comment_author_', false );
		// The same address, so that the include has the same post and comment page
		$uri = isset( $_SERVER['REQUEST_URI'] ) ? wp_unslash( $_SERVER['REQUEST_URI'] ) : '/'; // phpcs:ignore WordPress.Security.ValidatedSanitizedInput.InputNotSanitized
		$src = add_query_arg( self::PARAM, 'comments.' . $id, $uri );
		return '<esi:include src="' . esc_attr( $src ) . '"/>';
	}

	/** Shows just the Comments block, for an include. */
	public static function serve() {
		$name = self::name();
		if ( '' === $name ) {
			return;
		}
		$markup = preg_match( '/^comments\.([0-9a-f]{16})$/', $name, $m ) && is_singular() ? get_option( 'honey_esi_' . $m[1] ) : false;
		if ( ! is_string( $markup ) ) {
			status_header( 404 );
			exit;
		}
		if ( have_posts() ) {
			the_post();
		}
		self::$rendering = true;
		header( 'Content-Type: text/html; charset=' . get_option( 'blog_charset' ) );
		// phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped -- the block's own output
		echo do_blocks( $markup );
		exit;
	}
}

/**
 * Lets WooCommerce's shoppers be served cached pages, with their cart and
 * notices added (see above).
 */
final class Shoppers {
	/** Shoppers' bypass cookies (see Honey's cache.bypassCookies) */
	const COOKIES = 'woocommerce_items_in_cart, wp_woocommerce_session_';

	/** Puts the shopper's cart into the page's state, and their notices into its notices area. */
	const SCRIPT = '(function(c,n){var d=document.getElementById("wp-script-module-data-@wordpress/interactivity");if(c&&d){try{var j=JSON.parse(d.textContent),s=j.state&&j.state.woocommerce;if(s&&s.cart){s.cart=c;d.textContent=JSON.stringify(j)}}catch(e){}}var w=n&&document.querySelector(".woocommerce-notices-wrapper");if(w){w.innerHTML=n}})';

	/** @var bool whether the page shows store notices */
	private static $notices = false;

	public static function init() {
		if ( 0 === strpos( ESI::name(), 'woocommerce.' ) ) {
			// Once WooCommerce has loaded the shopper's cart, without
			// rendering a page
			add_action( 'wp_loaded', array( __CLASS__, 'serve' ), PHP_INT_MAX );
		} elseif ( ESI::capable() ) {
			add_action( 'template_redirect', array( __CLASS__, 'page' ), PHP_INT_MAX );
		}
	}

	/** Says the page is for shoppers too, and looks out for what's theirs on it. */
	public static function page() {
		$method = isset( $_SERVER['REQUEST_METHOD'] ) ? sanitize_text_field( wp_unslash( $_SERVER['REQUEST_METHOD'] ) ) : 'GET';
		// Classic themes show the cart in their own ways
		if ( ! function_exists( 'WC' ) || ! wp_is_block_theme() || ! in_array( $method, array( 'GET', 'HEAD' ), true ) || headers_sent() || is_user_logged_in() ) {
			return;
		}
		// Pages which are all about the shopper's cart
		if ( is_cart() || is_checkout() || is_account_page() ) {
			return;
		}
		// Honey doesn't cache pages for shoppers, but in case
		if ( WC()->session && WC()->session->has_session() ) {
			return;
		}
		header( 'Surrogate-Control: content="ESI/1.0"' );
		header( 'X-Honey-Esi-Cookies: ' . self::COOKIES, false );
		// Only wc_print_notices() uses it
		add_filter( 'woocommerce_notice_types', array( __CLASS__, 'shows_notices' ) );
		// After the Interactivity API's state
		add_action( 'wp_footer', array( __CLASS__, 'footer' ), PHP_INT_MAX );
	}

	public static function shows_notices( $types ) {
		self::$notices = true;
		return $types;
	}

	/** Ends the page with an include of the shopper's cart and notices, if it shows them. */
	public static function footer() {
		$parts = array();
		$state = function_exists( 'wp_interactivity_state' ) ? wp_interactivity_state( 'woocommerce' ) : array();
		if ( isset( $state['cart'] ) ) {
			$parts[] = 'cart';
		}
		if ( self::$notices ) {
			$parts[] = 'notices';
		}
		if ( ! $parts ) {
			return;
		}
		$src = add_query_arg( ESI::PARAM, 'woocommerce.' . implode( '.', $parts ), wp_make_link_relative( home_url( '/' ) ) );
		// In a comment, so that visitors who aren't shoppers get the page as it is
		echo '<!--esi <esi:include src="' . esc_attr( $src ) . '"/> -->';
	}

	/** Shows the shopper's cart and notices, for an include. */
	public static function serve() {
		if ( ! preg_match( '/^woocommerce((\.cart|\.notices)+)$/', ESI::name(), $m ) ) {
			status_header( 404 );
			exit;
		}
		$parts = explode( '.', ltrim( $m[1], '.' ) );
		header( 'Content-Type: text/html; charset=' . get_option( 'blog_charset' ) );
		if ( ! function_exists( 'WC' ) || ! WC()->session || ! WC()->session->has_session() ) {
			// Not a shopper: nothing to add, for anyone
			exit;
		}
		nocache_headers();
		$cart    = in_array( 'cart', $parts, true ) ? self::cart() : null;
		$notices = in_array( 'notices', $parts, true ) && function_exists( 'wc_print_notices' ) ? (string) wc_print_notices( true ) : '';
		if ( null === $cart && '' === $notices ) {
			exit;
		}
		$flags = JSON_HEX_TAG | JSON_HEX_AMP | JSON_UNESCAPED_SLASHES;
		// phpcs:ignore WordPress.Security.EscapeOutput.OutputNotEscaped -- JSON, safe in a script
		echo '<script>' . self::SCRIPT . '(' . wp_json_encode( $cart, $flags ) . ',' . wp_json_encode( $notices, $flags ) . ')</script>';
		exit;
	}

	/** The shopper's cart, as WooCommerce puts it in a page's state. */
	private static function cart() {
		// Without a leading backslash, as WooCommerce's container names them
		$package   = 'Automattic\WooCommerce\Blocks\Package';
		$hydration = 'Automattic\WooCommerce\Blocks\Domain\Services\Hydration';
		if ( ! class_exists( $package ) || ! class_exists( $hydration ) ) {
			return null;
		}
		try {
			$data = $package::container()->get( $hydration )->get_rest_api_response_data( '/wc/store/v1/cart' );
		} catch ( \Throwable $e ) {
			return null;
		}
		return isset( $data['body'] ) ? $data['body'] : null;
	}
}

/**
 * Hides the site's users from visitors who aren't logged in, when Honey
 * asks (see above).
 */
final class Users {
	public static function init() {
		if ( empty( $_SERVER['HTTP_X_HONEY_HIDE_USERS'] ) ) {
			return;
		}
		// Before redirect_canonical sends ?author=N to the author's archive
		add_action( 'template_redirect', array( __CLASS__, 'author_query' ), 0 );
		// After the REST API has checked who the visitor is
		add_filter( 'rest_pre_dispatch', array( __CLASS__, 'rest' ), 10, 3 );
		add_filter( 'wp_sitemaps_add_provider', array( __CLASS__, 'sitemap' ), 10, 2 );
	}

	/**
	 * Makes ?author=N a 404, and the authors' sitemap (which WordPress would
	 * otherwise answer with the home page, once it's left out).
	 */
	public static function author_query() {
		if ( is_user_logged_in() ) {
			return;
		}
		if ( ! isset( $_GET['author'] ) && 'users' !== get_query_var( 'sitemap' ) ) { // phpcs:ignore WordPress.Security.NonceVerification.Recommended
			return;
		}
		global $wp_query;
		$wp_query->set_404();
		status_header( 404 );
		nocache_headers();
	}

	/** Refuses to list users, or show one, through the REST API. */
	public static function rest( $result, $server, $request ) {
		if ( null !== $result || is_user_logged_in() ) {
			return $result;
		}
		if ( preg_match( '#^/wp/v2/users(/|$)#i', $request->get_route() ) ) {
			return new \WP_Error( 'rest_no_route', 'No route was found matching the URL and request method.', array( 'status' => 404 ) );
		}
		return $result;
	}

	/** Leaves the sitemap of authors out. */
	public static function sitemap( $provider, $name ) {
		return 'users' === $name ? false : $provider;
	}
}

Purge::init();
Fragments::init();
ESI::init();
Shoppers::init();
Users::init();
