// Package guard protects WordPress from the requests which attack almost
// every WordPress site: probes for its configuration, backups and logs,
// running PHP uploaded with media, and brute-forcing passwords through
// XML-RPC.  It also sets security headers on responses, and removes the
// ones which say which versions of software the site runs.
package guard

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/davidjwilkins/honey/clientip"
	"github.com/davidjwilkins/honey/events"
)

// XML-RPC modes (see Options.XMLRPC)
const (
	// XMLRPCSafe blocks the XML-RPC methods which are abused: brute-forcing
	// many passwords in one request (system.multicall), and pingbacks
	// (which can be used to attack other sites)
	XMLRPCSafe = "safe"
	// XMLRPCOff blocks XML-RPC entirely
	XMLRPCOff = "off"
	// XMLRPCOn allows it all
	XMLRPCOn = "on"
)

// BlockedHeader is the X-Honey-Cache header of blocked requests
const BlockedHeader = "BLOCKED"

// maxXMLRPCBytes is the largest XML-RPC request which is read to check its
// method: larger ones are refused in safe mode
const maxXMLRPCBytes = 16 << 20

// Options configure the guard
type Options struct {
	// BlockPaths blocks requests for sensitive files (see blockedPath)
	BlockPaths bool
	// XMLRPC is XMLRPCSafe, XMLRPCOff or XMLRPCOn ("" is XMLRPCOn)
	XMLRPC string
	// Headers sets security headers on responses, and removes those which
	// give away software versions
	Headers bool
	// HSTS, if set, is the Strict-Transport-Security header sent with
	// responses to requests which came over HTTPS
	HSTS string
	// AdminIPs, if not nil, are the only addresses the dashboard and login
	// page are served to (see dashboard)
	AdminIPs []*net.IPNet
	// HideUsers blocks anonymous requests which list the site's users (see
	// listsUsers)
	HideUsers bool
	// Limits, if set, locks out addresses which keep failing to log in
	// (and limits their requests to the backend: see Limits.AllowBackend)
	Limits *Limits
	// Events, if set, is where blocked requests are logged
	Events *events.Log
}

var (
	// wpConfig is WordPress's configuration, and copies of it (e.g.
	// wp-config.php.bak, wp-config.old, wp-config.php~)
	wpConfig = regexp.MustCompile(`(?i)(^|/)wp-config[^/]*$`)
	// backups are files left behind by editors, backups and logs (e.g.
	// wp-content/debug.log, or a database dump)
	backups = regexp.MustCompile(`(?i)(\.(bak|old|orig|save|swp|swo|sql|sql\.gz|sql\.zip|log)|~)$`)
	// uploadedPHP is PHP in the directories media is uploaded to, which
	// should only ever have files served as they are - including names
	// with a .php part (e.g. shell.php.jpg), which some servers run
	uploadedPHP = regexp.MustCompile(`(?i)^/wp-content/(uploads|blogs\.dir)/.*\.(php\d*|pht|phtml|phar|phps)(\.|/|$)`)
	// installer is WordPress's installer, and its readme (which says its
	// version)
	installer = regexp.MustCompile(`(?i)^/(wp-admin/(install|setup-config)\.php|readme\.html)$`)
	// exploits are what scanners try on every site, which are never part
	// of a WordPress site: PHPUnit's eval-stdin.php (left in vendor
	// directories), CGI scripts, router and Java exploits, and the usual
	// names of web shells
	exploits = regexp.MustCompile(`(?i)(/eval-stdin\.php$|^/cgi-bin/|^/(actuator|boaform|hnap1|solr|owa|remote/fgt_lang)(/|$)|^/(shell|wso|alfa|c99|r57|b374k|webshell|cmd|up|uploader)\.php$)`)
)

// blockedPath returns why requests for p are blocked, or "" if they aren't
func blockedPath(p string) string {
	p = path.Clean("/" + p)
	for _, segment := range strings.Split(p, "/") {
		// .git, .env, .htaccess, .DS_Store... but not .well-known
		if strings.HasPrefix(segment, ".") && segment != ".well-known" {
			return "hidden files"
		}
	}
	switch {
	case wpConfig.MatchString(p):
		return "WordPress's configuration"
	case uploadedPHP.MatchString(p):
		return "PHP in uploads"
	case backups.MatchString(p):
		return "backups and logs"
	case installer.MatchString(p):
		return "the installer"
	case exploits.MatchString(p):
		return "known exploits"
	}
	return ""
}

