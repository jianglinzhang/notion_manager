package regjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"notion-manager/internal/regjob/providers"
)

// fakeProvider drives the runner offline. Implementations of Login control
// timing/errors per test; ID is fixed so RegisteredVia stamping is exercised.
type fakeProvider struct {
	id    string
	login func(ctx context.Context, cred providers.Credential, backup *providers.Credential, opts providers.LoginOptions) (*providers.Session, error)
}

func (f *fakeProvider) ID() string                  { return f.id }
func (f *fakeProvider) Display() string             { return f.id }
func (f *fakeProvider) FormatHint() string          { return "fake" }
func (f *fakeProvider) RecommendedConcurrency() int { return 1 }
func (f *fakeProvider) Parse(string) ([]providers.Credential, error) {
	return nil, errors.New("unused")
}
func (f *fakeProvider) Login(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
	return f.login(ctx, c, b, opts)
}

// runnerHarness builds a Store + accounts directory + bookkeeping vars for
// the runner tests. Each helper returns a fresh tempdir so tests don't
// interfere when run in parallel.
type runnerHarness struct {
	store       Store
	accountsDir string
	creds       []providers.Credential
	job         *Job
	reloadCount atomic.Int32
}

func newRunnerHarness(t *testing.T, n int) *runnerHarness {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(filepath.Join(dir, "history.json"), 100)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	accountsDir := filepath.Join(dir, "accounts")
	if err := os.MkdirAll(accountsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	creds := make([]providers.Credential, n)
	emails := make([]string, n)
	for i := 0; i < n; i++ {
		creds[i] = providers.Credential{
			Email: fmt.Sprintf("user%02d@hotmail.com", i),
			Raw: map[string]string{
				"password":      "p",
				"client_id":     "c",
				"refresh_token": fmt.Sprintf("rt%02d", i),
			},
		}
		emails[i] = creds[i].Email
	}
	job := store.Create("microsoft", "", n, 1, emails)
	return &runnerHarness{
		store:       store,
		accountsDir: accountsDir,
		creds:       creds,
		job:         job,
	}
}

func sessionFor(c providers.Credential) *providers.Session {
	return &providers.Session{
		TokenV2:   "tk-" + c.Email,
		UserID:    "u-" + c.Email,
		SpaceID:   "s-" + c.Email,
		UserEmail: c.Email,
	}
}

func TestRunnerRespectsConcurrency(t *testing.T) {
	h := newRunnerHarness(t, 12)
	concurrency := 3

	var inflight atomic.Int32
	var peak atomic.Int32
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		now := inflight.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inflight.Add(-1)
		return sessionFor(c), nil
	}}

	Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: concurrency, AccountsDir: h.accountsDir})

	if got := peak.Load(); got > int32(concurrency) {
		t.Fatalf("peak inflight %d > concurrency %d", got, concurrency)
	}
	if got := peak.Load(); got != int32(concurrency) {
		t.Fatalf("peak inflight should reach concurrency=%d, got %d", concurrency, got)
	}
}

func TestRunnerWritesAccountFileAndCallsOnSuccess(t *testing.T) {
	h := newRunnerHarness(t, 2)
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		return sessionFor(c), nil
	}}

	Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{
		Concurrency: 2,
		AccountsDir: h.accountsDir,
		OnSuccess:   func(string) { h.reloadCount.Add(1) },
	})

	if h.reloadCount.Load() == 0 {
		t.Fatalf("onSuccess never invoked")
	}

	for _, c := range h.creds {
		matched := false
		entries, _ := os.ReadDir(h.accountsDir)
		for _, e := range entries {
			data, _ := os.ReadFile(filepath.Join(h.accountsDir, e.Name()))
			if strings.Contains(string(data), c.Email) {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("no account file found for %s", c.Email)
		}
	}

	got, _ := h.store.Get(h.job.ID)
	if got.OK != 2 || got.Fail != 0 {
		t.Fatalf("counters: OK=%d Fail=%d", got.OK, got.Fail)
	}
}

