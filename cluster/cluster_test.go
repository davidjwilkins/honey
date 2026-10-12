package cluster

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// testNode is a node with an HTTP server, which records what it applies
type testNode struct {
	*Node
	srv  *httptest.Server
	down atomic.Bool
	// hang, if set, holds up purges sent to it until it's closed
	hang chan struct{}

	mu      sync.Mutex
	applied []Purge
	expired int
}

func (t *testNode) Applied() []Purge {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Purge(nil), t.applied...)
}

func (t *testNode) Expired() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.expired
}

const testSecret = "a secret for the tests"

// newCluster starts a node for each name, each with every node (itself
// included) as a peer
func newCluster(t *testing.T, c *clock, names ...string) []*testNode {
	t.Helper()
	var nodes []*testNode
	var peers []string
	for range names {
		tn := &testNode{}
		tn.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tn.down.Load() {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			if tn.hang != nil && r.URL.Path == "/cluster/purge" {
				<-tn.hang
			}
			tn.Handler().ServeHTTP(w, r)
		}))
		t.Cleanup(tn.srv.Close)
		nodes = append(nodes, tn)
		peers = append(peers, tn.srv.URL)
	}
	for i, tn := range nodes {
		tn.Node = newNode(t, c, names[i], peers, tn)
	}
	return nodes
}