// Middleware returns next, guarded as opts say
func Middleware(opts Options, next http.Handler) http.Handler {
	if opts.XMLRPC == "" {
		opts.XMLRPC = XMLRPCOn
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PURGE" {
			// Purges are for Honey (see fetch.Control), not WordPress
			next.ServeHTTP(w, r)
			return
		}
		// Visitors never ask for what blockPaths and XML-RPC's safe mode
		// block, so those who do are probing for weaknesses
		if opts.BlockPaths {
			if why := blockedPath(r.URL.Path); why != "" {
				opts.Limits.Probed(r)
				block(w, r, opts, http.StatusForbidden, why, "security.blockPaths")
				return
			}
		}
		if isXMLRPC(r.URL.Path) {
			switch opts.XMLRPC {
			case XMLRPCOff:
				block(w, r, opts, http.StatusForbidden, "XML-RPC", `security.xmlrpc = "off"`)
				return
			case XMLRPCSafe:
				if why, status := checkXMLRPC(r); why != "" {
					opts.Limits.Probed(r)
					block(w, r, opts, status, why, `security.xmlrpc = "safe"`)
					return
				}
			}
		}
		if opts.AdminIPs != nil && dashboard(r) && !allowedIP(r, opts.AdminIPs) {
			block(w, r, opts, http.StatusForbidden, "the dashboard from other addresses", "security.adminIPs")
			return
		}
		if opts.HideUsers {
			if why := listsUsers(r); why != "" {
				block(w, r, opts, http.StatusNotFound, why, "security.hideUsers")
				return
			}
		}
		if opts.Headers || opts.HSTS != "" {
			w = &headerWriter{ResponseWriter: w, r: r, opts: opts}
		}
		kind := loginKind(r)
		if kind == "" || opts.Limits == nil {
			next.ServeHTTP(w, r)
			return
		}
		if left, locked := opts.Limits.LockedOut(r); locked {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(left.Seconds()))))
			block(w, r, opts, http.StatusTooManyRequests, "logins from locked out addresses", "security.loginAttempts")
			return
		}
		lw := &loginWriter{ResponseWriter: w, kind: kind}
		next.ServeHTTP(lw, r)
		switch failed, succeeded := lw.outcome(); {
		case failed:
			opts.Limits.LoginFailed(r)
		case succeeded:
			opts.Limits.LoginSucceeded(r)
		}
	})
}

// xmlrpcPath is WordPress's XML-RPC endpoint: in any directory (for
// WordPress in a subdirectory), and with anything after it, which servers
// pass to it as PATH_INFO
var xmlrpcPath = regexp.MustCompile(`(?i)/xmlrpc\.php(/|$)`)

func isXMLRPC(p string) bool {
	return xmlrpcPath.MatchString(path.Clean("/" + p))
}

// blockedMethods are the XML-RPC methods blocked in safe mode
var blockedMethods = map[string]string{
	"system.multicall": "XML-RPC system.multicall (password brute-forcing)",
	"pingback.ping":    "XML-RPC pingbacks",
}

// checkXMLRPC returns why an XML-RPC request is blocked in safe mode, and
// the status to block it with, or "" if it isn't.  Its body is read (and
// put back) to find the methods it calls.
func checkXMLRPC(r *http.Request) (string, int) {
	if r.Method != http.MethodPost {
		return "", 0
	}
	if r.Header.Get("Content-Encoding") != "" {
		// XML-RPC clients don't compress requests, and the method can't be
		// checked in one which is
		return "compressed XML-RPC requests", http.StatusUnsupportedMediaType
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxXMLRPCBytes+1))
	r.Body.Close()
	if err != nil {
		return "unreadable XML-RPC requests", http.StatusBadRequest
	}
	if len(body) > maxXMLRPCBytes {
		return "XML-RPC requests over 16MB", http.StatusRequestEntityTooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	methods, err := methodNames(body)
	if err != nil {
		// WordPress's parser might make something of it
		return "malformed XML-RPC requests", http.StatusBadRequest
	}
	for _, method := range methods {
		if why, blocked := blockedMethods[strings.ToLower(method)]; blocked {
			return why, http.StatusForbidden
		}
	}
	return "", 0
}

// methodNames returns the content of every <methodName> element in an
// XML-RPC request (WordPress calls the last, so any of them could be the
// one called), as an XML parser sees them: with entities decoded, and
// comments and CDATA sections handled.  Names are compared as WordPress's
// parser (which ignores namespaces) sees them: so <methodName> in a
// default namespace counts, and <x:methodName> doesn't.
func methodNames(body []byte) ([]string, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = false
	d.Entity = xml.HTMLEntity
	var names []string
	var in bool
	var name strings.Builder
	for {
		tok, err := d.RawToken()
		if err == io.EOF {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if strings.EqualFold(t.Name.Local, "methodName") && t.Name.Space == "" {
				in = true
				name.Reset()
			}
		case xml.CharData:
			if in {
				name.Write(t)
			}
		case xml.EndElement:
			if in && strings.EqualFold(t.Name.Local, "methodName") && t.Name.Space == "" {
				in = false
				// As PHP's trim() does
				names = append(names, strings.Trim(name.String(), " \t\n\r\x00\x0b"))
			}
		}
	}
}

// block refuses r with status, and logs it
func block(w http.ResponseWriter, r *http.Request, opts Options, status int, why, setting string) {
	from := "a visitor"
	if ip := clientip.From(r); ip != nil {
		from = ip.String()
	} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		from = host
	}
	uri := r.URL.RequestURI()
	if len(uri) > 200 {
		uri = uri[:200] + "..."
	}
	opts.Events.Add(events.Refused, "guard "+why, 10*time.Minute, func(e *events.Event) {
		e.Text = fmt.Sprintf("blocked %d %s for %s, most recently %s %q from %s",
			e.Count, plural(e.Count), why, r.Method, uri, from)
	})
	w.Header().Set("X-Honey-Cache", BlockedHeader)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "%s: blocked by Honey (%s)\n", http.StatusText(status), setting)
}

func plural(n int) string {
	if n == 1 {
		return "request"
	}
	return "requests"
}
