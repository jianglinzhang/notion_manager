package msalogin

import (
	"testing"
)

// TestParseCreateSpaceResponseAcceptsBareSpace mirrors the live
// browser-captured /createSpace response: spaceId + a populated space
// record, but NO space_view at all. This is the post-2026 contract;
// the old "ghost space rejection" test has been retired because the
// SPA now relies on a follow-up saveTransactionsMain (attachSpaceView)
// to materialise space_view, so a bare /createSpace response is the
// happy path, not a failure.
func TestParseCreateSpaceResponseAcceptsBareSpace(t *testing.T) {
	body := []byte(`{
		"spaceId": "bad8d1ca-4499-817e-bc07-00030e7a7a1a",
		"recordMap": {
			"space": {
				"bad8d1ca-4499-817e-bc07-00030e7a7a1a": {
					"value": { "value": { "id": "bad8d1ca-4499-817e-bc07-00030e7a7a1a", "name": "Penny" } }
				}
			}
		},
		"inviteLinkCode": "abc"
	}`)
	space, err := parseCreateSpaceResponse(body, "fallback")
	if err != nil {
		t.Fatalf("post-2026 createSpace responses lack space_view; should not be rejected: %v", err)
	}
	if space["id"] != "bad8d1ca-4499-817e-bc07-00030e7a7a1a" {
		t.Fatalf("space id mismatch: %v", space["id"])
	}
	if space["name"] != "Penny" {
		t.Fatalf("space name should come from RecordMap, got: %v", space["name"])
	}
}

// TestParseCreateSpaceResponseRejectsMissingSpaceID guards against a
// response that omits spaceId — Notion has been known to return that on
// throttled batch flows.
func TestParseCreateSpaceResponseRejectsMissingSpaceID(t *testing.T) {
	body := []byte(`{"recordMap":{"space":{}}}`)
	_, err := parseCreateSpaceResponse(body, "fallback")
	if err == nil {
		t.Fatalf("expected error for missing spaceId")
	}
}

// TestParseCreateSpaceResponseRejectsMalformedJSON covers transport-side
// HTML/error pages that occasionally slip through with HTTP 200.
func TestParseCreateSpaceResponseRejectsMalformedJSON(t *testing.T) {
	_, err := parseCreateSpaceResponse([]byte("<html>not json</html>"), "fallback")
	if err == nil {
		t.Fatalf("expected error for non-JSON body")
	}
}

// TestParseCreateSpaceResponseSuccess proves the happy path matches
// what the live browser sees: just spaceId + space record (no
// space_view), and we return the inner space.value.value map.
func TestParseCreateSpaceResponseSuccess(t *testing.T) {
	body := []byte(`{
		"spaceId": "8cf19636-0abb-8190-8746-0003e616dc1e",
		"recordMap": {
			"space": {
				"8cf19636-0abb-8190-8746-0003e616dc1e": {
					"value": { "value": {
						"id": "8cf19636-0abb-8190-8746-0003e616dc1e",
						"name": "Carrie's workspace",
						"plan_type": "personal"
					} }
				}
			}
		}
	}`)
	space, err := parseCreateSpaceResponse(body, "fallback")
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if space["id"] != "8cf19636-0abb-8190-8746-0003e616dc1e" {
		t.Fatalf("space id mismatch: %v", space["id"])
	}
	if space["name"] != "Carrie's workspace" {
		t.Fatalf("space name should come from RecordMap (not fallback), got: %v", space["name"])
	}
}

// TestParseCreateSpaceResponseUsesFallbackName proves that when Notion
// omits the inner space record we still report the fallback name (so
// downstream cache stays usable).
func TestParseCreateSpaceResponseUsesFallbackName(t *testing.T) {
	body := []byte(`{
		"spaceId": "id1",
		"recordMap": { "space": {} }
	}`)
	space, err := parseCreateSpaceResponse(body, "fallback name")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if space["name"] != "fallback name" {
		t.Fatalf("name = %v, want fallback", space["name"])
	}
	if space["id"] != "id1" {
		t.Fatalf("id mismatch: %v", space["id"])
	}
}

// TestUserRootHasSpaceViewEmpty replicates the exact /loadUserContent
// shape we observed from the 18 broken accounts (user_root present but
// only contains "id" + "version") — the verifier MUST reject it.
func TestUserRootHasSpaceViewEmpty(t *testing.T) {
	uid := "34dd872b-594c-81f0-8545-00026b87708f"
	body := []byte(`{
		"recordMap": {
			"user_root": {
				"` + uid + `": { "value": { "value": { "id": "` + uid + `", "version": 12 } } }
			}
		}
	}`)
	if userRootHasSpaceView(body, uid) {
		t.Fatalf("empty user_root should not be considered ready")
	}
}

// TestUserRootHasSpaceViewMissingUser asserts a response that doesn't
// even carry the requested user_root row is treated as "not ready".
func TestUserRootHasSpaceViewMissingUser(t *testing.T) {
	body := []byte(`{"recordMap":{"user_root":{}}}`)
	if userRootHasSpaceView(body, "any-user") {
		t.Fatalf("missing user_root row should be not-ready")
	}
}

// TestUserRootHasSpaceViewWithPointers covers the success signal: a
// non-empty space_view_pointers array means Notion has finally linked
// the workspace to this user.
func TestUserRootHasSpaceViewWithPointers(t *testing.T) {
	uid := "user-1"
	body := []byte(`{
		"recordMap": {
			"user_root": {
				"` + uid + `": { "value": { "value": {
					"id": "` + uid + `",
					"space_view_pointers": [{"id":"sv1","spaceId":"sp1","table":"space_view"}]
				} } }
			}
		}
	}`)
	if !userRootHasSpaceView(body, uid) {
		t.Fatalf("populated space_view_pointers should be ready")
	}
}

// TestUserRootHasSpaceViewWithLegacyArray ensures the older
// `space_views` field (string array) is also accepted, since some older
// Notion variants surface only that.
func TestUserRootHasSpaceViewWithLegacyArray(t *testing.T) {
	uid := "user-2"
	body := []byte(`{
		"recordMap": {
			"user_root": {
				"` + uid + `": { "value": { "value": {
					"id": "` + uid + `",
					"space_views": ["sv-x"]
				} } }
			}
		}
	}`)
	if !userRootHasSpaceView(body, uid) {
		t.Fatalf("populated space_views array should be ready")
	}
}

// TestUserRootHasSpaceViewMalformed covers the network-error path: any
// JSON parse failure is treated as not-ready (caller polls again).
func TestUserRootHasSpaceViewMalformed(t *testing.T) {
	if userRootHasSpaceView([]byte("not json"), "u") {
		t.Fatalf("malformed body should never be considered ready")
	}
}
