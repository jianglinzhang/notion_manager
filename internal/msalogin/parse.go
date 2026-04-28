package msalogin

import (
	"net/url"
	"regexp"
	"strings"
)

// stripJSEscapes decodes the JSON-style escapes embedded inline in a JS
// string literal. login.live.com / account.live.com render ServerData
// with the full set of JSON \uXXXX escapes (notably \u003a for ':' in
// the consent page's sRawInputScopes), plus the MS-specific \/
// slash escape — both of which we need verbatim for the consent
// POST body to be accepted.
func stripJSEscapes(s string) string {
	if s == "" {
		return s
	}
	s = unescapeJSUnicode(s)
	s = strings.ReplaceAll(s, `\/`, `/`)
	s = strings.ReplaceAll(s, `\"`, `"`)
	return s
}

// unescapeJSUnicode rewrites every `\uXXXX` (4 hex digits) sequence
// to the rune it encodes. Invalid escapes are left untouched.
func unescapeJSUnicode(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if i+5 < len(s) && s[i] == '\\' && s[i+1] == 'u' &&
			isHex(s[i+2]) && isHex(s[i+3]) && isHex(s[i+4]) && isHex(s[i+5]) {
			r := (hexVal(s[i+2]) << 12) | (hexVal(s[i+3]) << 8) |
				(hexVal(s[i+4]) << 4) | hexVal(s[i+5])
			b.WriteRune(rune(r))
			i += 6
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

// extractJSString pulls "key":"value" out of inline JS, decoding escapes.
func extractJSString(html, key string) string {
	pat := regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `"\s*:\s*"((?:[^"\\]|\\.)*)"`)
	m := pat.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	return stripJSEscapes(m[1])
}

// extractAttr returns the attribute value (e.g. value="...") from a tag-like
// HTML fragment.
func extractAttr(fragment, attr string) string {
	pat := regexp.MustCompile(attr + `="([^"]+)"`)
	m := pat.FindStringSubmatch(fragment)
	if m == nil {
		return ""
	}
	return m[1]
}

// extractError checks an HTML response for sErrTxt / sErrorCode hints from
// either ESTS or MSA. Empty string means "no error visible".
func extractError(html string) string {
	if m := regexp.MustCompile(`"sErrTxt"\s*:\s*"([^"]*)"`).FindStringSubmatch(html); m != nil && m[1] != "" {
		return m[1]
	}
	if m := regexp.MustCompile(`"sErrorCode"\s*:\s*"([^"]*)"`).FindStringSubmatch(html); m != nil && m[1] != "" {
		return "Error code: " + m[1]
	}
	return ""
}

// parseMSALoginPage extracts the PPFT + urlPost from the login.live.com
// OAuth2 authorize HTML. Mirrors fuckteam/notion_http.py:_parse_msa_login_page.
func parseMSALoginPage(html string) (*msaLoginPage, error) {
	if len(html) < 500 {
		return nil, newErr("msa_parse", "login page HTML too short (%d bytes)", len(html))
	}
	if e := extractError(html); e != "" {
		return nil, newErr("msa_parse", "MSA login page error: %s", e)
	}

	ppft := ""
	if sft := extractJSString(html, "sFTTag"); sft != "" {
		ppft = extractAttr(sft, "value")
	}
	if ppft == "" {
		ppft = extractJSString(html, "sFT")
	}
	if ppft == "" {
		return nil, newErr("msa_parse", "could not extract PPFT from MSA login page")
	}

	urlPost := extractJSString(html, "urlPost")
	if urlPost == "" {
		return nil, newErr("msa_parse", "could not extract urlPost from MSA login page")
	}

	return &msaLoginPage{
		ppft:          ppft,
		urlPost:       urlPost,
		username:      extractJSString(html, "sPOST_Username"),
		correlationID: extractJSString(html, "correlationId"),
	}, nil
}

// parseMSAKmsiPage extracts the NEW PPFT + NEW urlPost from the
// "Stay signed in?" page returned after a successful password POST.
func parseMSAKmsiPage(html string) (*msaKmsiPage, error) {
	if len(html) < 500 {
		return nil, newErr("msa_kmsi_parse", "KMSI page HTML too short (%d bytes)", len(html))
	}
	if e := extractError(html); e != "" {
		return nil, newErr("msa_kmsi_parse", "MSA KMSI page error: %s", e)
	}

	ppft := extractJSString(html, "sFT")
	if ppft == "" {
		if m := regexp.MustCompile(`name="PPFT"[^>]*value="([^"]+)"`).FindStringSubmatch(html); m != nil {
			ppft = m[1]
		}
	}
	if ppft == "" {
		return nil, newErr("msa_kmsi_parse", "could not extract new PPFT from KMSI page")
	}
	urlPost := extractJSString(html, "urlPost")
	if urlPost == "" {
		return nil, newErr("msa_kmsi_parse", "could not extract new urlPost from KMSI page")
	}
	return &msaKmsiPage{ppft: ppft, urlPost: urlPost}, nil
}

// resolveMSURL ensures URLs from the AAD ESTS pages are absolute.
func resolveMSURL(u string) string {
	if u == "" {
		return u
	}
	u = strings.ReplaceAll(u, `\u0026`, `&`)
	if !strings.HasPrefix(u, "http") {
		return "https://login.microsoftonline.com" + u
	}
	return u
}

// parseESTSLoginPage extracts $Config values from a login.microsoftonline.com
// AAD ESTS login page. Used only for the GetCredentialType handshake that
// reveals the FederationRedirectUrl pointing back to login.live.com for
// consumer accounts.
func parseESTSLoginPage(html string) (*estsLoginPage, error) {
	if len(html) < 500 {
		return nil, newErr("ests_parse", "ESTS login page HTML too short (%d bytes)", len(html))
	}
	cfg := &estsLoginPage{randomBlob: "PassportRN"}

	if sft := extractJSString(html, "sFTTag"); sft != "" {
		cfg.ppft = extractAttr(sft, "value")
	}
	if cfg.ppft == "" {
		cfg.ppft = extractJSString(html, "sFT")
	}
	if cfg.ppft == "" {
		if m := regexp.MustCompile(`name="PPFT"[^>]*value="([^"]+)"`).FindStringSubmatch(html); m != nil {
			cfg.ppft = m[1]
		}
	}
	if cfg.ppft == "" {
		return nil, newErr("ests_parse", "could not extract PPFT from ESTS login page")
	}
	cfg.urlPost = resolveMSURL(extractJSString(html, "urlPost"))
	if cfg.urlPost == "" {
		return nil, newErr("ests_parse", "could not extract urlPost from ESTS login page")
	}
	cfg.urlGetCredentialType = resolveMSURL(extractJSString(html, "urlGetCredentialType"))
	if rb := extractJSString(html, "sRandomBlob"); rb != "" {
		cfg.randomBlob = rb
	}
	return cfg, nil
}

// extractCodeFromURL returns the OAuth2 authorization code from a callback
// URL (looking in both query and fragment).
func extractCodeFromURL(rawurl string) (code, state, clientInfo string) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return "", "", ""
	}
	q := u.Query()
	if c := q.Get("code"); c != "" {
		return c, q.Get("state"), q.Get("client_info")
	}
	if u.Fragment != "" {
		fq, err := url.ParseQuery(u.Fragment)
		if err == nil {
			if c := fq.Get("code"); c != "" {
				return c, fq.Get("state"), fq.Get("client_info")
			}
		}
	}
	return "", "", ""
}

