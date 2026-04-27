package msalogin

import (
	"net/url"
	"strings"
	"testing"
)

// fixtureServerData reproduces the ServerData fields captured from a
// real account.live.com/Consent/Update page (see
// testdata/consent_capture.json). The string is wrapped in the same
// JSON-ish key/value shape the live page emits so extractJSString
// behaves identically.
var fixtureServerData = `<html><script>` +
	`var ServerData = {` +
	`"sClientId":"00000000495E4228",` +
	`"sRawInputScopes":"00000000495E4228:User.Read 00000000495E4228:int.offline_access",` +
	`"sRawInputGrantedScopes":"",` +
	`"sCanary":"X2wOl9M8cVfg5ZH/URhoCSNt6Ix+INt377g3xrzCpzE=5;DBB7U+o1TLRxiKTz+/5WcKtSCq5KeocHbgtvntdDEZY=5",` +
	`"sUnauthSessionID":"dbec2b894a9d48869a940c97d7ae7891",` +
	`"sPageId":"Account_UpdateConsentPage_Client"` +
	`};</script>` +
	strings.Repeat("x", 600) + // padding so the parser's >500 length guard is happy
	`</html>`

// fixtureCapturedBody is the exact form-urlencoded body the browser
// sent when we clicked Accept (testdata/consent_capture.json
// → consent_post.raw_post_data). encodeConsentForm must reproduce
// it byte-for-byte.
const fixtureCapturedBody = "ucaction=Yes" +
	"&client_id=00000000495E4228" +
	"&scope=00000000495E4228%3AUser.Read+00000000495E4228%3Aint.offline_access" +
	"&cscope=" +
	"&canary=X2wOl9M8cVfg5ZH%2FURhoCSNt6Ix%2BINt377g3xrzCpzE%3D5%3BDBB7U%2Bo1TLRxiKTz%2B%2F5WcKtSCq5KeocHbgtvntdDEZY%3D5"

func TestParseConsentPage_HappyPath(t *testing.T) {
	cfg, err := parseConsentPage(fixtureServerData)
	if err != nil {
		t.Fatalf("parseConsentPage: %v", err)
	}
	if cfg.sClientID != "00000000495E4228" {
		t.Errorf("sClientId = %q, want 00000000495E4228", cfg.sClientID)
	}
	if cfg.scopes != "00000000495E4228:User.Read 00000000495E4228:int.offline_access" {
		t.Errorf("scopes mismatch: %q", cfg.scopes)
	}
	if cfg.grantedScopes != "" {
		t.Errorf("grantedScopes should be empty for first-time consent, got %q", cfg.grantedScopes)
	}
	wantCanary := "X2wOl9M8cVfg5ZH/URhoCSNt6Ix+INt377g3xrzCpzE=5;DBB7U+o1TLRxiKTz+/5WcKtSCq5KeocHbgtvntdDEZY=5"
	if cfg.canary != wantCanary {
		t.Errorf("canary mismatch:\n got  %q\n want %q", cfg.canary, wantCanary)
	}
}

func TestParseConsentPage_MissingFields(t *testing.T) {
	cases := map[string]string{
		"missing sClientId": strings.Replace(fixtureServerData,
			`"sClientId":"00000000495E4228",`, "", 1),
		"missing sRawInputScopes": strings.Replace(fixtureServerData,
			`"sRawInputScopes":"00000000495E4228:User.Read 00000000495E4228:int.offline_access",`, "", 1),
		"missing sCanary": strings.Replace(fixtureServerData,
			`"sCanary":"X2wOl9M8cVfg5ZH/URhoCSNt6Ix+INt377g3xrzCpzE=5;DBB7U+o1TLRxiKTz+/5WcKtSCq5KeocHbgtvntdDEZY=5",`, "", 1),
	}
	for name, html := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConsentPage(html); err == nil {
				t.Fatalf("expected error for %s, got nil", name)
			}
		})
	}
}

func TestParseConsentPage_TooShort(t *testing.T) {
	if _, err := parseConsentPage("hello"); err == nil {
		t.Fatal("expected error for too-short HTML")
	}
}

