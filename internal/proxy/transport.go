package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"

	"notion-manager/internal/netutil"
)

// Chrome TLS transport using uTLS to mimic Chrome's JA3/JA4 fingerprint.
// Uses http2.Transport for proper HTTP/2 support with custom TLS dial.
//
// dialChromeTLS reads AppConfig.Proxy.NotionProxy at dial time, so
// updating the global proxy via /admin/settings takes effect on the next
// connection without rebuilding the singleton. Idle pooled connections
// are torn down by RebuildChromeTransport so a flipped setting doesn't
// leak across the boundary.
var (
	chromeRoundTripperOnce sync.Once
	chromeRoundTripperH2   *http2.Transport
)

func getChromeRoundTripper() http.RoundTripper {
	chromeRoundTripperOnce.Do(func() {
		chromeRoundTripperH2 = &http2.Transport{
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return dialChromeTLS(ctx, network, addr)
			},
			DisableCompression: true, // We set Accept-Encoding ourselves
		}
	})
	return chromeRoundTripperH2
}

// RebuildChromeTransport drops every idle pooled connection so the next
// notion request re-dials and picks up the freshly-configured upstream
// proxy. Active in-flight requests are unaffected — the http2.Transport
// will simply not lend their connections to new callers anymore.
//
// Called from /admin/settings PUT after persisting a new proxy URL.
func RebuildChromeTransport() {
	getChromeRoundTripper() // ensure init
	if chromeRoundTripperH2 != nil {
		chromeRoundTripperH2.CloseIdleConnections()
	}
}

func dialChromeTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	// Parse host for SNI
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	// Honour the configured TLS dial timeout via a context deadline
	// before delegating to the shared proxy-aware dialer. The dialer
	// already enforces its own 30s connect timeout, but the project's
	// AppConfig.Timeouts.TLSDialTimeout governs the overall budget
	// (raw TCP + TLS handshake) so the caller's expectations hold
	// regardless of which path we take.
	dialCtx := ctx
	if to := AppConfig.TLSDialTimeoutDuration(); to > 0 {
		var cancel context.CancelFunc
		dialCtx, cancel = context.WithTimeout(ctx, to)
		defer cancel()
	}

	rawConn, err := netutil.DialThroughProxy(dialCtx, network, addr, AppConfig.NotionProxyURL())
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}

	// Create uTLS connection with Chrome fingerprint + ALPN h2
	tlsConfig := &utls.Config{
		ServerName:         host,
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2", "http/1.1"},
	}

	tlsConn := utls.UClient(rawConn, tlsConfig, utls.HelloChrome_Auto)

	if err := tlsConn.HandshakeContext(dialCtx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}

	return tlsConn, nil
}

// getChromeHTTPClient returns an http.Client with Chrome TLS fingerprint
func getChromeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: getChromeRoundTripper(),
		Timeout:   timeout,
	}
}