// detectMSState classifies an MS HTML response into the next state for the
// state machine. Returns one of: "code_found", "msa_login", "msa_kmsi",
// "ests_login", "bsso_interrupt", "redirect_form", "proofs", "consent",
// "error", "unknown".
func detectMSState(html, currentURL string) string {
	parsed, _ := url.Parse(currentURL)
	path := strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(path, "/microsoftpopupcallback") {
		return "code_found"
	}

	if strings.Contains(html, `"sErrTxt"`) {
		if extractError(html) != "" {
			return "error"
		}
	}

	host := strings.ToLower(parsed.Host)
	isMSA := strings.Contains(host, "login.live.com")

	hasServerData := strings.Contains(html, "var ServerData") || strings.Contains(html, `"ServerData"`)
	hasSFTTag := strings.Contains(html, `"sFTTag"`) && strings.Contains(html, "PPFT")
	hasSFT := strings.Contains(html, `"sFT"`)

	// AAD's Chrome-SSO probe page. When BSSO succeeds it bridges silently;
	// when it fails (our non-browser case) the JS reloads urlPost which is
	// the same authorize URL with sso_reload=True.
	if strings.Contains(html, `name="PageID" content="BssoInterrupt"`) ||
		strings.Contains(html, `"BssoInterrupt"`) ||
		strings.Contains(html, `sso_reload=True`) {
		return "bsso_interrupt"
	}

	if isMSA {
		switch {
		case strings.HasSuffix(path, "/oauth20_authorize.srf") && (hasSFTTag || hasSFT):
			return "msa_login"
		case strings.HasSuffix(path, "/login.srf") && (hasSFTTag || hasSFT):
			return "msa_login"
		case strings.HasSuffix(path, "/ppsecure/post.srf") && hasServerData && hasSFT:
			return "msa_kmsi"
		}
	}

	if regexp.MustCompile(`"urlPost"\s*:`).MatchString(html) &&
		(hasSFTTag || hasSFT || strings.Contains(html, `name="PPFT"`)) {
		return "ests_login"
	}

	if strings.Contains(currentURL, "proofs/Add") || strings.Contains(html, `"proofType"`) ||
		strings.Contains(html, `id="frmAddProof"`) || strings.Contains(currentURL, "proofs/Verify") ||
		strings.Contains(html, `id="frmVerifyProof"`) {
		return "proofs"
	}

	// Microsoft Account ("consumer") OAuth consent SPA — rendered
	// by the Account_UpdateConsentPage_Client React bundle on
	// account.live.com/Consent/Update. The HTML is empty <div
	// id="root"> with a JSON ServerData blob carrying sCanary,
	// sClientId etc. We need a dedicated handler because there's
	// no <form> to fall through to.
	if strings.Contains(currentURL, "/Consent/Update") ||
		strings.Contains(html, `Account_UpdateConsentPage_Client`) ||
		strings.Contains(html, `"sPageId":"Account_UpdateConsentPage_Client"`) {
		return "consent"
	}

	if regexp.MustCompile(`<form[^>]*name="fmHF"`).MatchString(html) ||
		regexp.MustCompile(`<form[^>]*id="fmHF"`).MatchString(html) {
		return "redirect_form"
	}
	if regexp.MustCompile(`<form[^>]*action="[^"]*"`).MatchString(html) {
		return "redirect_form"
	}
	return "unknown"
}

