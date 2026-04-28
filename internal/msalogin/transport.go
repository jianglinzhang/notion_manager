package msalogin

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"notion-manager/internal/netutil"
)

// Notion + Microsoft endpoints fingerprint inbound TLS handshakes; bare
// Go's stdlib transport gets RST'd. We mimic Chrome via uTLS.
//
// The Microsoft auth flow flips between HTTP/2 (login.microsoftonline.com,
// www.notion.so) and HTTP/1.1 (login.live.com) per request, so we pin a
// per-host protocol decision learned from the first ALPN result and
// dispatch the matching sub-transport.
var (
	chromeTransportInst *chromeTransport
	chromeTransportOnce sync.Once
)

type chromeTransport struct {
	// proxyURL is the optional upstream proxy (HTTP/HTTPS/SOCKS5). Empty
	// means dial directly. Stored on the transport so per-job overrides
	// don't have to thread an extra arg through h1/h2 dial callbacks.
	proxyURL string

	h1 *http.Transport
	h2 *http2.Transport

	mu       sync.Mutex
	protoFor map[string]string
}

// getChromeTransport returns a process-wide singleton transport for callers
// that don't care about proxies. Per-Client transports with proxy overrides
// go through newChromeTransport so the ALPN cache stays partitioned by
// upstream.
func getChromeTransport() http.RoundTripper {
	chromeTransportOnce.Do(func() {
		chromeTransportInst = newChromeTransport("")
	})
	return chromeTransportInst
}

// newChromeTransport builds a transport that dials TCP through proxyURL.
// Empty proxyURL keeps the dial direct. The returned transport is *not*
// shared — callers that need a singleton should use getChromeTransport.
func newChromeTransport(proxyURL string) *chromeTransport {
	ct := &chromeTransport{
		proxyURL: proxyURL,
		protoFor: make(map[string]string),
	}
	ct.h1 = &http.Transport{
		DialTLSContext:        ct.dialH1,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	ct.h2 = &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return ct.dialH2(ctx, network, addr)
		},
		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
	return ct
}

func (c *chromeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return c.h1.RoundTrip(req)
	}
	addr := canonAddr(req.URL.Hostname(), req.URL.Port())
	c.mu.Lock()
	proto, known := c.protoFor[addr]
	c.mu.Unlock()

	// Default to h2 first when unknown (notion.so + most MS endpoints).
	useH2 := !known || proto == "h2"

	body, err := snapshotBody(req)
	if err != nil {
		return nil, err
	}

	if useH2 {
		// Prepare a request that won't be modified by h2 transport.
		req2 := cloneReq(req, body)
		resp, err := c.h2.RoundTrip(req2)
		if err == nil {
			c.mu.Lock()
			c.protoFor[addr] = "h2"
			c.mu.Unlock()
			return resp, nil
		}
		// Detect the "server actually spoke h1" scenario.
		if isH2ProtocolError(err) {
			c.mu.Lock()
			c.protoFor[addr] = "http/1.1"
			c.mu.Unlock()
			req3 := cloneReq(req, body)
			return c.h1.RoundTrip(req3)
		}
		return nil, err
	}
	req3 := cloneReq(req, body)
	return c.h1.RoundTrip(req3)
}

func snapshotBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	buf, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(buf))
	if req.GetBody == nil {
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
	}
	return buf, nil
}

func cloneReq(req *http.Request, body []byte) *http.Request {
	out := req.Clone(req.Context())
	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		out.ContentLength = int64(len(body))
	}
	return out
}

func isH2ProtocolError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "http2:") ||
		strings.Contains(msg, "frame too large") ||
		strings.Contains(msg, "PROTOCOL_ERROR") ||
		strings.Contains(msg, "HTTP/1.1 header")
}

func (c *chromeTransport) dialH1(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.dialChrome(ctx, network, addr, []string{"http/1.1"})
}

func (c *chromeTransport) dialH2(ctx context.Context, network, addr string) (net.Conn, error) {
	conn, err := c.dialChrome(ctx, network, addr, []string{"h2", "http/1.1"})
	if err != nil {
		return nil, err
	}
	// If the server insists on h1, we still hand the conn back; the
	// http2.Transport will produce a protocol error which RoundTrip
	// catches above and retries on the h1 transport.
	return conn, nil
}

func (c *chromeTransport) dialChrome(ctx context.Context, network, addr string, alpn []string) (*utls.UConn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	rawConn, err := netutil.DialThroughProxy(ctx, network, addr, c.proxyURL)
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}
	cfg := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         alpn,
	}
	tlsConn := utls.UClient(rawConn, cfg, utls.HelloChrome_120)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	return tlsConn, nil
}

func canonAddr(host, port string) string {
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(host, port)
}
