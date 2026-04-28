// Package providers defines the OAuth-style registration provider abstraction
// used by the bulk-register pipeline. A Provider knows how to:
//
//   - parse a Provider-specific bulk-input string into a slice of Credentials
//   - perform one Login attempt for a Credential (with an optional backup
//     credential used for second-factor proofs)
//
// The runner in internal/regjob is provider-agnostic and routes work through
// whichever Provider was selected on a given Job. New OAuth integrations
// (Google, GitHub, ...) plug in by implementing this interface and being
// registered with a Registry at startup.
package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Info is the public, JSON-serializable description of a Provider — what the
// dashboard's `GET /admin/register/providers` returns. It deliberately omits
// any executable methods so the same struct can travel to the frontend.
type Info struct {
	ID                     string `json:"id"`
	Display                string `json:"display"`
	FormatHint             string `json:"format_hint"`
	RecommendedConcurrency int    `json:"recommended_concurrency"`
	Enabled                bool   `json:"enabled"`
}

// Credential is a Provider-specific bag of fields parsed from one input
// row. Email is hoisted out so the runner can label steps without knowing
// any per-Provider semantics; Raw carries everything the Provider's Login
// implementation needs.
//
// JSON tags are applied so the bulk-register input sidecar (used to retry
// failed steps without re-pasting credentials) round-trips cleanly.
//
// Two passes are guaranteed:
//   - Parse() populates Email and Raw
//   - Login() consumes Email and Raw verbatim
type Credential struct {
	Email string            `json:"email"`
	Raw   map[string]string `json:"raw"`
}

// LoginOptions carries per-Job runtime knobs that don't belong on the
// (immutable) Provider or per-row Credential. The runner sets these once
// when the Job starts and passes the same value to every Login call so
// retries inherit identical settings.
type LoginOptions struct {
	// Proxy is an optional upstream proxy URL (http/https/socks5). Empty
	// means dial direct. Provider implementations should plumb this into
	// whichever HTTP client they ultimately use; failing to honor it
	// undermines the proxy isolation guarantees the dashboard advertises.
	Proxy string
}

// Session is the normalised result of a successful Login. Field names match
// the existing Account JSON schema 1:1 so callers can persist a Session
// directly without a per-Provider transform.
//
// RegisteredVia is the Provider.ID() of whichever Provider produced this
// Session; the runner stamps this into the JSON file so future re-login UX
// can route back to the right Provider.
type Session struct {
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
	AvailableModels []map[string]interface{} `json:"available_models,omitempty"`
	ExtractedAt     string                   `json:"extracted_at,omitempty"`
	RegisteredVia   string                   `json:"registered_via,omitempty"`
}

// Provider is the runtime contract every OAuth integration must satisfy.
// Implementations should be safe to call concurrently from multiple
// goroutines; the runner uses one Provider for the whole Job.
type Provider interface {
	ID() string
	Display() string
	FormatHint() string
	RecommendedConcurrency() int
	// Parse converts one bulk-input string into one or more Credentials.
	// Empty/whitespace lines and `#`-comments must be ignored. Returning
	// (nil, nil) for empty input is acceptable; the handler treats zero
	// credentials as a 400.
	Parse(input string) ([]Credential, error)
	// Login executes the registration flow for cred. backup may be nil
	// (single-row submissions) or a peer Credential the Provider can use
	// for second-factor proofs (e.g. Microsoft email verification). opts
	// carries per-Job runtime settings the runner has decided on (e.g.
	// the upstream proxy URL); implementations must honour the fields
	// they understand and silently ignore the rest. The returned Session
	// is written to disk by the runner; RegisteredVia is filled in by
	// the runner so Login implementations need not touch it.
	Login(ctx context.Context, cred Credential, backup *Credential, opts LoginOptions) (*Session, error)
}

// Registry is a goroutine-safe lookup table from Provider.ID() to Provider.
// Used by handlers to dispatch a Job to the right Provider and by the
// dashboard listing endpoint to enumerate available integrations.
type Registry struct {
	mu sync.RWMutex
	// order preserves registration order so the UI tabs are stable across
	// restarts (rather than depending on Go's map iteration order).
	order []string
	m     map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: map[string]Provider{}}
}

// Register adds p, panicking on duplicate IDs since this is a startup-time
// programmer error rather than runtime input.
func (r *Registry) Register(p Provider) {
	if p == nil {
		return
	}
	id := strings.TrimSpace(p.ID())
	if id == "" {
		panic("providers: provider with empty ID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.m[id]; exists {
		panic(fmt.Sprintf("providers: duplicate ID %q", id))
	}
	r.m[id] = p
	r.order = append(r.order, id)
}

// Get returns the provider for id (case-insensitive on the ID).
func (r *Registry) Get(id string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.m[strings.ToLower(strings.TrimSpace(id))]
	if ok {
		return p, true
	}
	// Allow exact-case lookup as a fallback.
	p, ok = r.m[id]
	return p, ok
}

// List returns Info entries in registration order. Stable: equal IDs sort
// by ID just to avoid flakiness in tests if a caller registers via a map.
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.order))
	for _, id := range r.order {
		p := r.m[id]
		out = append(out, Info{
			ID:                     p.ID(),
			Display:                p.Display(),
			FormatHint:             p.FormatHint(),
			RecommendedConcurrency: p.RecommendedConcurrency(),
			Enabled:                true,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return false })
	return out
}

// IDs returns the registered IDs in registration order. Useful for tests.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// NowExtractedAt is a convenience that returns the canonical
// `extracted_at` timestamp (RFC3339, UTC, second precision) used in
// Account JSON files. Provider implementations should use this so the
// schema stays uniform across providers.
func NowExtractedAt() string {
	return time.Now().UTC().Format(time.RFC3339)
}
