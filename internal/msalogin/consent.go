package msalogin

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// handleMSConsent finalises the Microsoft personal-account OAuth
// consent prompt that account.live.com renders as a single-page
// React app (Account_UpdateConsentPage_Client). The page has no
// classic <form>, but the React bundle's "Accept" button issues a
// vanilla form-urlencoded POST to the same URL we are already on,
// then 302s through login.live.com/oauth20_authorize.srf?...&res=success
// and ends up at https://www.notion.so/microsoftpopupcallback?code=…
//
// We replay that exact POST in pure Go. The body is composed from
// four ServerData fields embedded in the page HTML:
//
//	ucaction = "Yes"                       (or "No" to deny)
//	client_id = ServerData.sClientId       (the *MSA* app id, NOT the OAuth client_id)
//	scope     = ServerData.sRawInputScopes (space-separated, urlencoded space → "+")
//	cscope    = ServerData.sRawInputGrantedScopes (empty for first-time consent)
//	canary    = ServerData.sCanary
//
// Body byte sequence captured from a real browser session
// (testdata/consent_capture.json) is the source-of-truth spec.
//
// The handler returns ("", currentURL, code, nil) once the OAuth
// code lands in the callback URL; the state machine then short-
// circuits with the captured code.
func (c *Client) handleMSConsent(html, currentURL string) (string, string, string, error) {
	c.logf("consent — entry html_len=%d url=%s", len(html), truncate(currentURL, 120))

	if !strings.Contains(html, "ServerData") || len(html) < 2000 {
		fetched, finalURL, err := c.refetchConsentHTML(currentURL)
		if err != nil {
			return "", "", "", err
		}
		html, currentURL = fetched, finalURL
		c.logf("consent — refetched html_len=%d final_url=%s",
			len(html), truncate(currentURL, 120))
	}

	if path := dumpProofsDebug(c, "consent_entry", currentURL, html); path != "" {
		c.logf("consent — dumped entry HTML to %s", path)
	}

	cfg, err := parseConsentPage(html)
	if err != nil {
		return "", "", "", err
	}
	c.logf("consent — parsed sClientId=%s scopes=%q canary_len=%d granted_len=%d",
		cfg.sClientID, truncate(cfg.scopes, 80), len(cfg.canary), len(cfg.grantedScopes))

	body := encodeConsentForm(cfg, "Yes")
	req, err := http.NewRequest(http.MethodPost, currentURL, strings.NewReader(body))
	if err != nil {
		return "", "", "", newErr("consent", "build POST: %v", err)
	}
	// Header set captured from the browser. The order matters
	// only for User-Agent / Origin / Referer which Microsoft's
	// risk engine sometimes inspects.
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://account.live.com")
	req.Header.Set("Referer", currentURL)
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	c.logf("consent — POSTing accept (%d bytes)", len(body))
	resp, raw, err := c.do(req)
	if err != nil {
		return "", "", "", newErr("consent", "POST: %v", err)
	}
	resp, raw, code, err := c.followRedirects(resp, raw)
	if err != nil {
		return "", "", "", newErr("consent", "follow redirects: %v", err)
	}
	if code != "" {
		c.logf("consent — captured OAuth code (%d chars) via redirect chain", len(code))
		return string(raw), resp.Request.URL.String(), code, nil
	}

	// No code in URL — fall through to the state machine which
	// will inspect the response and either proceed or surface a
	// useful error. Almost always the redirect chain produces
	// the code, so reaching here means Microsoft re-prompted
	// for something we didn't expect; let the next iteration
	// classify the new state.
	c.logf("consent — POST returned without callback code; final url=%s status=%d body_len=%d",
		truncate(resp.Request.URL.String(), 120), resp.StatusCode, len(raw))
	return string(raw), resp.Request.URL.String(), "", nil
}

// refetchConsentHTML re-fetches the consent page through our HTTP
// client when the upstream handler (typically handleRedirectForm)
// only forwarded a Location header without the rendered body.
func (c *Client) refetchConsentHTML(currentURL string) (string, string, error) {
	req, err := http.NewRequest(http.MethodGet, currentURL, nil)
	if err != nil {
		return "", "", newErr("consent", "build GET: %v", err)
	}
	req.Header.Set("Accept",
		"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", newErr("consent", "GET %s: %v", truncate(currentURL, 100), err)
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		return "", "", newErr("consent", "read body: %v", rerr)
	}
	return string(body), resp.Request.URL.String(), nil
}

// consentForm bundles every ServerData field needed for the POST.
type consentForm struct {
	sClientID     string // ServerData.sClientId, e.g. "00000000495E4228"
	scopes        string // ServerData.sRawInputScopes (space-separated)
	grantedScopes string // ServerData.sRawInputGrantedScopes
	canary        string // ServerData.sCanary
}

// parseConsentPage extracts the four ServerData fields the consent
// POST expects. Any missing field is fatal — Microsoft drops the
// POST silently and we'd loop forever in the state machine.
func parseConsentPage(html string) (*consentForm, error) {
	if len(html) < 500 {
		return nil, newErr("consent_parse",
			"consent page HTML too short (%d bytes)", len(html))
	}
	if e := extractError(html); e != "" {
		return nil, newErr("consent_parse",
			"consent page error: %s", e)
	}
	cfg := &consentForm{
		sClientID:     extractJSString(html, "sClientId"),
		scopes:        extractJSString(html, "sRawInputScopes"),
		grantedScopes: extractJSString(html, "sRawInputGrantedScopes"),
		canary:        extractJSString(html, "sCanary"),
	}
	if cfg.sClientID == "" {
		return nil, newErr("consent_parse", "no sClientId in ServerData")
	}
	if cfg.scopes == "" {
		return nil, newErr("consent_parse", "no sRawInputScopes in ServerData")
	}
	if cfg.canary == "" {
		return nil, newErr("consent_parse", "no sCanary in ServerData")
	}
	// sRawInputGrantedScopes is allowed to be empty — first-time
	// consent has no previously-granted scope set.
	return cfg, nil
}

// encodeConsentForm serialises a consent POST body in the exact
// key order the Microsoft React bundle uses. We intentionally
// avoid url.Values{}.Encode() because that sorts keys lexically,
// which differs from the captured browser request.
//
// Key order: ucaction, client_id, scope, cscope, canary.
func encodeConsentForm(cfg *consentForm, ucaction string) string {
	var buf bytes.Buffer
	buf.Grow(64 + len(cfg.scopes) + len(cfg.canary) + len(cfg.grantedScopes))
	writeKV := func(k, v string) {
		if buf.Len() > 0 {
			buf.WriteByte('&')
		}
		buf.WriteString(url.QueryEscape(k))
		buf.WriteByte('=')
		buf.WriteString(url.QueryEscape(v))
	}
	writeKV("ucaction", ucaction)
	writeKV("client_id", cfg.sClientID)
	writeKV("scope", cfg.scopes)
	writeKV("cscope", cfg.grantedScopes)
	writeKV("canary", cfg.canary)
	return buf.String()
}
