package admin

import (
	"net/http"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/cluster"
	"github.com/davidjwilkins/honey/events"
	"github.com/stretchr/testify/assert"
)

func TestDashboardShowsTheCluster(t *testing.T) {
	status := cluster.Status{ID: "web1@abc", Healthy: true, Peers: []cluster.PeerStatus{
		{Address: "http://10.0.0.2:8081", Name: "web2", Seen: time.Now().Add(-2 * time.Second)},
		{Address: "http://10.0.0.3:8081", Error: "connection refused"},
	}}
	h := Handler(Options{Events: events.New(0), Cluster: func() cluster.Status { return status }})
	page := do(h, http.MethodGet, "/", "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, page, "this is <span class=\"mono\">web1</span> · in touch")
	assert.Contains(t, page, "<th scope=\"row\">web2</th>")
	assert.Contains(t, page, "<td>2.0s ago</td>")
	assert.Contains(t, page, "<td class=\"bad\">connection refused</td>")
	assert.Contains(t, page, "<td>never</td>")

	status.Healthy = false
	page = do(h, http.MethodGet, "/", "127.0.0.1:9090", nil).Body.String()
	assert.Contains(t, page, "out of touch")

	h = Handler(Options{Events: events.New(0)})
	page = do(h, http.MethodGet, "/", "127.0.0.1:9090", nil).Body.String()
	assert.NotContains(t, page, "Cluster</h2>", "not clustered")
}