func newNode(t *testing.T, c *clock, name string, peers []string, tn *testNode) *Node {
	t.Helper()
	n, err := New(Options{Name: name, Peers: peers, Secret: testSecret, Lease: 5 * time.Second,
		Apply: func(p Purge) {
			tn.mu.Lock()
			tn.applied = append(tn.applied, p)
			tn.mu.Unlock()
		},
		ExpireAll: func() {
			tn.mu.Lock()
			tn.expired++
			tn.mu.Unlock()
		},
		now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func tickAll(nodes ...*testNode) {
	for _, n := range nodes {
		n.tick(context.Background())
	}
}

// eventually waits for cond, failing the test if it doesn't happen
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPurgesReachTheOtherNodes(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b", "c")
	tickAll(nodes...)

	result := nodes[0].Purged(Purge{Path: "/blog/", Prefix: true})
	if strings.Join(result.Applied, ",") != "b,c" || len(result.Missed) != 0 {
		t.Fatalf("got %+v, want b and c to have applied it", result)
	}
	for _, n := range nodes[1:] {
		applied := n.Applied()
		if len(applied) != 1 || applied[0].Path != "/blog/" || !applied[0].Prefix || applied[0].Origin != nodes[0].ID() || applied[0].Seq != 1 {
			t.Errorf("%s applied %+v", n.ID(), applied)
		}
	}
	if len(nodes[0].Applied()) != 0 {
		t.Error("the purge was applied to the node it was made on again")
	}

	// Heartbeats don't apply it again
	tickAll(nodes...)
	for _, n := range nodes[1:] {
		if len(n.Applied()) != 1 {
			t.Errorf("%s applied %d purges, want 1", n.ID(), len(n.Applied()))
		}
	}
	for _, n := range nodes {
		if !n.Healthy() {
			t.Errorf("%s isn't healthy", n.ID())
		}
	}
}

func TestNodesDontPeerWithThemselves(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b")
	tickAll(nodes...)
	s := nodes[0].Status()
	if len(s.Peers) != 1 || s.Peers[0].Name != "b" || s.Peers[0].Address != nodes[1].srv.URL {
		t.Errorf("got peers %+v, want just b", s.Peers)
	}
}

func TestANodeWhichMissesAPurgeCatchesUp(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b", "c")
	a, b := nodes[0], nodes[1]
	tickAll(nodes...)

	b.down.Store(true)
	result := a.Purged(Purge{Path: "/a-post/"})
	if strings.Join(result.Applied, ",") != "c" || strings.Join(result.Missed, ",") != "b" {
		t.Fatalf("got %+v, want c to have applied it and b to have missed it", result)
	}
	b.down.Store(false)

	// b catches up from whichever node it hears from
	b.tick(context.Background())
	applied := b.Applied()
	if len(applied) != 1 || applied[0].Path != "/a-post/" {
		t.Fatalf("b applied %+v, want the purge it missed", applied)
	}
	if b.Expired() != 0 {
		t.Error("b expired its whole cache, rather than applying the purge it missed")
	}
}

func TestANodeWhichMissesALotExpiresEverything(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b")
	a, b := nodes[0], nodes[1]
	tickAll(nodes...)

	b.down.Store(true)
	for i := 0; i <= maxCatchUp; i++ {
		a.Purged(Purge{Path: "/a-post/"})
	}
	b.down.Store(false)
	b.tick(context.Background())
	if len(b.Applied()) != 0 || b.Expired() != 1 {
		t.Fatalf("b applied %d purges and expired its cache %d times, want 0 and 1", len(b.Applied()), b.Expired())
	}
	// And it's caught up
	a.Purged(Purge{Path: "/another-post/"})
	b.tick(context.Background())
	if applied := b.Applied(); len(applied) != 1 || applied[0].Path != "/another-post/" || b.Expired() != 1 {
		t.Errorf("b applied %+v, and expired its cache %d times", applied, b.Expired())
	}
}

func TestANodeWhosePurgesAreNoLongerKeptExpiresEverything(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b")
	a, b := nodes[0], nodes[1]
	tickAll(nodes...)

	b.down.Store(true)
	a.Purged(Purge{Path: "/a-post/"})
	a.Purged(Purge{Path: "/another-post/"})
	// The first is no longer kept
	c.Add(logTime - time.Minute)
	a.Purged(Purge{Path: "/a-third-post/"})
	c.Add(2 * time.Minute)
	a.Purged(Purge{Path: "/a-fourth-post/"})
	b.down.Store(false)

	// (b has been out of touch for a while, so it's fenced too)
	expired := b.Expired()
	b.tick(context.Background())
	if len(b.Applied()) != 0 || b.Expired() == expired {
		t.Fatalf("b applied %+v, and didn't expire its cache", b.Applied())
	}
	if !b.Healthy() {
		t.Error("b isn't healthy, having caught up")
	}
}

func TestANodeOutOfTouchStopsTrustingItsCache(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b")
	a, b := nodes[0], nodes[1]
	tickAll(nodes...)

	b.down.Store(true)
	c.Add(4 * time.Second)
	a.tick(context.Background())
	if !a.Healthy() || a.Expired() != 0 {
		t.Fatal("a stopped trusting its cache within the lease")
	}
	c.Add(time.Second)
	a.tick(context.Background())
	if a.Healthy() || a.Expired() != 1 {
		t.Fatalf("a is healthy (%v) and expired its cache %d times, out of touch for the lease", a.Healthy(), a.Expired())
	}
	c.Add(time.Second)
	a.tick(context.Background())
	if a.Expired() != 1 {
		t.Error("a expired its cache again within the lease")
	}
	c.Add(4 * time.Second)
	a.tick(context.Background())
	if a.Expired() != 2 {
		t.Error("a didn't expire its cache again after the lease")
	}

	b.down.Store(false)
	a.tick(context.Background())
	if !a.Healthy() {
		t.Error("a isn't healthy, back in touch")
	}
}

func TestANodeAloneTrustsItsCache(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	tn := &testNode{}
	tn.Node = newNode(t, c, "a", nil, tn)
	c.Add(time.Hour)
	tn.tick(context.Background())
	if !tn.Healthy() || tn.Expired() != 0 {
		t.Error("a node with no peers stopped trusting its cache")
	}
}

func TestANodeWhichIsItsOnlyPeerTrustsItsCache(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	a := newCluster(t, c, "a")[0]
	for i := 0; i < 3; i++ {
		a.tick(context.Background())
		c.Add(5 * time.Second)
	}
	if !a.Healthy() || a.Expired() != 0 {
		t.Error("a node alone in its cluster stopped trusting its cache")
	}
}

func TestMissedPurgesAreFetchedFromTheirOrigin(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b", "c")
	a, b, cn := nodes[0], nodes[1], nodes[2]
	tickAll(nodes...)
	a.Purged(Purge{Path: "/a-post/"})
	b.down.Store(true)
	a.Purged(Purge{Path: "/another-post/"})

	// c restarts, skipping a's purges so far, then has the next one
	cn.Node = newNode(t, c, "c", []string{a.srv.URL, b.srv.URL, cn.srv.URL}, cn)
	cn.tick(context.Background())
	a.tick(context.Background())
	a.Purged(Purge{Path: "/a-third-post/"})

	// b has missed two, which c says it has (but doesn't keep): it gets
	// them from a, rather than expiring its cache
	b.down.Store(false)
	for i := 0; i < 5; i++ {
		expired := b.Expired()
		b.tick(context.Background())
		if applied := b.Applied(); len(applied) != 3 || applied[2].Path != "/a-third-post/" || b.Expired() != expired {
			t.Fatalf("b applied %+v, and expired its cache %d times", applied, b.Expired()-expired)
		}
	}
}

func TestANodeStartingUpSkipsEarlierPurges(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b")
	a, b := nodes[0], nodes[1]
	tickAll(nodes...)
	for i := 0; i < 3; i++ {
		a.Purged(Purge{Path: "/a-post/"})
	}

	// b restarts
	b.Node = newNode(t, c, "b", []string{a.srv.URL, b.srv.URL}, b)
	b.applied = nil
	expired := b.Expired()
	b.tick(context.Background())
	if len(b.Applied()) != 0 {
		t.Errorf("b applied %+v, purges from before it started", b.Applied())
	}
	if b.Expired() != expired+1 {
		t.Errorf("b expired its cache %d times, joining the cluster, want once", b.Expired()-expired)
	}

	// Later purges from a are caught up with
	b.down.Store(true)
	a.Purged(Purge{Path: "/another-post/"})
	b.down.Store(false)
	b.tick(context.Background())
	if applied := b.Applied(); len(applied) != 1 || applied[0].Path != "/another-post/" {
		t.Errorf("b applied %+v", applied)
	}
}

func TestARestartedNodeNumbersItsPurgesAfresh(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b")
	a, b := nodes[0], nodes[1]
	tickAll(nodes...)
	a.Purged(Purge{Path: "/a-post/"})

	c.Add(time.Second)
	a.Node = newNode(t, c, "a", []string{a.srv.URL, b.srv.URL}, a)
	tickAll(a, b)
	if result := a.Purged(Purge{Path: "/another-post/"}); len(result.Applied) != 1 {
		t.Fatalf("got %+v", result)
	}
	if applied := b.Applied(); len(applied) != 2 || applied[1].Path != "/another-post/" || applied[1].Seq != 1 {
		t.Errorf("b applied %+v, want both purges", applied)
	}
}

func TestPeersHostNamesAreLookedUp(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b", "c")
	port := func(n *testNode) string { return n.srv.URL[strings.LastIndex(n.srv.URL, ":"):] }
	if port(nodes[1]) == port(nodes[2]) {
		t.Skip("the servers' ports are the same")
	}
	// A name for b and c (which only works for each with its own port, as
	// they're on the same host)
	a := nodes[0]
	a.Node = newNode(t, c, "a", []string{"http://honey" + port(nodes[1]), "http://honey" + port(nodes[2])}, a)
	a.opts.resolve = func(ctx context.Context, host string) ([]string, error) {
		return []string{"127.0.0.1", "127.0.0.2"}, nil
	}
	a.tick(context.Background())
	s := a.Status()
	if len(s.Peers) != 4 {
		t.Fatalf("got peers %+v, want each address at each port", s.Peers)
	}
	reached := 0
	for _, p := range s.Peers {
		if p.Error == "" {
			reached++
		}
	}
	if reached != 2 {
		t.Errorf("reached %d peers, want b and c: %+v", reached, s.Peers)
	}
}

func TestRequestsMustBeSigned(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a")
	body := `{"origin":"x@1","seq":1,"path":"/"}`

	for name, header := range map[string]string{
		"unsigned":         "",
		"another secret":   sign("another secret entirely", http.MethodPost, "/cluster/purge", []byte(body), c.Now()),
		"another URI":      sign(testSecret, http.MethodPost, "/cluster/heartbeat", []byte(body), c.Now()),
		"another body":     sign(testSecret, http.MethodPost, "/cluster/purge", []byte(`{}`), c.Now()),
		"signed long ago":  sign(testSecret, http.MethodPost, "/cluster/purge", []byte(body), c.Now().Add(-2*time.Minute)),
		"signed in future": sign(testSecret, http.MethodPost, "/cluster/purge", []byte(body), c.Now().Add(2*time.Minute)),
		"garbled":          "1700000000 zz",
	} {
		req, _ := http.NewRequest(http.MethodPost, nodes[0].srv.URL+"/cluster/purge", strings.NewReader(body))
		req.Header.Set(SignatureHeader, header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: got %s, want 403", name, resp.Status)
		}
	}
	if len(nodes[0].Applied()) != 0 {
		t.Errorf("applied %+v", nodes[0].Applied())
	}

	req, _ := http.NewRequest(http.MethodPost, nodes[0].srv.URL+"/cluster/purge", strings.NewReader(body))
	req.Header.Set(SignatureHeader, sign(testSecret, http.MethodPost, "/cluster/purge", []byte(body), c.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(nodes[0].Applied()) != 1 {
		t.Errorf("a signed purge got %s, and applied %+v", resp.Status, nodes[0].Applied())
	}
}

func TestResponsesMustBeSigned(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	impostor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(SignatureHeader, sign(testSecret, "RESPONSE", "a different request", []byte(`{}`), c.Now()))
		w.Write([]byte(`{}`))
	}))
	defer impostor.Close()
	tn := &testNode{}
	tn.Node = newNode(t, c, "a", []string{impostor.URL}, tn)
	if result := tn.Purged(Purge{Path: "/"}); len(result.Missed) != 0 {
		// It isn't a peer until it has answered a heartbeat
		t.Fatalf("got %+v", result)
	}
	tn.tick(context.Background())
	if result := tn.Purged(Purge{Path: "/"}); len(result.Missed) != 1 {
		t.Errorf("got %+v, want the purge not confirmed", result)
	}
	if s := tn.Status(); len(s.Peers) != 1 || !strings.Contains(s.Peers[0].Error, "isn't signed") {
		t.Errorf("got %+v", s.Peers)
	}
}

func TestNewChecksOptions(t *testing.T) {
	for name, opts := range map[string]Options{
		"short secret": {Secret: "short", Peers: []string{"http://10.0.0.1:8081"}},
		"not a URL":    {Secret: testSecret, Peers: []string{"10.0.0.1:8081"}},
		"no host":      {Secret: testSecret, Peers: []string{"http:///"}},
	} {
		if _, err := New(opts); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestPurgesCanBeSentWithoutWaiting(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b")
	tickAll(nodes...)
	nodes[0].opts.PurgeWait = -1
	if result := nodes[0].Purged(Purge{Path: "/"}); len(result.Applied)+len(result.Missed) != 0 {
		t.Errorf("got %+v", result)
	}
	eventually(t, "b to apply the purge", func() bool { return len(nodes[1].Applied()) == 1 })
}

func TestPurgesDontWaitForNodesWhichAreDown(t *testing.T) {
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	nodes := newCluster(t, c, "a", "b", "c")
	a, b := nodes[0], nodes[1]
	tickAll(nodes...)
	a.opts.PurgeWait = time.Minute

	b.down.Store(true)
	a.tick(context.Background())
	b.hang = make(chan struct{})
	b.down.Store(false)
	defer close(b.hang)
	start := time.Now()
	result := a.Purged(Purge{Path: "/a-post/"})
	if strings.Join(result.Applied, ",") != "c" || strings.Join(result.Missed, ",") != "b" {
		t.Errorf("got %+v, want c to have applied it and b not waited for", result)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("the purge waited for b")
	}
}
