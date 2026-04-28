// Package microsoft implements the providers.Provider interface for the
// Microsoft consumer (MSA) Notion onboarding flow. It is a thin adapter over
// internal/msalogin: Parse splits the existing four-field bulk format, and
// Login wraps msalogin.Client.Login() with context cancellation support.
package microsoft

import (
	"context"
	"fmt"
	"strings"
	"time"

	"notion-manager/internal/msalogin"
	"notion-manager/internal/regjob/providers"
)

const (
	ID      = "microsoft"
	Display = "Microsoft"

	formatHint = "每行一个账号：email----password----client_id----refresh_token"

	// Recommended concurrency conservatively errs low because MSA login
	// has anti-abuse heuristics that escalate aggressively past ~5
	// parallel sessions per IP.
	defaultConcurrency = 1

	// loginTimeout caps a single MSA + Notion onboarding flow. The
	// underlying msalogin.Client uses this for per-request HTTP timeouts.
	loginTimeout = 30 * time.Second
)

// Field keys used inside Credential.Raw. Exported so tests can assert
// without string-literal duplication.
const (
	FieldPassword     = "password"
	FieldClientID     = "client_id"
	FieldRefreshToken = "refresh_token"
)

// Provider is the Microsoft adapter. ParseFn / LoginFn are exported seams
// used by tests; production code leaves them nil and falls through to the
// real msalogin implementation.
type Provider struct {
	// ParseFn, when non-nil, replaces the default Parse behavior. Used by
	// tests; nil in production.
	ParseFn func(string) ([]providers.Credential, error)
	// LoginFn, when non-nil, replaces the real msalogin.Client. Used by
	// tests to keep them offline. The opts argument mirrors what the
	// runner passes to the production Login path so tests can assert
	// against e.g. the proxy URL.
	LoginFn func(ctx context.Context, cred providers.Credential, backup *providers.Credential, opts providers.LoginOptions) (*providers.Session, error)
}

// New returns a default Provider that wires Login through msalogin.
func New() *Provider { return &Provider{} }

func (p *Provider) ID() string                  { return ID }
func (p *Provider) Display() string             { return Display }
func (p *Provider) FormatHint() string          { return formatHint }
func (p *Provider) RecommendedConcurrency() int { return defaultConcurrency }

// Parse splits the bulk input into Credentials. The format is the same one
// msalogin.ParseTokens already expects, but we return providers.Credential
// so the runner stays Provider-agnostic.
func (p *Provider) Parse(input string) ([]providers.Credential, error) {
	if p.ParseFn != nil {
		return p.ParseFn(input)
	}
	out := []providers.Credential{}
	lines := strings.Split(input, "\n")
	for i, line := range lines {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		parts := strings.SplitN(s, "----", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("line %d: expected 4 fields separated by '----', got %d", i+1, len(parts))
		}
		email := strings.TrimSpace(parts[0])
		password := strings.TrimSpace(parts[1])
		clientID := strings.TrimSpace(parts[2])
		refreshToken := strings.TrimSpace(parts[3])
		if email == "" || password == "" {
			return nil, fmt.Errorf("line %d: email and password are required", i+1)
		}
		out = append(out, providers.Credential{
			Email: email,
			Raw: map[string]string{
				FieldPassword:     password,
				FieldClientID:     clientID,
				FieldRefreshToken: refreshToken,
			},
		})
	}
	return out, nil
}

// Login executes one MSA + Notion onboarding round. backup, when non-nil,
// is used by msalogin for second-factor proofs (the "next account in the
// list" peer-pairing strategy). opts.Proxy, if set, routes the entire MS
// + Notion handshake through the given upstream proxy URL.
func (p *Provider) Login(ctx context.Context, cred providers.Credential, backup *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
	if p.LoginFn != nil {
		return p.LoginFn(ctx, cred, backup, opts)
	}
	tok := credentialToToken(cred)
	msaOpts := msalogin.Options{Timeout: loginTimeout, ProxyURL: opts.Proxy}
	if backup != nil && backup.Email != "" {
		b := credentialToToken(*backup)
		msaOpts.Backup = &b
	}
	c, err := msalogin.New(tok, msaOpts)
	if err != nil {
		return nil, err
	}
	type result struct {
		s   *msalogin.NotionSession
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := c.Login()
		done <- result{s, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return nil, r.err
		}
		return notionSessionToSession(r.s), nil
	}
}

func credentialToToken(c providers.Credential) msalogin.Token {
	if c.Raw == nil {
		return msalogin.Token{Email: c.Email}
	}
	return msalogin.Token{
		Email:        c.Email,
		Password:     c.Raw[FieldPassword],
		ClientID:     c.Raw[FieldClientID],
		RefreshToken: c.Raw[FieldRefreshToken],
	}
}

func notionSessionToSession(s *msalogin.NotionSession) *providers.Session {
	if s == nil {
		return nil
	}
	return &providers.Session{
		TokenV2:         s.TokenV2,
		UserID:          s.UserID,
		UserName:        s.UserName,
		UserEmail:       s.UserEmail,
		SpaceID:         s.SpaceID,
		SpaceName:       s.SpaceName,
		SpaceViewID:     s.SpaceViewID,
		PlanType:        s.PlanType,
		Timezone:        s.Timezone,
		ClientVersion:   s.ClientVersion,
		BrowserID:       s.BrowserID,
		DeviceID:        s.DeviceID,
		FullCookie:      s.FullCookie,
		AvailableModels: s.AvailableModels,
		ExtractedAt:     s.ExtractedAt,
	}
}
