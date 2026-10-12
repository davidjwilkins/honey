// Package cluster keeps several Honey nodes' caches in step: a purge on one
// node (from WordPress, the admin page or a PURGE request) is passed on to
// the others, which apply it too.
//
// There's no leader and no consensus: a cache only holds copies of what
// WordPress says, so what matters is that every node drops what was purged,
// soon - not that they agree on an order.  So:
//
//   - Each purge is numbered by the node it started on (its origin: the
//     node's name and when it started, so that a restart begins a new
//     sequence), and sent to every other node, which applies it and says
//     so.  The purge waits for that, for up to Options.PurgeWait, so that
//     once WordPress's purge request returns, the nodes which could be
//     reached have all dropped the page.
//   - Every node asks the others, a few times a second, how far they have
//     got with each origin's purges (a version vector, in heartbeats).  A
//     node which is behind - a purge never reached it, say - asks for the
//     ones it missed from a node which has them (each keeps a log of recent
//     purges), and applies them.  If they're no longer kept (or there are
//     a lot of them), it expires its whole cache instead.
//   - A node which hasn't caught up with another for Options.Lease can't
//     know what it has missed, so it stops trusting its cache: it expires
//     it all, again every Lease while it's out of touch, and says it isn't
//     healthy (see Healthy), so that a load balancer can stop sending it
//     visitors.  A node starting up expires what it has cached too (e.g.
//     loaded from a snapshot), as it can't know what was purged while it
//     was down.
//
// So a node serves a page which was purged for at most about Lease after
// the purge, however the network fails - or, as the cache is expired rather
// than emptied (so that visitors needn't all wait for WordPress at once),
// once more after that, while it's refreshed, if it may be served stale.
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/davidjwilkins/honey/events"
)

const (
	// DefaultLease is how long a node trusts its cache without hearing
	// from another, by default
	DefaultLease = 5 * time.Second
	// DefaultPurgeWait is how long a purge waits for the other nodes to
	// apply it, by default
	DefaultPurgeWait = 2 * time.Second
	// maxLog bounds the purges each node keeps, for others to catch up
	// with, and logTime how long it keeps them
	maxLog  = 10000
	logTime = time.Hour
	// maxBody bounds the requests nodes send each other
	maxBody = 4 << 20
	// maxCatchUp is how many missed purges a node applies, catching up;
	// more than that, it expires its whole cache instead, which is quicker
	maxCatchUp = 100
	// resolveEvery is how often peers' host names are looked up again
	resolveEvery = 30 * time.Second
)

// Purge is a purge passed between nodes
type Purge struct {
	// Origin is the node it started on, and Seq its number there
	Origin string `json:"origin"`
	Seq    uint64 `json:"seq"`
	// Path is the path (and query string) purged, or with Prefix the path
	// everything under which was purged; Soft is whether it was expired
	// rather than removed.  Or Fragments are the shared fragments purged
	// (see fetch.FragmentPurgeHeader).
	Path      string    `json:"path,omitempty"`
	Prefix    bool      `json:"prefix,omitempty"`
	Soft      bool      `json:"soft,omitempty"`
	Fragments string    `json:"fragments,omitempty"`
	At        time.Time `json:"at"`
}

func (p Purge) String() string {
	if p.Fragments != "" {
		return "the shared fragments " + p.Fragments
	}
	if p.Prefix {
		return p.Path + "*"
	}
	return p.Path
}

// Options configures a Node
type Options struct {
	// Name is this node's (the host name, by default)
	Name string
	// Peers are the other nodes' cluster addresses, as URLs (e.g.
	// "http://10.0.0.2:8081"): a host name which resolves to several
	// addresses is a node at each (e.g. a Kubernetes headless service).
	// This node's own address may be among them.
	Peers []string
	// Secret signs what nodes send each other
	Secret string
	// Lease and PurgeWait (see the package's documentation): 0 for the
	// defaults, and a negative PurgeWait not to wait
	Lease     time.Duration
	PurgeWait time.Duration
	// Apply applies a purge from another node to this one's cache
	Apply func(Purge)
	// ExpireAll expires everything in this node's cache (softly: it may
	// be served while it's refreshed)
	ExpireAll func()
	// Events, if set, is where what happens is logged, for the admin page
	Events *events.Log

	now     func() time.Time
	client  *http.Client
	resolve func(ctx context.Context, host string) ([]string, error)
}

