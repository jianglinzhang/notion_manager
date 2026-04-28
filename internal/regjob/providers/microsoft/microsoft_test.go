package microsoft

import (
	"context"
	"errors"
	"strings"
	"testing"

	"notion-manager/internal/regjob/providers"
)

func TestMicrosoftMetadata(t *testing.T) {
	p := New()
	if p.ID() != ID {
		t.Errorf("ID = %q, want %q", p.ID(), ID)
	}
	if p.Display() != Display {
		t.Errorf("Display = %q, want %q", p.Display(), Display)
	}
	if hint := p.FormatHint(); hint == "" || !strings.Contains(hint, "----") {
		t.Errorf("FormatHint = %q, must mention 4-dash separator", hint)
	}
	if p.RecommendedConcurrency() != 1 {
		t.Errorf("RecommendedConcurrency = %d, want 1", p.RecommendedConcurrency())
	}
}

func TestMicrosoftParseFourFields(t *testing.T) {
	p := New()
	in := strings.Join([]string{
		"a@x.com----pw1----cid1----rt1",
		"  ", // blank
		"# a comment",
		"b@x.com----pw2----cid2----rt2",
	}, "\n")
	got, err := p.Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Email != "a@x.com" {
		t.Errorf("emails[0] = %q", got[0].Email)
	}
	if got[0].Raw[FieldPassword] != "pw1" {
		t.Errorf("password[0] = %q", got[0].Raw[FieldPassword])
	}
	if got[0].Raw[FieldClientID] != "cid1" {
		t.Errorf("client_id[0] = %q", got[0].Raw[FieldClientID])
	}
	if got[0].Raw[FieldRefreshToken] != "rt1" {
		t.Errorf("refresh_token[0] = %q", got[0].Raw[FieldRefreshToken])
	}
	if got[1].Email != "b@x.com" || got[1].Raw[FieldPassword] != "pw2" {
		t.Errorf("second row mismatch: %+v", got[1])
	}
}

func TestMicrosoftParseRejectsShortRows(t *testing.T) {
	p := New()
	if _, err := p.Parse("a@x.com----pw1----cid1"); err == nil {
		t.Fatalf("Parse should reject 3-field rows")
	}
	if _, err := p.Parse("----pw1----cid1----rt1"); err == nil {
		t.Fatalf("Parse should reject empty email")
	}
	if _, err := p.Parse("a@x.com--------cid1----rt1"); err == nil {
		t.Fatalf("Parse should reject empty password")
	}
}

func TestMicrosoftParseEmptyInput(t *testing.T) {
	p := New()
	got, err := p.Parse("\n\n# only comments\n\n")
	if err != nil {
		t.Fatalf("Parse(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty input returned %d creds", len(got))
	}
}

func TestMicrosoftLoginPropagatesOptsToFn(t *testing.T) {
	var seen providers.LoginOptions
	p := New()
	p.LoginFn = func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		seen = opts
		return nil, errors.New("ok-stub")
	}
	want := providers.LoginOptions{Proxy: "socks5://user:pw@h:1"}
	_, err := p.Login(context.Background(), providers.Credential{Email: "a@x"}, nil, want)
	if err == nil || !strings.Contains(err.Error(), "ok-stub") {
		t.Fatalf("unexpected err: %v", err)
	}
	if seen != want {
		t.Fatalf("opts not forwarded: got %+v want %+v", seen, want)
	}
}

func TestCredentialRoundTrip(t *testing.T) {
	c := providers.Credential{
		Email: "a@x.com",
		Raw: map[string]string{
			FieldPassword:     "pw",
			FieldClientID:     "cid",
			FieldRefreshToken: "rt",
		},
	}
	tok := credentialToToken(c)
	if tok.Email != "a@x.com" || tok.Password != "pw" || tok.ClientID != "cid" || tok.RefreshToken != "rt" {
		t.Fatalf("roundtrip lost data: %+v", tok)
	}
}