// parseRedirectForm extracts a server-driven HTML auto-submit form (used by
// MS for cross-domain handoffs) into (action, fields). Returns empty action
// when the document is not a redirect form.
func parseRedirectForm(html string) (action string, fields map[string]string) {
	fields = map[string]string{}

	if m := regexp.MustCompile(`<form[^>]*id="?fmHF"?[^>]*action="([^"]+)"`).FindStringSubmatch(html); m != nil {
		action = m[1]
	}
	if action == "" {
		if m := regexp.MustCompile(`<form[^>]*action="([^"]+)"[^>]*id="?fmHF"?`).FindStringSubmatch(html); m != nil {
			action = m[1]
		}
	}
	if action == "" {
		if m := regexp.MustCompile(`<form[^>]*action="([^"]+)"`).FindStringSubmatch(html); m != nil {
			action = m[1]
		}
	}
	for _, m := range regexp.MustCompile(`<input[^>]*type="hidden"[^>]*name="([^"]+)"[^>]*value="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		fields[m[1]] = m[2]
	}
	for _, m := range regexp.MustCompile(`<input[^>]*name="([^"]+)"[^>]*type="hidden"[^>]*value="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		if _, ok := fields[m[1]]; !ok {
			fields[m[1]] = m[2]
		}
	}
	action = resolveMSURL(action)
	return action, fields
}

// extractFormAttr returns the value of attr from the first <form> tag whose
// opening fragment matches matchExpr (a regex matching part of the start
// tag, e.g. `id="frmAddProof"`).
func extractFormAttr(html, matchExpr, attr string) string {
	pat := regexp.MustCompile(`<form[^>]*` + matchExpr + `[^>]*` + attr + `="([^"]+)"`)
	if m := pat.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	pat = regexp.MustCompile(`<form[^>]*` + attr + `="([^"]+)"[^>]*` + matchExpr)
	if m := pat.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// extractInputValue returns the value of the first hidden <input name="...">
// tag matching the given name.
func extractInputValue(html, name string) string {
	pat := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `"\s*[^>]*value="([^"]*)"`)
	if m := pat.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// extractInputValueWherePrefix is like extractInputValue but only returns
// matches whose value starts with prefix.
func extractInputValueWherePrefix(html, name, prefix string) string {
	pat := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `"[^>]*value="(` + regexp.QuoteMeta(prefix) + `[^"]*)"`)
	if m := pat.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// extractClientVersion finds the Notion build hash embedded in the login
// page HTML. Falls back to empty string when neither pattern matches.
func extractClientVersion(html string) string {
	if m := regexp.MustCompile(`src="/_assets/app-([a-f0-9]+)\.js"`).FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := regexp.MustCompile(`src="/_assets/ClientFramework-([a-f0-9]+)\.js"`).FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}
