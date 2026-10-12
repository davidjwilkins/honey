package main

import (
	"bytes"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/davidjwilkins/honey/admin"
	"github.com/davidjwilkins/honey/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func TestPrintPasswordHash(t *testing.T) {
	var out, prompt bytes.Buffer
	require.NoError(t, printPasswordHash(strings.NewReader("correct horse battery staple\n"), &out, &prompt))
	hash := strings.TrimSpace(out.String())
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse battery staple")))
	assert.Contains(t, prompt.String(), "Password")
	assert.Error(t, printPasswordHash(strings.NewReader("short\n"), &out, &prompt))
}

func TestAdminHost(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "the site")
	}))
	defer origin.Close()
	hash, err := admin.HashPassword("correct horse battery staple")
	require.NoError(t, err)
	cfg, err := config.Parse(`
hosts = ["www.example.com"]
[backend]
uri = "` + origin.URL + `"
[admin]
host = "honey.example.com"
passwordHash = "` + hash + `"
`)
	require.NoError(t, err)
	h := build(cfg, time.Now())
	handler := withAdminHost(cfg, h)
	opts := serveOptions(cfg, handler)
	assert.True(t, opts.HostAllowed("honey.example.com"), "its certificate can be got, with [tls]")
	assert.True(t, opts.HostAllowed("www.example.com"))

	send := func(host, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
		if form != nil {
			r = httptest.NewRequest(http.MethodPost, "http://"+host+path, strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Origin", "https://"+host)
		}
		// Over Honey's own HTTPS
		r.TLS = &tls.ConnectionState{}
		r.Header.Set("Accept", "text/html")
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	assert.Equal(t, "the site", send("www.example.com", "/", nil).Body.String())
	w := send("honey.example.com", "/", nil)
	assert.Equal(t, http.StatusSeeOther, w.Code)
	assert.Equal(t, "/login?next=%2F", w.Header().Get("Location"))
	w = send("honey.example.com", "/login", url.Values{"password": {"correct horse battery staple"}})
	require.Equal(t, http.StatusSeeOther, w.Code)
	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.True(t, cookies[0].Secure)
	w = send("honey.example.com", "/", nil, cookies...)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "Dashboard")
}
