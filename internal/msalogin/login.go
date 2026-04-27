package msalogin

import (
	"fmt"
	"strings"
)

// Login drives the full Notion SSO login + onboarding pipeline and returns
// the populated NotionSession. The Client is stateful and not safe for
// reuse across multiple Login() calls.
func (c *Client) Login() (*NotionSession, error) {
	if err := c.initNotionSession(); err != nil {
		return nil, err
	}
	code, err := c.msLoginGetCode()
	if err != nil {
		return nil, err
	}
	if code == "" {
		return nil, newErr("ms_state", "MS state machine returned empty code")
	}
	if err := c.exchangeCode(code, c.callbackState, c.callbackClientInfo); err != nil {
		return nil, err
	}
	if err := c.handleOnboarding(); err != nil {
		return nil, err
	}
	return c.extractSession()
}

// ParseTokens parses one or more "<email>----<password>----<client_id>----<refresh_token>"
// lines from raw text (typical clipboard paste / textarea contents).
//
// Empty lines and shell-style comments (lines starting with `#`) are
// silently skipped. Lines with too few parts return a descriptive error
// pointing at the offending line number.
func ParseTokens(raw string) ([]Token, error) {
	out := []Token{}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		parts := strings.SplitN(s, "----", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("line %d: expected 4 fields separated by '----', got %d", i+1, len(parts))
		}
		t := Token{
			Email:        strings.TrimSpace(parts[0]),
			Password:     strings.TrimSpace(parts[1]),
			ClientID:     strings.TrimSpace(parts[2]),
			RefreshToken: strings.TrimSpace(parts[3]),
		}
		if t.Email == "" || t.Password == "" {
			return nil, fmt.Errorf("line %d: email and password are required", i+1)
		}
		out = append(out, t)
	}
	return out, nil
}

// PairBackups assigns each token a backup using the N → N+1 strategy. The
// last token gets the first one wrapping around. This mirrors the bulk
// register loop in fuckteam/notion_export_accounts.py.
func PairBackups(tokens []Token) []*Token {
	if len(tokens) == 0 {
		return nil
	}
	out := make([]*Token, len(tokens))
	for i := range tokens {
		next := tokens[(i+1)%len(tokens)]
		t := next
		out[i] = &t
	}
	return out
}