// TestEncodeConsentForm_MatchesBrowserCapture verifies the emitted
// body matches the bytes captured from a real Chrome session.
func TestEncodeConsentForm_MatchesBrowserCapture(t *testing.T) {
	cfg := &consentForm{
		sClientID:     "00000000495E4228",
		scopes:        "00000000495E4228:User.Read 00000000495E4228:int.offline_access",
		grantedScopes: "",
		canary:        "X2wOl9M8cVfg5ZH/URhoCSNt6Ix+INt377g3xrzCpzE=5;DBB7U+o1TLRxiKTz+/5WcKtSCq5KeocHbgtvntdDEZY=5",
	}
	got := encodeConsentForm(cfg, "Yes")
	if got != fixtureCapturedBody {
		t.Errorf("encoded body mismatch\n got:  %s\n want: %s", got, fixtureCapturedBody)
	}

	// Also verify the body round-trips cleanly through net/url so a
	// downstream parser sees the same fields the browser would.
	parsed, err := url.ParseQuery(got)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if parsed.Get("ucaction") != "Yes" {
		t.Errorf("ucaction=%q", parsed.Get("ucaction"))
	}
	if parsed.Get("client_id") != cfg.sClientID {
		t.Errorf("client_id=%q", parsed.Get("client_id"))
	}
	if parsed.Get("scope") != cfg.scopes {
		t.Errorf("scope=%q", parsed.Get("scope"))
	}
	if parsed.Get("canary") != cfg.canary {
		t.Errorf("canary=%q", parsed.Get("canary"))
	}
	// cscope should be present-but-empty.
	if v, ok := parsed["cscope"]; !ok || len(v) != 1 || v[0] != "" {
		t.Errorf("cscope should be present and empty, got %v (ok=%v)", v, ok)
	}
}

// TestEncodeConsentForm_KeyOrder ensures the body is emitted in the
// browser-observed key order, not Go's lexical order. Microsoft's
// risk engine has been observed to reject form bodies whose key
// ordering looks "machine-generated".
func TestEncodeConsentForm_KeyOrder(t *testing.T) {
	cfg := &consentForm{
		sClientID:     "abc",
		scopes:        "x:y",
		grantedScopes: "z:w",
		canary:        "c1",
	}
	got := encodeConsentForm(cfg, "Yes")
	want := "ucaction=Yes&client_id=abc&scope=x%3Ay&cscope=z%3Aw&canary=c1"
	if got != want {
		t.Errorf("key order wrong\n got:  %s\n want: %s", got, want)
	}
}

// TestEncodeConsentForm_NoConsent confirms we can also emit a
// "deny" body — useful for future error-handling tests even
// though the production flow only ever sends Yes.
func TestEncodeConsentForm_NoConsent(t *testing.T) {
	cfg := &consentForm{sClientID: "x", scopes: "s", canary: "c"}
	body := encodeConsentForm(cfg, "No")
	if !strings.HasPrefix(body, "ucaction=No&") {
		t.Errorf("expected ucaction=No prefix, got %q", body)
	}
}

// TestParseConsentPage_DecodesUnicodeEscapes verifies that ServerData
// values containing JSON \uXXXX escapes (notably \u003a for ':' in
// sRawInputScopes) are decoded before they are POSTed back. Live
// account.live.com pages emit `00000000495E4228\u003aUser.Read`
// instead of `00000000495E4228:User.Read`, and Microsoft rejects the
// consent POST with `oauth20_authorize.srf?res=error&ec=` when the
// raw escape is sent verbatim.
func TestParseConsentPage_DecodesUnicodeEscapes(t *testing.T) {
	html := `<html><script>` +
		`var ServerData = {` +
		`"sClientId":"00000000495E4228",` +
		`"sRawInputScopes":"00000000495E4228\u003aUser.Read 00000000495E4228\u003aint.offline_access",` +
		`"sRawInputGrantedScopes":"",` +
		`"sCanary":"abc\u003ddef",` +
		`"sPageId":"Account_UpdateConsentPage_Client"` +
		`};</script>` +
		strings.Repeat("x", 600) +
		`</html>`

	cfg, err := parseConsentPage(html)
	if err != nil {
		t.Fatalf("parseConsentPage: %v", err)
	}
	wantScopes := "00000000495E4228:User.Read 00000000495E4228:int.offline_access"
	if cfg.scopes != wantScopes {
		t.Errorf("scopes did not decode \\u003a:\n got  %q\n want %q", cfg.scopes, wantScopes)
	}
	if cfg.canary != "abc=def" {
		t.Errorf("canary did not decode \\u003d: got %q want abc=def", cfg.canary)
	}

	// And confirm the encoded body uses the decoded ':' (urlencoded
	// to %3A) rather than the literal backslash-u sequence.
	body := encodeConsentForm(cfg, "Yes")
	if !strings.Contains(body, "scope=00000000495E4228%3AUser.Read+00000000495E4228%3Aint.offline_access") {
		t.Errorf("body should encode ':' as %%3A; got %q", body)
	}
	if strings.Contains(body, `\u003a`) || strings.Contains(body, `%5Cu003a`) {
		t.Errorf("body must not contain raw \\u003a escapes; got %q", body)
	}
}