// Node is this Honey, as one of a cluster.  Its Purged, Healthy and Status
// methods may be called on a nil Node (which isn't in a cluster).
type Node struct {
	opts Options
	id   string

	mu sync.Mutex
	// seq numbers this node's own purges
	seq uint64
	// applied is how far this node has got with each origin's purges
	applied map[string]*progress
	// log is the recent purges, from every origin, oldest first
	log []Purge
	// peers are the other nodes, by address
	peers   map[string]*peer
	self    map[string]bool
	address time.Time // when peers' addresses were last looked up
	// synced is whether this node has caught up with another since it
	// started (until then, it adopts how far the others have got, having
	// expired its cache as it started); adopted is whether it has, so its
	// cache must be expired again once it's synced.  contact is when it
	// last caught up with another.
	synced  bool
	adopted bool
	contact time.Time
	started time.Time
	// fenced is whether it's out of touch, and expired when it last
	// expired its cache for it
	fenced  bool
	expired time.Time
	// catching up, by origin
	catching map[string]bool
}

// progress is how far a node has got with an origin's purges: every one up
// to contiguous, and those above it in above
type progress struct {
	contiguous uint64
	above      map[uint64]bool
}

func (p *progress) has(seq uint64) bool {
	return seq <= p.contiguous || p.above[seq]
}

func (p *progress) add(seq uint64) {
	if p.has(seq) {
		return
	}
	if p.above == nil {
		p.above = map[uint64]bool{}
	}
	p.above[seq] = true
	for p.above[p.contiguous+1] {
		delete(p.above, p.contiguous+1)
		p.contiguous++
	}
}

// peer is another node
type peer struct {
	address  string
	id, name string
	seen     time.Time
	err      string
}

// New returns a node of a cluster
func New(opts Options) (*Node, error) {
	if len(opts.Secret) < 16 {
		return nil, errors.New("the cluster's secret must be at least 16 characters")
	}
	if opts.Name == "" {
		opts.Name, _ = os.Hostname()
	}
	if opts.Lease <= 0 {
		opts.Lease = DefaultLease
	}
	if opts.PurgeWait == 0 {
		opts.PurgeWait = DefaultPurgeWait
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.client == nil {
		opts.client = &http.Client{Timeout: opts.Lease}
	}
	if opts.resolve == nil {
		opts.resolve = net.DefaultResolver.LookupHost
	}
	for _, p := range opts.Peers {
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("the cluster peer %q isn't an http(s) URL", p)
		}
	}
	now := opts.now()
	n := &Node{opts: opts, id: opts.Name + "@" + strconv.FormatInt(now.UnixNano(), 36),
		applied: map[string]*progress{}, peers: map[string]*peer{}, self: map[string]bool{},
		started: now, catching: map[string]bool{}}
	return n, nil
}

// ID is this node's name, and when it started
func (n *Node) ID() string { return n.id }

