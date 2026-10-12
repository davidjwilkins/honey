package guard

import (
	"container/list"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/clientip"
	"github.com/davidjwilkins/honey/events"
)

// maxAddresses bounds how many addresses Limits keeps track of: the least
// recently seen are forgotten first
const maxAddresses = 100000

// LimitOptions configure Limits
type LimitOptions struct {
	// LoginAttempts is how many failed logins an address may make within
	// LoginLockout before it's locked out (from logging in) for
	// LoginLockout.  Zero turns lockouts off.
	LoginAttempts int
	LoginLockout  time.Duration
	// Rate is how many requests a second an address may send to the
	// backend (cache hits don't count), with bursts of up to Burst.  Zero
	// turns rate limiting off.
	Rate  float64
	Burst int
	// ProbeLimit is how many probes (requests for sensitive files and
	// known exploits: see isProbe) an address may make within
	// probeWindow before it's banned from the backend for ProbeBan.  Zero
	// turns bans off.
	ProbeLimit int
	ProbeBan   time.Duration
	// Allow are addresses which are never locked out or limited
	Allow []*net.IPNet
	// Events, if set, is where lockouts and limiting are logged
	Events *events.Log
	// now is the time, for tests
	now func() time.Time
}

// Limits keeps track of addresses' failed logins and requests to the
// backend, to lock out those which keep failing to log in (e.g. guessing
// passwords) and limit those sending too many requests which the cache
// can't answer.  Addresses are IPv4 addresses and IPv6 /64 networks (as one
// computer usually has a whole /64).
type Limits struct {
	opts LimitOptions

	mu        sync.Mutex
	addresses map[string]*list.Element
	lru       *list.List
}

// address is what's known about an address
type address struct {
	key string
	// failures counts failed logins since firstFailure, and lockedUntil
	// is when its lockout ends
	failures     int
	firstFailure time.Time
	lockedUntil  time.Time
	// tokens is how many more requests it may send to the backend (as of
	// filled), refilled at the Rate, up to the Burst
	tokens float64
	filled time.Time
	// probes counts its probes since firstProbe, and bannedUntil is when
	// its ban from the backend ends
	probes      int
	firstProbe  time.Time
	bannedUntil time.Time
}

// probeWindow is how long probes are counted over
const probeWindow = 10 * time.Minute

// NewLimits returns Limits configured by opts
func NewLimits(opts LimitOptions) *Limits {
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.Rate > 0 && opts.Burst < 1 {
		opts.Burst = int(math.Ceil(opts.Rate * 10))
	}
	return &Limits{opts: opts, addresses: map[string]*list.Element{}, lru: list.New()}
}

// addressKey returns the key r's address is kept under, or "" if it isn't
// limited
func (l *Limits) addressKey(r *http.Request) (string, net.IP) {
	ip := clientip.From(r)
	if ip == nil {
		return "", nil
	}
	for _, n := range l.opts.Allow {
		if n.Contains(ip) {
			return "", ip
		}
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String(), ip4
	}
	network := &net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}
	return network.String(), ip
}

// get returns key's address, keeping track of it if create, with l.mu held
func (l *Limits) get(key string, create bool) *address {
	if e, ok := l.addresses[key]; ok {
		l.lru.MoveToFront(e)
		return e.Value.(*address)
	}
	if !create {
		return nil
	}
	a := &address{key: key, tokens: float64(l.opts.Burst), filled: l.opts.now()}
	l.addresses[key] = l.lru.PushFront(a)
	if l.lru.Len() > maxAddresses {
		oldest := l.lru.Back()
		l.lru.Remove(oldest)
		delete(l.addresses, oldest.Value.(*address).key)
	}
	return a
}

