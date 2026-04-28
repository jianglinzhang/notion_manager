// Package msalogin implements a pure-Go HTTP client that drives Microsoft
// SSO + Notion onboarding to provision fresh Notion accounts.
//
// It mirrors the Python implementation in fuckteam/notion_http.py, scoped to
// the consumer (MSA / login.live.com) flow used by hotmail.com / outlook.com /
// live.com tokens. Enterprise (AAD ESTS, BSSO, passkey) paths are not
// implemented because the input format we accept is always
// "email----password----client_id----refresh_token", i.e. consumer accounts
// with an Outlook OAuth refresh token.
//
// Output is a NotionSession compatible with notion-manager's Account JSON
// schema (token_v2, user_id, space_id, ...), ready to drop into the
// accounts/ directory.
package msalogin

import "fmt"

// NotionSession is the populated session record for a successfully
// provisioned Notion account. The JSON representation matches Account in
// internal/proxy/types.go so the result can be persisted directly.
type NotionSession struct {
	TokenV2         string                   `json:"token_v2"`
	UserID          string                   `json:"user_id"`
	UserName        string                   `json:"user_name"`
	UserEmail       string                   `json:"user_email"`
	SpaceID         string                   `json:"space_id"`
	SpaceName       string                   `json:"space_name"`
	SpaceViewID     string                   `json:"space_view_id"`
	PlanType        string                   `json:"plan_type"`
	Timezone        string                   `json:"timezone"`
	ClientVersion   string                   `json:"client_version"`
	BrowserID       string                   `json:"browser_id,omitempty"`
	DeviceID        string                   `json:"device_id,omitempty"`
	FullCookie      string                   `json:"full_cookie,omitempty"`
	AvailableModels []map[string]interface{} `json:"available_models"`
	ExtractedAt     string                   `json:"extracted_at"`
}

// Token bundles the fields parsed from a single line of the bulk input
// "<email>----<password>----<client_id>----<refresh_token>".
type Token struct {
	Email        string
	Password     string
	ClientID     string
	RefreshToken string
}

// String returns a redacted human-friendly representation.
func (t Token) String() string {
	rt := t.RefreshToken
	if len(rt) > 12 {
		rt = rt[:6] + "…" + rt[len(rt)-4:]
	}
	return fmt.Sprintf("Token(email=%s client_id=%s rt=%s)", t.Email, t.ClientID, rt)
}

// msOAuthConfig is the OAuth2 authorize URL handed to us by Notion's server.
type msOAuthConfig struct {
	authorizeURL string
	clientID     string
	redirectURI  string
}

// msaLoginPage represents the relevant fields parsed from login.live.com's
// OAuth2 authorize page (the consumer login page).
type msaLoginPage struct {
	ppft          string
	urlPost       string
	username      string
	correlationID string
}

// msaKmsiPage represents the "Stay signed in?" page returned after a
// successful password POST. It carries a NEW PPFT and NEW urlPost (with a
// fresh ``route=`` query) that must be used for the type=28 KMSI submit.
type msaKmsiPage struct {
	ppft    string
	urlPost string
}

// estsLoginPage represents the AAD ESTS login page values (used only when
// the FederationRedirectUrl points back to login.microsoftonline.com, which
// shouldn't normally happen for consumer accounts).
type estsLoginPage struct {
	ppft                 string
	urlPost              string
	urlGetCredentialType string
	randomBlob           string
}

// loginErrorKind classifies recoverable vs fatal MS errors.
type loginErrorKind int

const (
	errKindGeneric loginErrorKind = iota
	errKindBadPassword
	errKindAccountLocked
	errKindAccountNotFound
	errKindProofsRequired
)

// LoginError is returned by Login() on any failure of the MS / Notion flow.
type LoginError struct {
	Stage   string // "ms_oauth_discovery", "msa_login", "proofs", "notion_callback", ...
	Message string
	Kind    loginErrorKind
}

func (e *LoginError) Error() string {
	if e.Stage == "" {
		return e.Message
	}
	return fmt.Sprintf("[%s] %s", e.Stage, e.Message)
}

func newErr(stage, format string, args ...interface{}) *LoginError {
	return &LoginError{Stage: stage, Message: fmt.Sprintf(format, args...)}
}

// IsProofsRequired reports whether the login failed because Microsoft
// demanded an email verification but no working backup mailbox was
// available.
func IsProofsRequired(err error) bool {
	if le, ok := err.(*LoginError); ok {
		return le.Kind == errKindProofsRequired
	}
	return false
}