// Healthy returns whether this node's cache can be trusted: whether it has
// caught up with another node within the lease (or it started within the
// lease, having expired its cache)
func (n *Node) Healthy() bool {
	if n == nil {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.fenced
}

// Run expires this node's cache (what was cached before it joined the
// cluster, e.g. loaded from a snapshot, may have been purged on the other
// nodes since), then sends heartbeats, catches up and watches the lease
// until ctx is done
func (n *Node) Run(ctx context.Context) {
	n.opts.ExpireAll()
	ticker := time.NewTicker(n.opts.Lease / 5)
	defer ticker.Stop()
	for {
		n.tick(ctx)
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

// tick sends a round of heartbeats, and fences the node if it's out of
// touch
func (n *Node) tick(ctx context.Context) {
	n.lookUpPeers(ctx)
	n.mu.Lock()
	var peers []*peer
	for _, p := range n.peers {
		peers = append(peers, p)
	}
	n.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p *peer) {
			defer wg.Done()
			n.heartbeat(ctx, p)
		}(p)
	}
	wg.Wait()
	n.mu.Lock()
	alone := len(n.peers) == 0 && len(n.self) > 0
	n.mu.Unlock()
	if alone {
		// Every peer's address is this node's: there's nothing to miss
		n.inTouch(n.opts.now())
	}
	n.mu.Lock()
	expire := false
	if !n.synced && !n.contact.IsZero() {
		// Purges up to how far the others had got were skipped, adopting
		// that: so what was cached since this node started is expired too
		n.synced = true
		expire, n.adopted = n.adopted, false
	}
	n.mu.Unlock()
	if expire {
		n.opts.ExpireAll()
	}
	n.checkLease()
}

// lookUpPeers finds the other nodes' addresses, every resolveEvery
func (n *Node) lookUpPeers(ctx context.Context) {
	now := n.opts.now()
	n.mu.Lock()
	if !n.address.IsZero() && now.Sub(n.address) < resolveEvery {
		n.mu.Unlock()
		return
	}
	n.address = now
	n.mu.Unlock()
	addresses := map[string]bool{}
	for _, raw := range n.opts.Peers {
		u, _ := url.Parse(raw)
		host, port := u.Hostname(), u.Port()
		if net.ParseIP(host) != nil {
			addresses[strings.TrimSuffix(raw, "/")] = true
			continue
		}
		ips, err := n.opts.resolve(ctx, host)
		if err != nil || len(ips) == 0 {
			// It's tried as it is, which reports the problem
			addresses[strings.TrimSuffix(raw, "/")] = true
			continue
		}
		for _, ip := range ips {
			a := *u
			a.Host = ip
			if port != "" {
				a.Host = net.JoinHostPort(ip, port)
			} else if strings.Contains(ip, ":") {
				a.Host = "[" + ip + "]"
			}
			addresses[strings.TrimSuffix(a.String(), "/")] = true
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for a := range addresses {
		if n.peers[a] == nil && !n.self[a] {
			n.peers[a] = &peer{address: a}
		}
	}
	for a := range n.peers {
		if !addresses[a] {
			delete(n.peers, a)
		}
	}
}

// heartbeat tells a peer how far this node has got, hears how far it has,
// and catches up with it
func (n *Node) heartbeat(ctx context.Context, p *peer) {
	var theirs heartbeatMessage
	sent := n.opts.now()
	err := n.call(ctx, http.MethodPost, p.address+"/cluster/heartbeat", n.heartbeatMessage(), &theirs)
	n.mu.Lock()
	if err != nil {
		changed := p.err == ""
		p.err = err.Error()
		n.mu.Unlock()
		if changed && p.id != "" {
			n.opts.Events.Add(events.Error, "cluster "+p.address, 10*time.Minute, events.Text(fmt.Sprintf("can't reach the cluster node %s (%s): %v", p.name, p.address, err)))
		}
		return
	}
	if theirs.ID == n.id {
		// It's this node
		delete(n.peers, p.address)
		n.self[p.address] = true
		n.mu.Unlock()
		return
	}
	if p.err != "" && p.id != "" {
		n.opts.Events.Add(events.Info, "", 0, events.Text(fmt.Sprintf("in touch with the cluster node %s (%s) again", theirs.Name, p.address)))
	}
	p.id, p.name, p.seen, p.err = theirs.ID, theirs.Name, n.opts.now(), ""
	n.mu.Unlock()
	if n.heard(ctx, p.address, theirs) {
		n.inTouch(sent)
	}
}

// inTouch records that this node had caught up with the others at at
func (n *Node) inTouch(at time.Time) {
	n.mu.Lock()
	if at.After(n.contact) {
		n.contact = at
	}
	fenced := n.fenced
	n.fenced = false
	n.mu.Unlock()
	if fenced {
		n.opts.Events.Add(events.Info, "", 0, events.Text("back in touch with the rest of the cluster: trusting the cache again"))
	}
}

type heartbeatMessage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Applied is how far the node has got with each origin's purges
	// (every one up to the number)
	Applied map[string]uint64 `json:"applied"`
}

func (n *Node) heartbeatMessage() heartbeatMessage {
	n.mu.Lock()
	defer n.mu.Unlock()
	m := heartbeatMessage{ID: n.id, Name: n.opts.Name, Applied: map[string]uint64{}}
	for origin, p := range n.applied {
		m.Applied[origin] = p.contiguous
	}
	return m
}

// heard hears from another node (at address, if it's known) how far it has
// got, catches up with it, and returns whether this node has now got as far
func (n *Node) heard(ctx context.Context, address string, theirs heartbeatMessage) bool {
	n.mu.Lock()
	var behind []string
	for origin, upTo := range theirs.Applied {
		if origin == n.id {
			continue
		}
		mine := n.progress(origin)
		if upTo <= mine.contiguous {
			continue
		}
		if !n.synced && mine.contiguous == 0 && len(mine.above) == 0 {
			// Starting up: the cache was expired as it started, so earlier
			// purges don't matter (see tick)
			mine.contiguous, n.adopted = upTo, true
			continue
		}
		if address != "" && !n.catching[origin] {
			n.catching[origin] = true
			behind = append(behind, origin)
		}
	}
	n.mu.Unlock()
	for _, origin := range behind {
		// From the origin itself, if it's a peer: it has all its own
		// purges, where another node may have skipped some (see catchUp)
		from := n.addressOf(origin)
		if from == "" {
			from = address
		}
		n.catchUp(ctx, from, origin, theirs.Applied[origin])
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for origin, upTo := range theirs.Applied {
		if origin != n.id && upTo > n.progress(origin).contiguous {
			return false
		}
	}
	return true
}

// progress returns how far this node has got with origin's purges (with
// n.mu held)
func (n *Node) progress(origin string) *progress {
	p := n.applied[origin]
	if p == nil {
		p = &progress{}
		n.applied[origin] = p
	}
	return p
}

// catchUp fetches origin's purges this node has missed from the node at
// address, which has applied them up to upTo, and applies them
func (n *Node) catchUp(ctx context.Context, address, origin string, upTo uint64) {
	defer func() {
		n.mu.Lock()
		delete(n.catching, origin)
		n.mu.Unlock()
	}()
	n.mu.Lock()
	after := n.progress(origin).contiguous
	n.mu.Unlock()
	var missed catchUpMessage
	err := n.call(ctx, http.MethodGet, address+"/cluster/purges?"+url.Values{"origin": {origin}, "after": {strconv.FormatUint(after, 10)}}.Encode(), nil, &missed)
	if err != nil {
		// Next time
		return
	}
	if !missed.Complete || len(missed.Purges) > maxCatchUp {
		// They're gone (or expiring everything is quicker than applying
		// them all): anything could be out of date
		n.opts.ExpireAll()
		n.mu.Lock()
		p := n.progress(origin)
		if upTo > p.contiguous {
			p.contiguous, p.above = upTo, nil
		}
		n.mu.Unlock()
		why := "which are no longer kept"
		if missed.Complete {
			why = fmt.Sprintf("(%d of them)", len(missed.Purges))
		}
		n.opts.Events.Add(events.Purge, "cluster missed", 10*time.Minute, events.Text(fmt.Sprintf(
			"missed purges from the cluster node %s %s: expired the whole cache", nameOf(origin), why)))
		return
	}
	applied := 0
	for _, p := range missed.Purges {
		if p.Origin == origin && n.receive(p) {
			applied++
		}
	}
	if applied > 0 {
		n.opts.Events.Add(events.Purge, "cluster caught up "+origin, time.Minute, func(e *events.Event) {
			total, _ := e.Data().(int)
			total += applied
			e.SetData(total)
			e.Text = fmt.Sprintf("caught up with %d %s from the cluster node %s", total, plural(total, "missed purge", "missed purges"), nameOf(origin))
		})
	}
}

type catchUpMessage struct {
	// Complete is whether Purges are all the purges asked for (or some
	// are no longer kept)
	Complete bool    `json:"complete"`
	Purges   []Purge `json:"purges"`
}

// checkLease fences this node if it hasn't caught up with another for the
// lease, and expires its cache again every lease while it hasn't
func (n *Node) checkLease() {
	now := n.opts.now()
	n.mu.Lock()
	if len(n.opts.Peers) == 0 {
		n.mu.Unlock()
		return
	}
	last := n.contact
	if last.IsZero() {
		last = n.started
	}
	if now.Sub(last) < n.opts.Lease {
		n.mu.Unlock()
		return
	}
	first := !n.fenced
	if !first && now.Sub(n.expired) < n.opts.Lease {
		n.mu.Unlock()
		return
	}
	n.fenced, n.expired = true, now
	n.mu.Unlock()
	n.opts.ExpireAll()
	if first {
		n.opts.Events.Add(events.Error, "", 0, events.Text(fmt.Sprintf(
			"out of touch with the rest of the cluster for %s: not trusting the cache (it's refreshed every %s until it's back in touch)", n.opts.Lease, n.opts.Lease)))
	}
}

// Result is how a purge went: the other nodes which applied it, and those
// which didn't say so (in time)
type Result struct {
	Applied, Missed []string
}

// Purged passes a purge made on this node on to the others, and waits (for
// up to PurgeWait) for them to apply it - except for those which couldn't
// be reached at the last heartbeat, which it doesn't wait for (so that
// while a node is down, each purge doesn't wait for it): they catch up
// when they're back in touch, or stop trusting their caches.
func (n *Node) Purged(p Purge) Result {
	if n == nil {
		return Result{}
	}
	n.mu.Lock()
	n.seq++
	p.Origin, p.Seq, p.At = n.id, n.seq, n.opts.now()
	n.progress(n.id).add(p.Seq)
	n.record(p)
	type target struct {
		address, name string
	}
	var reachable, unreachable []target
	for _, peer := range n.peers {
		t := target{peer.address, peer.name}
		if t.name == "" {
			t.name = t.address
		}
		if peer.err != "" {
			unreachable = append(unreachable, t)
		} else {
			reachable = append(reachable, t)
		}
	}
	n.mu.Unlock()

	send := func(ctx context.Context, t target) error {
		return n.call(ctx, http.MethodPost, t.address+"/cluster/purge", p, nil)
	}
	// Sent in the background, not waited for
	background := func(targets []target) {
		for _, t := range targets {
			go func(t target) {
				ctx, cancel := context.WithTimeout(context.Background(), n.opts.Lease)
				defer cancel()
				send(ctx, t)
			}(t)
		}
	}
	var result Result
	for _, t := range unreachable {
		result.Missed = append(result.Missed, t.name)
	}
	background(unreachable)
	if n.opts.PurgeWait < 0 {
		// Not waiting: the others catch up if it doesn't reach them
		background(reachable)
		return Result{}
	}

	type answer struct {
		name string
		err  error
	}
	answers := make(chan answer, len(reachable))
	ctx, cancel := context.WithTimeout(context.Background(), n.opts.PurgeWait)
	defer cancel()
	for _, t := range reachable {
		go func(t target) {
			answers <- answer{t.name, send(ctx, t)}
		}(t)
	}
	for range reachable {
		a := <-answers
		if a.err != nil {
			result.Missed = append(result.Missed, a.name)
		} else {
			result.Applied = append(result.Applied, a.name)
		}
	}
	sort.Strings(result.Applied)
	sort.Strings(result.Missed)
	if len(result.Missed) > 0 {
		n.opts.Events.Add(events.Purge, "cluster unconfirmed", time.Minute, events.Text(fmt.Sprintf(
			"the purge of %s wasn't confirmed by the cluster %s %s: %s, or it stops trusting its cache",
			p, plural(len(result.Missed), "node", "nodes"), strings.Join(result.Missed, ", "),
			plural(len(result.Missed), "it catches up when it's back in touch", "they catch up when they're back in touch"))))
	}
	return result
}

// receive applies a purge from another node, unless it has been already,
// and returns whether it was applied
func (n *Node) receive(p Purge) bool {
	n.mu.Lock()
	progress := n.progress(p.Origin)
	if progress.has(p.Seq) {
		n.mu.Unlock()
		return false
	}
	// Recorded before it's applied, so that it's only applied once
	progress.add(p.Seq)
	n.record(p)
	n.mu.Unlock()
	n.opts.Apply(p)
	return true
}

// record adds a purge to the log (with n.mu held)
func (n *Node) record(p Purge) {
	n.log = append(n.log, p)
	now := n.opts.now()
	drop := 0
	for drop < len(n.log) && (len(n.log)-drop > maxLog || now.Sub(n.log[drop].At) > logTime) {
		drop++
	}
	// (Appending copies what's left to a new array as it grows, so what's
	// dropped is let go of)
	n.log = n.log[drop:]
}

// missed returns origin's purges after after, which this node has applied,
// and whether they're all of them (the log still has every one)
func (n *Node) missed(origin string, after uint64) catchUpMessage {
	n.mu.Lock()
	defer n.mu.Unlock()
	upTo := n.progress(origin).contiguous
	m := catchUpMessage{Purges: []Purge{}}
	for _, p := range n.log {
		if p.Origin == origin && p.Seq > after && p.Seq <= upTo {
			m.Purges = append(m.Purges, p)
		}
	}
	m.Complete = uint64(len(m.Purges)) == upTo-min(after, upTo)
	return m
}

// Handler serves the other nodes' requests: their heartbeats, purges and
// requests for purges they missed
func (n *Node) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /cluster/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		var theirs heartbeatMessage
		if !n.read(w, r, &theirs) {
			return
		}
		if theirs.ID != n.id {
			n.mu.Lock()
			for _, p := range n.peers {
				if p.id == theirs.ID {
					p.seen = n.opts.now()
				}
			}
			n.mu.Unlock()
			go n.heard(context.Background(), n.addressOf(theirs.ID), theirs)
		}
		n.reply(w, r, n.heartbeatMessage())
	})
	mux.HandleFunc("POST /cluster/purge", func(w http.ResponseWriter, r *http.Request) {
		var p Purge
		if !n.read(w, r, &p) {
			return
		}
		if p.Origin == "" || p.Seq == 0 || (p.Path == "" && p.Fragments == "") {
			http.Error(w, "That isn't a purge", http.StatusBadRequest)
			return
		}
		if n.receive(p) {
			n.opts.Events.Add(events.Purge, "cluster purge "+p.Origin, time.Minute, func(e *events.Event) {
				what := p.String()
				if p.Soft {
					what += " (soft)"
				}
				if e.Count > 1 {
					what = fmt.Sprintf("%d purges, the latest %s,", e.Count, what)
				}
				e.Text = fmt.Sprintf("applied %s from the cluster node %s", what, nameOf(p.Origin))
			})
		}
		n.reply(w, r, struct{}{})
	})
	mux.HandleFunc("GET /cluster/purges", func(w http.ResponseWriter, r *http.Request) {
		if !n.read(w, r, nil) {
			return
		}
		after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
		if err != nil {
			http.Error(w, "after must be a number", http.StatusBadRequest)
			return
		}
		n.reply(w, r, n.missed(r.URL.Query().Get("origin"), after))
	})
	return mux
}

