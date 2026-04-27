package msalogin

import (
	"strings"
	"testing"
)

func TestParseTokensSingleLine(t *testing.T) {
	raw := "user@hotmail.com----pass1234----9e5f94bc-e8a4-4e73-b8be-63364c29d753----M.C513_BAY.0.U.AAA"
	got, err := ParseTokens(raw)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 token, got %d", len(got))
	}
	want := Token{
		Email:        "user@hotmail.com",
		Password:     "pass1234",
		ClientID:     "9e5f94bc-e8a4-4e73-b8be-63364c29d753",
		RefreshToken: "M.C513_BAY.0.U.AAA",
	}
	if got[0] != want {
		t.Fatalf("token mismatch:\n got %#v\nwant %#v", got[0], want)
	}
}

func TestParseTokensMultiLineWithCommentsAndBlank(t *testing.T) {
	raw := strings.Join([]string{
		"# bulk register input",
		"",
		"a@hotmail.com----p1----c1----rt1",
		"  b@hotmail.com----p2----c2----rt2  ",
		"# end",
	}, "\n")
	got, err := ParseTokens(raw)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 tokens, got %d", len(got))
	}
	if got[0].Email != "a@hotmail.com" || got[1].Email != "b@hotmail.com" {
		t.Fatalf("emails: %v %v", got[0], got[1])
	}
}

func TestParseTokensRejectsTooFewFields(t *testing.T) {
	raw := "user@hotmail.com----pass----client_id"
	_, err := ParseTokens(raw)
	if err == nil {
		t.Fatalf("expected error for 3-field line")
	}
	if !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("error should reference line number, got %v", err)
	}
}

func TestParseTokensRejectsBlankFields(t *testing.T) {
	raw := "----pass----c----rt"
	_, err := ParseTokens(raw)
	if err == nil {
		t.Fatalf("expected error for empty email")
	}
}

func TestPairBackupsRotates(t *testing.T) {
	toks := []Token{
		{Email: "a@x"}, {Email: "b@x"}, {Email: "c@x"},
	}
	got := PairBackups(toks)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0].Email != "b@x" || got[1].Email != "c@x" || got[2].Email != "a@x" {
		t.Fatalf("rotation wrong: %v %v %v", got[0].Email, got[1].Email, got[2].Email)
	}
}

func TestExtractCodeFromURL(t *testing.T) {
	rawurl := "https://www.notion.so/microsoftpopupcallback?code=ABC123&state=eyJ4Ijoi&client_info=info"
	code, state, info := extractCodeFromURL(rawurl)
	if code != "ABC123" || state != "eyJ4Ijoi" || info != "info" {
		t.Fatalf("got code=%q state=%q info=%q", code, state, info)
	}
}

func TestExtractCodeFromFragment(t *testing.T) {
	rawurl := "https://x/?#code=DEF&state=ZZ"
	code, state, _ := extractCodeFromURL(rawurl)
	if code != "DEF" || state != "ZZ" {
		t.Fatalf("got code=%q state=%q", code, state)
	}
}

func TestParseOAuthStateBase64URLEncoded(t *testing.T) {
	// Encoded: {"encryptedToken":"abc","encryptedNonce":"def","callbackType":"popup"}
	raw := "eyJlbmNyeXB0ZWRUb2tlbiI6ImFiYyIsImVuY3J5cHRlZE5vbmNlIjoiZGVmIiwiY2FsbGJhY2tUeXBlIjoicG9wdXAifQ"
	obj := parseOAuthState(raw)
	if obj == nil {
		t.Fatalf("parse failed")
	}
	if obj["encryptedToken"] != "abc" || obj["callbackType"] != "popup" {
		t.Fatalf("unexpected obj: %#v", obj)
	}
}

func TestExtractError(t *testing.T) {
	html := `var ServerData = {"sErrTxt":"Account locked","other":"x"};`
	if got := extractError(html); got != "Account locked" {
		t.Fatalf("got %q", got)
	}
}

func TestExtractClientVersion(t *testing.T) {
	html := `<script src="/_assets/app-abc1234567890.js"></script>`
	if v := extractClientVersion(html); v != "abc1234567890" {
		t.Fatalf("got %q", v)
	}
}

func TestExtractCodeFromVerificationEmail(t *testing.T) {
	for _, c := range []struct {
		subj, body, want string
	}{
		{"Microsoft account security code", "The code is 123456 and please use it.", "123456"},
		{"Verify your email - 654321", "ignore", "654321"},
		{"Login code: 999000 (do not share)", "", "999000"},
	} {
		if got := extractVerificationCode(c.subj, "", c.body); got != c.want {
			t.Errorf("subj=%q body=%q: got %q want %q", c.subj, c.body, got, c.want)
		}
	}
}