// TestRunnerOnSuccessReceivesEmail exercises the RunOpts.OnSuccess
// contract: each successful step must hand the just-persisted email back
// to the caller so it can drive a per-account quota refresh.
func TestRunnerOnSuccessReceivesEmail(t *testing.T) {
	h := newRunnerHarness(t, 3)
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		return sessionFor(c), nil
	}}

	var mu sync.Mutex
	got := map[string]bool{}
	Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{
		Concurrency: 2,
		AccountsDir: h.accountsDir,
		OnSuccess: func(email string) {
			mu.Lock()
			defer mu.Unlock()
			got[email] = true
		},
	})

	for _, c := range h.creds {
		if !got[c.Email] {
			t.Fatalf("OnSuccess never invoked for %s; got=%v", c.Email, got)
		}
	}
}

func TestRunnerStampsRegisteredVia(t *testing.T) {
	h := newRunnerHarness(t, 1)
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		return sessionFor(c), nil
	}}
	Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: 1, AccountsDir: h.accountsDir})

	entries, _ := os.ReadDir(h.accountsDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	data, _ := os.ReadFile(filepath.Join(h.accountsDir, entries[0].Name()))
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if v, _ := got["registered_via"].(string); v != "microsoft" {
		t.Fatalf("registered_via = %q, want microsoft", v)
	}
}

func TestRunnerFailureRecordedAndTruncated(t *testing.T) {
	h := newRunnerHarness(t, 1)

	huge := strings.Repeat("X", MaxStepMessageBytes*4)
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		return nil, errors.New(huge)
	}}

	Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: 1, AccountsDir: h.accountsDir})
	got, _ := h.store.Get(h.job.ID)
	if got.Fail != 1 {
		t.Fatalf("Fail counter: %d", got.Fail)
	}
	if got.Steps[0].Status != StepFail {
		t.Fatalf("status: %s", got.Steps[0].Status)
	}
	if len(got.Steps[0].Message) > MaxStepMessageBytes+32 {
		t.Fatalf("message not truncated: len=%d", len(got.Steps[0].Message))
	}

	entries, _ := os.ReadDir(h.accountsDir)
	if len(entries) != 0 {
		t.Fatalf("unexpected files: %v", entries)
	}
}

func TestRunnerBackupPairing(t *testing.T) {
	h := newRunnerHarness(t, 3)

	var seen sync.Map // cred.Email → backup email
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		var bEmail string
		if b != nil {
			bEmail = b.Email
		}
		seen.Store(c.Email, bEmail)
		return sessionFor(c), nil
	}}

	Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: 1, AccountsDir: h.accountsDir})

	wantBackup := map[string]string{
		h.creds[0].Email: h.creds[1].Email,
		h.creds[1].Email: h.creds[2].Email,
		h.creds[2].Email: h.creds[0].Email,
	}
	for email, want := range wantBackup {
		got, ok := seen.Load(email)
		if !ok {
			t.Fatalf("login never called for %s", email)
		}
		if got.(string) != want {
			t.Fatalf("backup for %s: got %s want %s", email, got, want)
		}
	}
}

func TestRunnerCancelledOnContextDone(t *testing.T) {
	h := newRunnerHarness(t, 8)

	ctx, cancel := context.WithCancel(context.Background())
	startedSecond := make(chan struct{}, 1)
	var ran atomic.Int32
	prov := &fakeProvider{id: "microsoft", login: func(c context.Context, cred providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		n := ran.Add(1)
		if n == 2 {
			select {
			case startedSecond <- struct{}{}:
			default:
			}
		}
		select {
		case <-c.Done():
			return nil, c.Err()
		case <-time.After(2 * time.Second):
			return sessionFor(cred), nil
		}
	}}

	done := make(chan struct{})
	go func() {
		Run(ctx, h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: 2, AccountsDir: h.accountsDir})
		close(done)
	}()
	<-startedSecond
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
	}

	got, _ := h.store.Get(h.job.ID)
	if got.OK != 0 {
		t.Fatalf("no successful login expected after cancel, got OK=%d", got.OK)
	}
	pending := 0
	for _, s := range got.Steps {
		if s.Status == StepPending {
			pending++
		}
	}
	if pending == 0 {
		t.Fatalf("expected some pending steps after cancel, got 0")
	}
}

