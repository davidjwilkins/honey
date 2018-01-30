package fetch

import (
	"net/http"
	"net/url"
	"testing"
)

func newTestValidRequest() *http.Request {
	url, err := url.Parse("https://www.insomniac.com")
	if err != nil {
		panic(err)
	}
	return &http.Request{
		Method: http.MethodGet,
		URL:    url,
		Header: http.Header{},
	}
}

func TestSwitchBackendUrlIsChanged(t *testing.T) {
	backend, err := url.Parse("http://www.backend.com")
	if err != nil {
		panic(err)
	}
	r := newTestValidRequest()
	rewriter := NewRewriter(backend)
	rewriter.SwitchBackend(r)
	if r.URL.Scheme != "http" {
		t.Error("SwitchBackend should switch requests scheme to the correct one")
	}
	if r.URL.Host != "www.backend.com" {
		t.Error("SwitchBackend should switch requests host to the correct one")
	}
	if r.Host != "www.insomniac.com" {
		t.Errorf("Expected host www.insomniac.com, got %s", r.Host)
	}
}

func TestSwitchBackendXForwarderProtoIsAdded(t *testing.T) {
	backend, err := url.Parse("http://www.backend.com")
	if err != nil {
		panic(err)
	}
	r := newTestValidRequest()
	rewriter := NewRewriter(backend)
	rewriter.SwitchBackend(r)
	if r.Header.Get("X-Forwarded-Proto") != "https" {
		t.Error("SwitchBackend should set X-Forwarded-Proto header")
	}
}
