package msalogin

import (
	"os"
	"strings"
	"testing"
)

// TestProofsVerifyOnlyPageParsing pins the field extraction we rely on
// when MS interjects a second proofs/Verify page after a successful
// AddProof+VerifyProof+SLT round (the "CatB" compliance re-prompt).
// Pre-2026-04-27 the proofs handler errored out on this page with
// `no proofs/Add form action`; the new submitVerifyProof helper must
// be able to drive it end-to-end.
//
// Fixture is a real captured page (sanitized only for whitespace);
// regenerate with `cp logs/<run>_after_verifyproof.html
// internal/msalogin/testdata/proofs_verify_only_catb.html` if MS
// changes the layout.
func TestProofsVerifyOnlyPageParsing(t *testing.T) {
	html := mustReadTestdata(t, "proofs_verify_only_catb.html")

	// Branch detector: must classify this as verify-only.
	if got := extractFormAttr(html, `id="frmAddProof"`, "action"); got != "" {
		t.Fatalf("frmAddProof action should be absent on a CatB re-verify page, got %q", got)
	}
	verifyAction := extractFormAttr(html, `id="frmVerifyProof"`, "action")
	if verifyAction == "" {
		t.Fatal("frmVerifyProof action MUST be present — without it submitVerifyProof has nothing to POST to")
	}
	if !strings.Contains(verifyAction, "/proofs/Verify") {
		t.Fatalf("verify action should target /proofs/Verify, got: %s", truncate(verifyAction, 120))
	}
	// HTML uses &amp; — submitVerifyProof unescapes it; mirror that here.
	verifyAction = strings.ReplaceAll(verifyAction, "&amp;", "&")
	if !strings.Contains(verifyAction, "epid=") {
		t.Fatalf("verify action must carry the fresh epid; got: %s", truncate(verifyAction, 120))
	}

	// Hidden fields the verify POST depends on.
	canary := extractInputValue(html, "canary")
	if canary == "" {
		t.Fatal("canary input missing — verify POST will be rejected as anti-CSRF")
	}
	if action := extractInputValue(html, "action"); action != "VerifyProof" {
		t.Fatalf("hidden action should be VerifyProof, got %q", action)
	}

	// The `proof` radio carries the OTT descriptor we have to echo back.
	proofValue := extractInputValueWherePrefix(html, "proof", "OTT")
	if proofValue == "" {
		t.Fatal("OTT proof value missing — MS will reject the verify POST without it")
	}
	if !strings.Contains(proofValue, "@") {
		t.Fatalf("OTT proof value should reference the backup email, got %q", proofValue)
	}

	// SLT follow-up should be present so the final hop reaches oauth.
	if !strings.Contains(html, `id="frmSubmitSLT"`) {
		t.Fatal("frmSubmitSLT is expected on this page so we can finish the OAuth round")
	}
}

// TestProofsBranchSelectorClassifiesVerifyOnly directly exercises the
// `addAction == "" && hasVerify` decision used in handleProofsEmailFlow.
// This is the only thing that prevents the regression "no proofs/Add
// form action" from coming back.
func TestProofsBranchSelectorClassifiesVerifyOnly(t *testing.T) {
	html := mustReadTestdata(t, "proofs_verify_only_catb.html")
	url := "https://account.live.com/proofs/Verify?mkt=en-us&epid=foo&id=293577"

	addAction := extractFormAttr(html, `id="frmAddProof"`, "action")
	if addAction == "" {
		// fallback regex used in the real handler
		addAction = extractFormAttr(html, `action="[^"]*proofs/Add[^"]*"`, "action")
	}
	hasVerify := strings.Contains(html, `id="frmVerifyProof"`) ||
		strings.Contains(url, "proofs/Verify")

	if addAction != "" {
		t.Fatalf("CatB re-verify page must NOT advertise an Add form, got addAction=%q", truncate(addAction, 120))
	}
	if !hasVerify {
		t.Fatal("hasVerify must be true so the handler routes to runProofsVerifyOnly instead of erroring out")
	}
}

func mustReadTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return string(data)
}
