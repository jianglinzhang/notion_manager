package msalogin

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── Refresh request shape ────────────────────────────────────────────────

// TestRefreshSendsDotDefaultScope is the regression test for the
// "AADSTS70000: scopes requested are unauthorized or expired" outage:
// MSA consumer refresh_tokens issued to FOCI client ids (e.g. the
// Outlook-mobile family `9e5f94bc-...`) MUST request the magical
// `/.default` scope. Asking for any concrete sub-scope like
// `Mail.Read offline_access` triggers a 400 invalid_grant because
// MSA does not support incremental consent on these tokens. The
// reference Python impl (fuckteam/outlook_mail/src/outlook_mail/client.py
// line 18) uses GRAPH_SCOPE = "https://graph.microsoft.com/.default".
func TestRefreshSendsDotDefaultScope(t *testing.T) {
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm = string(body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"AT","expires_in":3600,"scope":"https://graph.microsoft.com/Mail.Read"}`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	tok, err := c.Refresh("rt", "cid")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok != "AT" {
		t.Fatalf("access_token: %s", tok)
	}
	form, perr := url.ParseQuery(gotForm)
	if perr != nil {
		t.Fatalf("body not form-encoded: %v", perr)
	}
	if got := form.Get("scope"); got != "https://graph.microsoft.com/.default" {
		t.Fatalf("scope must be /.default for FOCI consumer tokens, got %q", got)
	}
	if got := form.Get("grant_type"); got != "refresh_token" {
		t.Fatalf("grant_type: %s", got)
	}
	if got := form.Get("client_id"); got != "cid" {
		t.Fatalf("client_id: %s", got)
	}
	if got := form.Get("refresh_token"); got != "rt" {
		t.Fatalf("refresh_token: %s", got)
	}
}

// TestRefreshFailsWhenScopeMissingMailRead protects against the silent
// "Graph 401" outage we observed in plus2.txt: hotmail.com / live.com
// FOCI tokens come back HTTP 200 with a useful access_token, BUT the
// returned `scope` field only carries IMAP/POP/SMTP — no Mail.Read.
// If we hand that token to Graph /me/messages we get a 401 with no
// useful diagnostic, and the proofs poller burns its full timeout for
// nothing. Fail fast with a permanent error so the registration job
// can move on to the next account.
//
// The cure for these tokens is the IMAP XOAUTH2 path; until we ship
// that, the lying-about-Mail.Read case must be terminal.
func TestRefreshFailsWhenScopeMissingMailRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
            "access_token":"AT",
            "expires_in":3600,
            "scope":"https://graph.microsoft.com/.default https://graph.microsoft.com/IMAP.AccessAsUser.All https://graph.microsoft.com/POP.AccessAsUser.All https://graph.microsoft.com/SMTP.Send"
        }`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	_, err := c.Refresh("rt", "cid")
	if err == nil {
		t.Fatalf("expected error when scope lacks Mail.Read")
	}
	if !IsPermanentRefreshError(err) {
		t.Fatalf("must be permanent so caller bails out: %v", err)
	}
	if !strings.Contains(err.Error(), "Mail.Read") {
		t.Fatalf("error must mention missing scope: %v", err)
	}
}

// TestRefreshAcceptsScopeContainingMailRead verifies the happy path
// (e.g. tokens carrying graph.microsoft.com/Mail.Read) still works.
func TestRefreshAcceptsScopeContainingMailRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
            "access_token":"AT",
            "expires_in":3600,
            "scope":"https://graph.microsoft.com/Mail.Read https://graph.microsoft.com/.default"
        }`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	tok, err := c.Refresh("rt", "cid")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok != "AT" {
		t.Fatalf("token: %s", tok)
	}
}

// ── Refresh error classification ─────────────────────────────────────────

// TestRefreshClassifiesInvalidGrantAsPermanent locks down the contract
// that drives the fail-fast fix: Microsoft returning HTTP 400 with
// `error: "invalid_grant"` (AADSTS70000 etc.) is a revoked / expired
// refresh_token and will never self-heal. Refresh MUST surface this as
// a permanent error so WaitVerificationCode can give up immediately
// instead of sleeping for the full 90 s timeout.
func TestRefreshClassifiesInvalidGrantAsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70000: The grant is expired."}`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	_, err := c.Refresh("rt", "cid")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !IsPermanentRefreshError(err) {
		t.Fatalf("invalid_grant must be permanent; got: %v", err)
	}
}