func TestRunnerEmitsFinishOnComplete(t *testing.T) {
	h := newRunnerHarness(t, 1)
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		return sessionFor(c), nil
	}}

	_, ch, sCancel, err := h.store.Subscribe(h.job.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sCancel()

	go Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: 1, AccountsDir: h.accountsDir})

	deadline := time.After(2 * time.Second)
	gotDone := false
	for !gotDone {
		select {
		case ev := <-ch:
			if ev.Kind == EventDone {
				gotDone = true
			}
		case <-deadline:
			t.Fatalf("never received EventDone")
		}
	}

	got, _ := h.store.Get(h.job.ID)
	if got.State != JobDone {
		t.Fatalf("state after Run: %s", got.State)
	}
}

// TestRunnerRejectsIncompleteSession guards the post-fix invariant: even
// if a future provider regression returns a non-nil session whose
// SpaceID/UserID/TokenV2 are empty (the exact shape that produced the 18
// "no_workspace" zombies pre-fix), the runner MUST mark it failed and
// MUST NOT persist anything to accountsDir. Otherwise the dashboard would
// happily list the orphan and /ai would freeze on it again.
func TestRunnerRejectsIncompleteSession(t *testing.T) {
	cases := []struct {
		name string
		s    *providers.Session
	}{
		{"no_space_id", &providers.Session{TokenV2: "tk", UserID: "u"}},
		{"no_user_id", &providers.Session{TokenV2: "tk", SpaceID: "s"}},
		{"no_token", &providers.Session{UserID: "u", SpaceID: "s"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRunnerHarness(t, 1)
			s := tc.s
			s.UserEmail = h.creds[0].Email
			prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
				return s, nil
			}}
			Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: 1, AccountsDir: h.accountsDir})

			got, _ := h.store.Get(h.job.ID)
			if got.OK != 0 || got.Fail != 1 {
				t.Fatalf("counters: OK=%d Fail=%d (expected 0/1)", got.OK, got.Fail)
			}
			if got.Steps[0].Status != StepFail {
				t.Fatalf("step status: %s", got.Steps[0].Status)
			}
			if !strings.Contains(got.Steps[0].Message, "incomplete session") {
				t.Fatalf("error message should mention 'incomplete session', got: %q", got.Steps[0].Message)
			}
			entries, _ := os.ReadDir(h.accountsDir)
			if len(entries) != 0 {
				t.Fatalf("incomplete session should NOT write any file, got %d files: %v", len(entries), entries)
			}
		})
	}
}

func TestRunnerWrittenFileIsValidJSON(t *testing.T) {
	h := newRunnerHarness(t, 1)
	prov := &fakeProvider{id: "microsoft", login: func(ctx context.Context, c providers.Credential, b *providers.Credential, opts providers.LoginOptions) (*providers.Session, error) {
		return &providers.Session{
			TokenV2: "tk-1", UserID: "u-1", SpaceID: "s-1", UserEmail: c.Email,
			SpaceName: "Workspace", PlanType: "free",
		}, nil
	}}
	Run(context.Background(), h.store, h.job.ID, prov, h.creds, RunOpts{Concurrency: 1, AccountsDir: h.accountsDir})

	entries, _ := os.ReadDir(h.accountsDir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(h.accountsDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got["token_v2"] != "tk-1" {
		t.Fatalf("missing token: %v", got)
	}
}
