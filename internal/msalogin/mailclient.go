package msalogin

import (
	"strings"
	"time"
)

// MailClient is the minimum surface the proofs flow needs from a
// mailbox backend. Both the Graph implementation (OutlookClient) and
// the IMAP XOAUTH2 implementation (IMAPMailClient) satisfy it.
//
// Every method takes (refreshToken, clientID) so the same client can
// service many backups without per-call constructor overhead, and
// because each implementation maintains its own token cache anyway.
type MailClient interface {
	// CollectExistingIDs returns IDs of messages currently in the
	// mailbox so subsequent calls can skip pre-existing mail when
	// hunting for the freshly-arrived verification code.
	CollectExistingIDs(refreshToken, clientID string) map[string]struct{}

	// WaitVerificationCode polls the mailbox for a new message
	// containing a 6-digit code, returning the code or an error.
	// PermanentRefreshError MUST be propagated unchanged so callers
	// can fail fast.
	WaitVerificationCode(
		refreshToken, clientID string,
		skipIDs map[string]struct{},
		timeout, pollInterval time.Duration,
	) (string, error)
}

// imapDomains lists email domains whose OAuth refresh tokens come
// from FOCI consumer client_ids that grant only IMAP/POP/SMTP — never
// Mail.Read. These accounts must use IMAP XOAUTH2 to read mail.
//
// Source of truth: fuckteam/outlook_mail/src/outlook_mail/__init__.py
// (the working Python reference). Add domains here as they emerge;
// removing one only makes sense if Microsoft expands the FOCI client's
// granted scopes (unlikely).
var imapDomains = map[string]struct{}{
	"hotmail.com": {},
	"live.com":    {},
	"live.cn":     {},
	"live.jp":     {},
	"msn.com":     {},
}

// SelectMailClient picks the right backend for an email address:
//
//   - hotmail.com / live.* / msn.com: IMAP XOAUTH2 (tokens lack Mail.Read)
//   - everything else (incl. outlook.com): Graph /me/messages
//
// We never return nil; an empty email defaults to Graph because IMAP
// has no useful XOAUTH2 username to log in with.
func SelectMailClient(userEmail string, timeout time.Duration) MailClient {
	domain := emailDomain(userEmail)
	if domain == "" {
		return NewOutlookClient(timeout)
	}
	if _, ok := imapDomains[domain]; ok {
		return NewIMAPMailClient(userEmail, timeout)
	}
	return NewOutlookClient(timeout)
}

// mailBackendName returns a short string for log lines so operators
// can see at a glance whether a given proofs attempt went through
// Graph or IMAP. It is purely decorative.
func mailBackendName(c MailClient) string {
	switch c.(type) {
	case *IMAPMailClient:
		return "IMAP"
	case *OutlookClient:
		return "Graph"
	default:
		return "mail"
	}
}

// emailDomain extracts the lower-cased domain part of an email
// address. Returns "" if the email is malformed.
func emailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// buildXOAuth2String constructs the SASL XOAUTH2 initial-response
// string per Microsoft's IMAP/POP OAuth spec:
//
//	user=<email>\x01auth=Bearer <access_token>\x01\x01
//
// The 0x01 (Ctrl-A) bytes are mandatory delimiters; they're easy to
// get wrong (whitespace, \r\n, comma all fail silently with a
// "AUTHENTICATE failed" from Outlook). Centralised + unit-tested.
//
// Reference: https://learn.microsoft.com/exchange/client-developer/
// legacy-protocols/how-to-authenticate-an-imap-pop-smtp-application-
// by-using-oauth
func buildXOAuth2String(userEmail, accessToken string) string {
	return "user=" + userEmail + "\x01auth=Bearer " + accessToken + "\x01\x01"
}
