package regjob

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"notion-manager/internal/regjob/providers"
)

// RunOpts bundles the per-Job runtime knobs. We pass it as a value rather
// than positional args so adding a new knob (e.g. a per-Job rate limit)
// doesn't break every call site.
//
// OnSuccess is invoked once per successfully written account JSON file,
// receiving the email of the account just persisted. Callers typically
// reload the AccountPool from disk and kick off a per-account quota
// refresh from the email.
type RunOpts struct {
	Concurrency int
	AccountsDir string
	Proxy       string
	OnSuccess   func(email string)
}

// Run drives bulk registration for a list of credentials, writing one
// Account JSON file per success into opts.AccountsDir, calling opts.OnSuccess
// (if non-nil) after each successful write so the AccountPool can reload.
//
// provider is the Provider implementation responsible for executing each
// individual Login. The runner is otherwise provider-agnostic.
//
// Run blocks until either:
//   - all credentials have been attempted (Steps in terminal state), OR
//   - ctx is cancelled (in-flight workers observe cancellation and return)
//
// In both cases the job transitions to Done via store.Finish.
func Run(
	ctx context.Context,
	store Store,
	jobID string,
	provider providers.Provider,
	creds []providers.Credential,
	opts RunOpts,
) {
	if provider == nil {
		log.Printf("[regjob] Run called with nil provider; aborting job %s", jobID)
		store.Finish(jobID)
		return
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if opts.AccountsDir != "" {
		_ = os.MkdirAll(opts.AccountsDir, 0o755)
	}

	loginOpts := providers.LoginOptions{Proxy: opts.Proxy}

	backups := pairBackups(creds)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	// Serialize file writes so two concurrent successes for the same email
	// (rare, only when an operator submits dups) don't race the rename.
	var writeMu sync.Mutex

dispatch:
	for i := range creds {
		// Bail out cleanly if the context is already cancelled before we
		// ever scheduled this token. The remaining steps stay pending.
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		wg.Add(1)
		go func(idx int, cred providers.Credential, backup *providers.Credential) {
			defer wg.Done()
			defer func() { <-sem }()

			runStep(ctx, store, jobID, idx, provider, cred, backup, loginOpts, opts.AccountsDir, opts.OnSuccess, &writeMu)
		}(i, creds[i], backups[i])
	}
	wg.Wait()
	store.Finish(jobID)
}

// runStep handles one row of the job. It is split out so the worker
// goroutine body stays small.
func runStep(
	ctx context.Context,
	store Store,
	jobID string,
	idx int,
	provider providers.Provider,
	cred providers.Credential,
	backup *providers.Credential,
	loginOpts providers.LoginOptions,
	accountsDir string,
	onSuccess func(email string),
	writeMu *sync.Mutex,
) {
	startedAt := time.Now().UnixMilli()
	store.UpdateStep(jobID, idx, func(st *Step) {
		st.Status = StepRunning
		st.Email = cred.Email
		st.StartedAt = startedAt
	})

	if ctx.Err() != nil {
		store.UpdateStep(jobID, idx, func(st *Step) {
			st.Status = StepFail
			st.Message = "cancelled"
			st.EndedAt = time.Now().UnixMilli()
		})
		return
	}

	session, err := provider.Login(ctx, cred, backup, loginOpts)
	if err != nil {
		log.Printf("[regjob] FAIL %s (%s): %v", cred.Email, provider.ID(), err)
		msg := err.Error()
		if errors.Is(err, context.Canceled) {
			msg = "cancelled"
		}
		store.UpdateStep(jobID, idx, func(st *Step) {
			st.Status = StepFail
			st.Message = msg
			st.EndedAt = time.Now().UnixMilli()
		})
		return
	}
	if session == nil {
		store.UpdateStep(jobID, idx, func(st *Step) {
			st.Status = StepFail
			st.Message = "login returned empty session"
			st.EndedAt = time.Now().UnixMilli()
		})
		return
	}
	// Belt-and-braces: providers SHOULD have already errored on a
	// half-baked session, but cheap to double-check here. Writing an
	// account file with no space_id leaves a zombie that surfaces as
	// "no_workspace" on the dashboard and a perpetual /ai skeleton.
	if session.SpaceID == "" || session.UserID == "" || session.TokenV2 == "" {
		log.Printf("[regjob] FAIL %s (%s): provider returned incomplete session (space_id=%q user_id=%q token_v2_len=%d)",
			cred.Email, provider.ID(), session.SpaceID, session.UserID, len(session.TokenV2))
		store.UpdateStep(jobID, idx, func(st *Step) {
			st.Status = StepFail
			st.Message = "incomplete session: missing space_id/user_id/token_v2"
			st.EndedAt = time.Now().UnixMilli()
		})
		return
	}

	// Stamp provenance so the dashboard can surface "via Microsoft" and
	// (later) wire up "Re-login" through the same provider.
	if session.RegisteredVia == "" {
		session.RegisteredVia = provider.ID()
	}

	var fileBase string
	if accountsDir != "" {
		writeMu.Lock()
		path := filepath.Join(accountsDir, accountFilename(cred.Email))
		err := writeAccountFile(path, session)
		writeMu.Unlock()
		if err != nil {
			store.UpdateStep(jobID, idx, func(st *Step) {
				st.Status = StepFail
				st.Message = "write: " + err.Error()
				st.EndedAt = time.Now().UnixMilli()
			})
			return
		}
		fileBase = filepath.Base(path)
	}

	if onSuccess != nil {
		onSuccess(cred.Email)
	}

	log.Printf("[regjob] OK %s (%s) → %s", cred.Email, provider.ID(), fileBase)
	store.UpdateStep(jobID, idx, func(st *Step) {
		st.Status = StepOK
		st.SpaceID = session.SpaceID
		st.UserID = session.UserID
		st.File = fileBase
		st.Message = ""
		st.EndedAt = time.Now().UnixMilli()
	})
}

// pairBackups assigns each credential a backup using the N → N+1 strategy.
// The last cred wraps to the first. With a single-element list the backup
// is nil (no peer available).
func pairBackups(creds []providers.Credential) []*providers.Credential {
	out := make([]*providers.Credential, len(creds))
	if len(creds) <= 1 {
		return out
	}
	for i := range creds {
		next := creds[(i+1)%len(creds)]
		out[i] = &next
	}
	return out
}

var safeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func accountFilename(email string) string {
	clean := safeFilenameChars.ReplaceAllString(strings.ToLower(email), "_")
	clean = strings.Trim(clean, "_")
	if clean == "" {
		clean = "account"
	}
	return clean + ".json"
}

func writeAccountFile(path string, s *providers.Session) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