// addressOf returns the address of the peer with id, or ""
func (n *Node) addressOf(id string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.peers {
		if p.id == id {
			return p.address
		}
	}
	return ""
}

// read checks a request from another node is signed, and decodes its body
// into v (if it isn't nil), or responds with why it can't and returns false
func (n *Node) read(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		return false
	}
	if !verify(n.opts.Secret, r.Header.Get(SignatureHeader), r.Method, r.URL.RequestURI(), body, n.opts.now()) {
		http.Error(w, "Not signed with the cluster's secret", http.StatusForbidden)
		return false
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			http.Error(w, "Bad request: "+err.Error(), http.StatusBadRequest)
			return false
		}
	}
	return true
}

// reply responds to r with v, signed (with r's signature, so that it can't
// be passed off as the response to another request)
func (n *Node) reply(w http.ResponseWriter, r *http.Request, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(SignatureHeader, sign(n.opts.Secret, "RESPONSE", r.Header.Get(SignatureHeader), body, n.opts.now()))
	w.Write(body)
}

// call sends another node a request (with body as JSON, if it isn't nil),
// and decodes its signed response into v (if it isn't nil)
func (n *Node) call(ctx context.Context, method, rawURL string, body, v any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	signature := sign(n.opts.Secret, method, req.URL.RequestURI(), payload, n.opts.now())
	req.Header.Set(SignatureHeader, signature)
	resp, err := n.opts.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("it said %s: %s", resp.Status, strings.TrimSpace(string(reply)))
	}
	if !verify(n.opts.Secret, resp.Header.Get(SignatureHeader), "RESPONSE", signature, reply, n.opts.now()) {
		return errors.New("its response isn't signed with the cluster's secret")
	}
	if v != nil {
		return json.Unmarshal(reply, v)
	}
	return nil
}

// Status is what the admin page shows of the cluster
type Status struct {
	ID      string
	Healthy bool
	Peers   []PeerStatus
}

// PeerStatus is another node, as this one sees it
type PeerStatus struct {
	Address, Name string
	// Seen is when this node last heard from it, and Error why it last
	// couldn't
	Seen  time.Time
	Error string
}

// Status returns what this node knows of the cluster
func (n *Node) Status() Status {
	if n == nil {
		return Status{Healthy: true}
	}
	s := Status{ID: n.id, Healthy: n.Healthy()}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, p := range n.peers {
		s.Peers = append(s.Peers, PeerStatus{Address: p.address, Name: p.name, Seen: p.seen, Error: p.err})
	}
	sort.Slice(s.Peers, func(i, j int) bool { return s.Peers[i].Address < s.Peers[j].Address })
	return s
}

// nameOf returns the name in a node's ID
func nameOf(id string) string {
	name, _, _ := strings.Cut(id, "@")
	return name
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