// TestRefreshTreatsServerErrorAsTransient ensures we DON'T fail-fast on
// 5xx blips — Graph occasionally returns a 503 that resolves after a
// retry, and we don't want to abort proofs on a flake.
func TestRefreshTreatsServerErrorAsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"server_error","error_description":"transient"}`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	_, err := c.Refresh("rt", "cid")
	if err == nil {
		t.Fatalf("expected error")
	}
	if IsPermanentRefreshError(err) {
		t.Fatalf("5xx must be treated as transient; got permanent: %v", err)
	}
}

// TestRefreshNetworkErrorTreatedAsTransient covers timeouts / DNS
// failures / connection resets; those are obviously not refresh_token
// problems.
func TestRefreshNetworkErrorTreatedAsTransient(t *testing.T) {
	c := NewOutlookClientWithEndpoint(200*time.Millisecond, "http://127.0.0.1:1") // unroutable
	_, err := c.Refresh("rt", "cid")
	if err == nil {
		t.Fatalf("expected network error")
	}
	if IsPermanentRefreshError(err) {
		t.Fatalf("network error must be transient; got permanent: %v", err)
	}
}

// TestRefreshClassifiesInvalidClientAsPermanent: another OAuth error
// code that is permanent (the client_id is wrong / app deleted).
func TestRefreshClassifiesInvalidClientAsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_client"}`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	_, err := c.Refresh("rt", "cid")
	if err == nil || !IsPermanentRefreshError(err) {
		t.Fatalf("invalid_client must be permanent; got: %v", err)
	}
}

// ── WaitVerificationCode fail-fast behavior ──────────────────────────────

// TestWaitVerificationCodeFailFastOnPermanentRefreshError is THE bug
// from the user's log: PaulMorales7195's 90-second freeze. With the
// fix, a permanent Refresh error must abort the polling loop on the
// first attempt rather than retrying for the full timeout window.
func TestWaitVerificationCodeFailFastOnPermanentRefreshError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"AADSTS70000"}`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	start := time.Now()
	// Use a 30s timeout — pre-fix this would burn the full 30s; post-fix
	// it must return well under one second.
	_, err := c.WaitVerificationCode("rt", "cid", nil, 30*time.Second, 50*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error on permanent refresh failure")
	}
	if !IsPermanentRefreshError(err) {
		t.Fatalf("returned error should be permanent (so caller can map to errKindProofsRequired); got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("fail-fast violated: WaitVerificationCode burned %s on a permanent error", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("permanent error must short-circuit on first call; got %d HTTP attempts", got)
	}
}

// TestWaitVerificationCodeRetriesOnTransientRefreshError verifies the
// non-regression: transient errors (5xx etc.) still get retried within
// the timeout — we don't want to overcorrect and abandon proofs the
// moment Graph hiccups.
func TestWaitVerificationCodeRetriesOnTransientRefreshError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"server_error"}`))
	}))
	defer srv.Close()

	c := NewOutlookClientWithEndpoint(2*time.Second, srv.URL)
	start := time.Now()
	_, err := c.WaitVerificationCode("rt", "cid", nil, 600*time.Millisecond, 100*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
	// Must NOT bail early on transient. Should at least span ~timeout.
	if elapsed < 400*time.Millisecond {
		t.Fatalf("transient error should be retried; bailed too fast (%s)", elapsed)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("expected multiple retries on transient error, got %d", got)
	}
	if IsPermanentRefreshError(err) {
		t.Fatalf("timeout from transient errors should NOT be classified as permanent")
	}
}

// ── proofs error mapping ────────────────────────────────────────────────

// TestPermanentRefreshErrorIsLoginErrorProofsRequired confirms the
// runner-level contract: when handleProofsEmailFlow surfaces a permanent
// Refresh failure, runStep must see it as proofs-required (registration
// failure for THIS account, not a system-wide abort).
func TestPermanentRefreshErrorIsLoginErrorProofsRequired(t *testing.T) {
	// Build the same wrapper handleProofsEmailFlow would build.
	inner := newPermanentRefreshError(errors.New("invalid_grant: revoked"))
	wrapped := &LoginError{Stage: "proofs", Message: inner.Error(), Kind: errKindProofsRequired}
	if !IsProofsRequired(wrapped) {
		t.Fatalf("LoginError(errKindProofsRequired) must satisfy IsProofsRequired")
	}
	// And the inner sentinel keeps its permanence so callers can log
	// the distinction if useful.
	if !IsPermanentRefreshError(inner) {
		t.Fatalf("constructor sentinel must be permanent")
	}
	// Sanity: a generic error is NOT permanent.
	if IsPermanentRefreshError(errors.New("generic boom")) {
		t.Fatalf("plain error should not be permanent")
	}
	// Substring check on the wrapped message keeps the surface stable
	// for any future log-grepping.
	if !strings.Contains(wrapped.Error(), "invalid_grant") {
		t.Fatalf("wrapped message should mention invalid_grant; got: %s", wrapped.Error())
	}
}
