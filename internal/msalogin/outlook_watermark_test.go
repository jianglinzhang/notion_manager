package msalogin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestOutlookCollectExistingIDsStampsWatermark — CollectExistingIDs
// MUST emit a `ts=<RFC3339>;seq=0` key derived from the newest
// receivedDateTime, otherwise WaitVerificationCode falls back to (now-1m)
// and re-accepts stale code mail across rounds of the proofs flow.
func TestOutlookCollectExistingIDsStampsWatermark(t *testing.T) {
	newestRFC := "2026-04-27T06:24:09Z"
	newest, _ := time.Parse(time.RFC3339, newestRFC)

	srv := newGraphStub(t, []graphMsg{
		{ID: "AAA", Subject: "Microsoft account security code", Preview: "Security code: 772265", ReceivedAt: newestRFC},
		{ID: "OLD", Subject: "old", Preview: "no code here", ReceivedAt: "2026-04-25T00:00:00Z"},
	})
	defer srv.Close()

	c := newTestOutlookClient(srv.URL)
	skipIDs := c.CollectExistingIDs("rt", "cid")

	if _, ok := skipIDs[imapWatermarkKey]; !ok {
		t.Fatalf("watermark sentinel key missing from skipIDs: %v", keysOfMap(skipIDs))
	}
	gotTS, gotSeq := extractWatermark(skipIDs)
	if !gotTS.Equal(newest) {
		t.Fatalf("watermark ts mismatch: got %v want %v", gotTS, newest)
	}
	if gotSeq != 0 {
		t.Fatalf("watermark seq should be 0 for Graph backend, got %d", gotSeq)
	}
	if _, ok := skipIDs["AAA"]; !ok {
		t.Fatal("ID dedup must still record message IDs alongside the watermark")
	}
}

// TestOutlookWaitVerificationCodeRejectsStaleCodeAcrossRounds — the
// regression test for the "[ms iter N] verify-only resubmits the same
// 772265 forever" bug from RaymondTran/SherryStevens. Pre-scan the
// mailbox AFTER the first code email arrives, then poll: the stale
// code email must NOT be returned, even though Graph hands it back as
// a "different" id (we simulate Graph's known mutable-id behaviour by
// returning a fresh id on the second list call).
func TestOutlookWaitVerificationCodeRejectsStaleCodeAcrossRounds(t *testing.T) {
	staleAt := "2026-04-27T06:24:09Z"
	freshAt := "2026-04-27T06:24:30Z"

	// Each call to /me/messages returns a different id for the same
	// underlying stale email, mimicking Graph's id mutation. The
	// fresh email arrives only on the 3rd poll.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"AT","expires_in":3600,"scope":"https://graph.microsoft.com/Mail.Read"}`)
			return
		}
		calls++
		var msgs []graphMsg
		switch calls {
		case 1: // pre-scan
			msgs = []graphMsg{{ID: "STALE_v1", Subject: "Microsoft account security code", Preview: "Security code: 772265", ReceivedAt: staleAt}}
		case 2: // 1st poll: stale email re-served with new id
			msgs = []graphMsg{{ID: "STALE_v2", Subject: "Microsoft account security code", Preview: "Security code: 772265", ReceivedAt: staleAt}}
		default: // fresh code finally arrives
			msgs = []graphMsg{
				{ID: "FRESH", Subject: "Microsoft account security code", Preview: "Security code: 552113", ReceivedAt: freshAt},
				{ID: "STALE_v3", Subject: "Microsoft account security code", Preview: "Security code: 772265", ReceivedAt: staleAt},
			}
		}
		writeGraphMessages(w, msgs)
	}))
	defer srv.Close()

	c := newTestOutlookClient(srv.URL)
	c.tokenURL = srv.URL + "/oauth2/v2.0/token"

	skipIDs := c.CollectExistingIDs("rt", "cid")
	got, err := c.WaitVerificationCode("rt", "cid", skipIDs, 5*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitVerificationCode: %v", err)
	}
	if got != "552113" {
		t.Fatalf("MUST return the fresh code; got %q (means watermark let stale 772265 through and would loop MS forever)", got)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

type graphMsg struct {
	ID         string
	Subject    string
	Preview    string
	ReceivedAt string
}

func writeGraphMessages(w http.ResponseWriter, msgs []graphMsg) {
	type body struct {
		Content     string `json:"content"`
		ContentType string `json:"contentType"`
	}
	type item struct {
		ID          string `json:"id"`
		Subject     string `json:"subject"`
		BodyPreview string `json:"bodyPreview"`
		ReceivedAt  string `json:"receivedDateTime"`
		Body        body   `json:"body"`
	}
	resp := struct {
		Value []item `json:"value"`
	}{}
	for _, m := range msgs {
		resp.Value = append(resp.Value, item{
			ID:          m.ID,
			Subject:     m.Subject,
			BodyPreview: m.Preview,
			ReceivedAt:  m.ReceivedAt,
			Body:        body{Content: m.Preview, ContentType: "text"},
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func newGraphStub(t *testing.T, msgs []graphMsg) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"AT","expires_in":3600,"scope":"https://graph.microsoft.com/Mail.Read"}`)
			return
		}
		writeGraphMessages(w, msgs)
	}))
}

func newTestOutlookClient(graphAndTokenBase string) *OutlookClient {
	c := NewOutlookClientWithEndpoint(2*time.Second, graphAndTokenBase+"/oauth2/v2.0/token")
	c.graphBase = graphAndTokenBase
	return c
}

func keysOfMap(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