// LockedOut returns whether r's address is locked out of logging in, and
// for how much longer
func (l *Limits) LockedOut(r *http.Request) (time.Duration, bool) {
	if l == nil || l.opts.LoginAttempts <= 0 {
		return 0, false
	}
	key, _ := l.addressKey(r)
	if key == "" {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.get(key, false)
	if a == nil {
		return 0, false
	}
	if left := a.lockedUntil.Sub(l.opts.now()); left > 0 {
		return left, true
	}
	return 0, false
}

// LoginFailed records a failed login from r's address, locking it out if
// it has failed too often
func (l *Limits) LoginFailed(r *http.Request) {
	if l == nil || l.opts.LoginAttempts <= 0 {
		return
	}
	key, ip := l.addressKey(r)
	if key == "" {
		return
	}
	l.mu.Lock()
	now := l.opts.now()
	a := l.get(key, true)
	if now.Sub(a.firstFailure) > l.opts.LoginLockout {
		a.failures, a.firstFailure = 0, now
	}
	a.failures++
	lockedOut := a.failures >= l.opts.LoginAttempts && !a.lockedUntil.After(now)
	failures := a.failures
	if lockedOut {
		a.lockedUntil = now.Add(l.opts.LoginLockout)
		a.failures, a.firstFailure = 0, time.Time{}
	}
	l.mu.Unlock()
	if lockedOut {
		l.opts.Events.Add(events.Refused, "", 0, events.Text(fmt.Sprintf("locked out %s from logging in for %s, after %d failed logins (most recently %s %s)",
			describe(key, ip), formatDuration(l.opts.LoginLockout), failures, r.Method, r.URL.Path)))
	}
}

// LoginSucceeded forgets r's address's failed logins
func (l *Limits) LoginSucceeded(r *http.Request) {
	if l == nil {
		return
	}
	key, _ := l.addressKey(r)
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if a := l.get(key, false); a != nil {
		a.failures, a.firstFailure = 0, time.Time{}
	}
}

// Probed records a probe from r's address, banning it from the backend if
// it has probed too often
func (l *Limits) Probed(r *http.Request) {
	if l == nil || l.opts.ProbeLimit <= 0 {
		return
	}
	key, ip := l.addressKey(r)
	if key == "" {
		return
	}
	l.mu.Lock()
	now := l.opts.now()
	a := l.get(key, true)
	if now.Sub(a.firstProbe) > probeWindow {
		a.probes, a.firstProbe = 0, now
	}
	a.probes++
	banned := a.probes >= l.opts.ProbeLimit && !a.bannedUntil.After(now)
	probes := a.probes
	if banned {
		a.bannedUntil = now.Add(l.opts.ProbeBan)
		a.probes, a.firstProbe = 0, time.Time{}
	}
	l.mu.Unlock()
	if banned {
		l.opts.Events.Add(events.Refused, "", 0, events.Text(fmt.Sprintf("banned %s from WordPress for %s, after %d probes for sensitive files or exploits (most recently %s %s); it's still served cached pages",
			describe(key, ip), formatDuration(l.opts.ProbeBan), probes, r.Method, r.URL.Path)))
	}
}

// AllowBackend returns "" if r's address may send another request to the
// backend, or else why not (the setting which refuses it), and how long
// until it may (see fetch.Options.BackendGate)
func (l *Limits) AllowBackend(r *http.Request) (time.Duration, string) {
	if l == nil || (l.opts.Rate <= 0 && l.opts.ProbeLimit <= 0) {
		return 0, ""
	}
	key, ip := l.addressKey(r)
	if key == "" {
		return 0, ""
	}
	l.mu.Lock()
	now := l.opts.now()
	if l.opts.Rate <= 0 {
		// Only bans: don't keep track of every address
		a := l.get(key, false)
		var left time.Duration
		if a != nil {
			left = a.bannedUntil.Sub(now)
		}
		l.mu.Unlock()
		if left > 0 {
			return left, "security.probeLimit"
		}
		return 0, ""
	}
	a := l.get(key, true)
	if left := a.bannedUntil.Sub(now); left > 0 {
		l.mu.Unlock()
		return left, "security.probeLimit"
	}
	a.tokens = math.Min(float64(l.opts.Burst), a.tokens+now.Sub(a.filled).Seconds()*l.opts.Rate)
	a.filled = now
	if a.tokens >= 1 {
		a.tokens--
		l.mu.Unlock()
		return 0, ""
	}
	wait := time.Duration((1 - a.tokens) / l.opts.Rate * float64(time.Second))
	l.mu.Unlock()
	l.opts.Events.Add(events.Refused, "limited "+key, 10*time.Minute, func(e *events.Event) {
		e.Text = fmt.Sprintf("rate-limited %s: refused %d %s to WordPress (it may send %s a second)",
			describe(key, ip), e.Count, plural(e.Count), formatRate(l.opts.Rate))
	})
	return wait, "security.rateLimit"
}

// Lockout is an address which is locked out of logging in, or banned
// from the backend
type Lockout struct {
	// Address is the address, or IPv6 /64 network
	Address string
	Until   time.Time
	// From is what it's blocked from: LockedOutOfLogins or BannedFromBackend
	From string
}

// What addresses are blocked from (see Lockout)
const (
	LockedOutOfLogins = "logging in"
	BannedFromBackend = "WordPress (for probing)"
)

// Lockouts returns the addresses which are locked out or banned now, the
// longest first
func (l *Limits) Lockouts() []Lockout {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.opts.now()
	var lockouts []Lockout
	for key, e := range l.addresses {
		a := e.Value.(*address)
		if a.lockedUntil.After(now) {
			lockouts = append(lockouts, Lockout{Address: key, Until: a.lockedUntil, From: LockedOutOfLogins})
		}
		if a.bannedUntil.After(now) {
			lockouts = append(lockouts, Lockout{Address: key, Until: a.bannedUntil, From: BannedFromBackend})
		}
	}
	sort.Slice(lockouts, func(i, j int) bool {
		if !lockouts[i].Until.Equal(lockouts[j].Until) {
			return lockouts[i].Until.After(lockouts[j].Until)
		}
		if lockouts[i].Address != lockouts[j].Address {
			return lockouts[i].Address < lockouts[j].Address
		}
		return lockouts[i].From < lockouts[j].From
	})
	return lockouts
}

// Unblock ends the lockout and ban of the address (or IPv6 network) key (and rate limiting), and returns whether
// it was locked out
func (l *Limits) Unblock(key string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.addresses[key]
	if !ok {
		return false
	}
	a := e.Value.(*address)
	now := l.opts.now()
	wasLocked := a.lockedUntil.After(now) || a.bannedUntil.After(now)
	l.lru.Remove(e)
	delete(l.addresses, key)
	return wasLocked
}

// describe describes an address for the events log
func describe(key string, ip net.IP) string {
	if ip != nil && ip.To4() == nil {
		return fmt.Sprintf("%s (%s)", key, ip)
	}
	return key
}

func formatDuration(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return d.String()
	}
}

func formatRate(rate float64) string {
	if rate == math.Trunc(rate) {
		return fmt.Sprintf("%d", int(rate))
	}
	return fmt.Sprintf("%g", rate)
}
