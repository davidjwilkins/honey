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
 * Purges are sent when WordPress has finished handling the request, once
 * each, as PURGE requests (see Honey's README).
 */

namespace Honey;

if ( ! defined( 'ABSPATH' ) ) {
	exit;
}

final class Purge {
	/** @var array<string, bool> paths to purge => whether to purge everything under them */
	private static $paths = array();

	/** @var bool whether to purge everything */
	private static $everything = false;

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
	 * Purges $url - or, with $prefix, every URL starting with it.  Only
	 * the path and query string are sent: Honey replaces the host with
	 * its backend's, as it does for every request.
	 */
	public static function url( $url, $prefix = false ) {
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
		self::$paths[ $path ] = $prefix || ! empty( self::$paths[ $path ] );
		self::schedule();
	}

	/** Purges everything in the cache. */
	public static function everything() {
		self::$everything = true;
		self::schedule();
	}

	private static function schedule() {
		if ( ! self::$scheduled ) {
			self::$scheduled = true;
			add_action( 'shutdown', array( __CLASS__, 'send' ) );
		}
	}

	/** Purges the pages showing $post. */
	public static function post( $post ) {
		$post = get_post( $post );
		if ( ! $post || wp_is_post_revision( $post ) || wp_is_post_autosave( $post ) ) {
			return;
		}
		if ( ! is_post_type_viewable( $post->post_type ) || 'nav_menu_item' === $post->post_type ) {
			return;
		}
		// Its comment pages and paged parts are under it, e.g.
		// /hello-world/comment-page-2/
		self::url( get_permalink( $post ), true );
		self::home();
		$archive = get_post_type_archive_link( $post->post_type );
		if ( $archive ) {
			self::url( $archive, true );
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
					self::url( $link, true );
				}
			}
		}
		if ( post_type_supports( $post->post_type, 'author' ) ) {
			self::url( get_author_posts_url( (int) $post->post_author ), true );
		}
		if ( 'post' === $post->post_type ) {
			$time = strtotime( $post->post_date );
			if ( $time ) {
				// Not under the year: with date-based permalinks, that
				// would be every post of the year
				self::url( get_year_link( gmdate( 'Y', $time ) ) );
				self::url( get_month_link( gmdate( 'Y', $time ), gmdate( 'm', $time ) ) );
				self::url( get_day_link( gmdate( 'Y', $time ), gmdate( 'm', $time ), gmdate( 'd', $time ) ) );
			}
		}
		self::url( get_feed_link() );
		self::url( get_feed_link( 'comments_' . get_default_feed() ) );
		self::url( home_url( '/wp-sitemap' ), true );
	}

	/** Purges the home page (and its other pages), and the posts page. */
	private static function home() {
		self::url( home_url( '/' ) );
		self::url( home_url( '/page/' ), true );
		$posts_page = (int) get_option( 'page_for_posts' );
		if ( $posts_page ) {
			self::url( get_permalink( $posts_page ), true );
		}
	}

	public static function post_status_changed( $new_status, $old_status, $post ) {
		if ( 'publish' === $new_status || 'publish' === $old_status ) {
			self::post( $post );
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
			self::url( $old, true );
		}
	}

	public static function post_deleted( $post_id, $post = null ) {
		$post = $post ? $post : get_post( $post_id );
		if ( $post && 'publish' === $post->post_status ) {
			self::post( $post );
		}
	}

	private static function comment_changed( $comment ) {
		$comment = get_comment( $comment );
		if ( $comment && $comment->comment_post_ID ) {
			self::url( get_permalink( (int) $comment->comment_post_ID ), true );
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
			self::comment_changed( $comment );
		}
	}

	public static function comment_deleted( $comment_id, $comment = null ) {
		if ( $comment && '1' === $comment->comment_approved ) {
			self::comment_changed( $comment );
		}
	}

	public static function term_changed( $term_id, $tt_id = 0, $taxonomy = '' ) {
		$link = get_term_link( (int) $term_id, $taxonomy );
		if ( ! is_wp_error( $link ) ) {
			self::url( $link, true );
		}
	}

	public static function term_deleted( $term_id, $taxonomy ) {
		self::term_changed( $term_id, 0, $taxonomy );
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
			self::everything();
		}
	}

	/** Sends the purges.  It runs once WordPress has handled the request. */
	public static function send() {
		$paths = array();
		if ( self::$everything ) {
			$paths[] = '/*';
		} else {
			foreach ( self::$paths as $path => $prefix ) {
				// Honey purges everything starting with what precedes a *
				$paths[] = $prefix ? $path . '*' : $path;
			}
		}
		self::$paths      = array();
		self::$everything = false;

		$base    = untrailingslashit( defined( 'HONEY_URL' ) ? HONEY_URL : self::origin() );
		$headers = array();
		if ( defined( 'HONEY_SECRET' ) && HONEY_SECRET ) {
			$header             = defined( 'HONEY_SECRET_HEADER' ) && HONEY_SECRET_HEADER ? HONEY_SECRET_HEADER : 'X-Honey-Secret';
			$headers[ $header ] = HONEY_SECRET;
		}
		foreach ( array_unique( $paths ) as $path ) {
			$response = wp_remote_request(
				$base . $path,
				array(
					'method'      => 'PURGE',
					'headers'     => $headers,
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
		self::everything();
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

Purge::init();
