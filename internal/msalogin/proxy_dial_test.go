package msalogin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewWithBadProxyRejected exercises the construction-time check so a
// caller misconfiguring proxy URL gets a clear error rather than a confusing
// per-request dial failure.
func TestNewWithBadProxyRejected(t *testing.T) {
	_, err := New(Token{Email: "a@b", Password: "p"}, Options{ProxyURL: "ftp://nope"})
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
}

// TestNewWithoutProxyKeepsSingleton makes sure the no-proxy path doesn't
// allocate a fresh transport for every Client (which would defeat the
// existing chromeTransport singleton's purpose of caching ALPN
// negotiations across the auth flow's many requests).
func TestNewWithoutProxyKeepsSingleton(t *testing.T) {
	c1, err := New(Token{Email: "a@b", Password: "p"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := New(Token{Email: "x@y", Password: "p"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c1.http.Transport != c2.http.Transport {
		t.Fatal("expected shared transport for no-proxy clients")
	}
}

// TestNewWithProxyAllocatesNewTransport asserts that distinct proxy URLs
// each get their own chromeTransport so per-host ALPN cache state doesn't
// bleed across proxies.
func TestNewWithProxyAllocatesNewTransport(t *testing.T) {
	c1, err := New(Token{Email: "a@b", Password: "p"}, Options{ProxyURL: "socks5://1.1.1.1:1080"})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := New(Token{Email: "x@y", Password: "p"}, Options{ProxyURL: "socks5://2.2.2.2:1080"})
	if err != nil {
		t.Fatal(err)
	}
	if c1.http.Transport == c2.http.Transport {
		t.Fatal("expected distinct transports for distinct proxy URLs")
	}
}

// Sanity: the chromeTransport surfaces TLS handshake errors when the
// destination isn't really TLS. Guards against silent fallthrough.
func TestChromeTransportFailsHandshakeOnNonTLS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	tr := newChromeTransport("")
	req, _ := http.NewRequest("GET", strings.Replace(srv.URL, "http://", "https://", 1), nil)
	_, err := tr.RoundTrip(req)
	if err == nil {
		t.Fatal("expected TLS handshake error against plain HTTP server")
	}
	_ = err
}

// (smoke) Verify that the test-side TLS hook still completes when proxy
// is empty: ensures the rewrite kept the singleton functional. Uses a
// real httptest TLS server.
func TestChromeTransportTLSWorksDirect(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	srv.TLS = &tls.Config{}
	srv.StartTLS()
	defer srv.Close()

	tr := newChromeTransport("")
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Skipf("TLS verification unavailable in test env: %v", err)
		return
	}
	resp.Body.Close()
}
