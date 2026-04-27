package regjob

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Store is the interface used by HTTP handlers. Implementations must be
// goroutine-safe and broadcast events to all subscribers without blocking
// the producer.
type Store interface {
	// Create starts a new Job. provider should be the providers.Provider.ID()
	// driving the run (e.g. "microsoft"). Empty provider is allowed for
	// backward compatibility but discouraged. proxy is recorded on the
	// Job so the history view and retry path can default to the same
	// upstream that originally produced the run.
	Create(provider, proxy string, total, concurrency int, emails []string) *Job
	Get(id string) (*Job, bool)
	List(limit int) []*Job
	UpdateStep(jobID string, idx int, fn func(*Step))
	Finish(jobID string)
	Subscribe(jobID string) (snapshot *Job, ch <-chan StoreEvent, cancel func(), err error)
	Close()

	// Delete removes a job from the in-memory ring and from disk. The
	// associated input sidecar (if any) is also deleted. Idempotent: a
	// missing job is not an error.
	Delete(jobID string)

	// SaveInputs persists the bulk-register input that produced jobID so
	// the retry handler can reconstruct credentials without re-pasting.
	// payload is JSON-encoded into an input sidecar file next to the
	// history JSON. Returns an error only on disk failure.
	SaveInputs(jobID string, payload SidecarPayload) error
	// LoadInputs returns the previously-persisted SidecarPayload for
	// jobID. ok=false when no sidecar exists (e.g. older jobs created
	// before the retry feature, or jobs whose sidecar was already
	// deleted).
	LoadInputs(jobID string) (payload SidecarPayload, ok bool)
}

// SidecarPayload is what SaveInputs persists: provider id, optional proxy,
// and the credential rows. Stored on disk so the retry handler can rerun
// just the failed steps without forcing the operator to re-paste secrets.
//
// The path layout is `<dir>/.register_inputs/<jobID>.json` where dir is the
// parent of the history.json file the Store was constructed with.
type SidecarPayload struct {
	Provider    string                 `json:"provider"`
	Proxy       string                 `json:"proxy,omitempty"`
	Credentials []sidecarCredentialDTO `json:"credentials"`
}

// sidecarCredentialDTO mirrors providers.Credential without importing
// providers in this file. Handlers convert at the boundary.
type sidecarCredentialDTO struct {
	Email string            `json:"email"`
	Raw   map[string]string `json:"raw"`
}

// SidecarCredential is the public alias callers (handlers) use to build
// payloads. Defined in this package to avoid forcing them to know the
// internal DTO type.
type SidecarCredential = sidecarCredentialDTO

// memoryStore implements Store with an in-memory ring of recent jobs and an
// atomic JSON snapshot on disk for persistence across restarts.
type memoryStore struct {
	path   string
	memCap int

	mu     sync.RWMutex
	jobs   []*Job // newest last (FIFO order of creation); cap = memCap
	subs   map[string]map[int]chan StoreEvent
	subSeq int

	// Save coordination: writes to saveCh trigger an async flush; the
	// background goroutine coalesces a burst into one disk write. Finish()
	// and Create() also call saveSync to guarantee on-disk durability at
	// natural sync points (so a crash never loses a "completed" job).
	saveCh chan struct{}
	stopCh chan struct{}
	saveWG sync.WaitGroup
	saveMu sync.Mutex // serializes concurrent saveSnapshot calls
}

// NewStore opens the store at path (creating the parent directory as needed)
// and loads the most recent memCap jobs into memory. memCap <= 0 defaults
// to 100.
func NewStore(path string, memCap int) (Store, error) {
	if memCap <= 0 {
		memCap = 100
	}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("regjob: mkdir %q: %w", filepath.Dir(path), err)
		}
	}
	s := &memoryStore{
		path:   path,
		memCap: memCap,
		subs:   map[string]map[int]chan StoreEvent{},
		saveCh: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.saveWG.Add(1)
	go s.saveLoop()
	return s, nil
}

// load reads the JSON file (if present), trims to memCap, and stores it.
// Missing file is not an error.
func (s *memoryStore) load() error {
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("regjob: read %q: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var jobs []*Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return fmt.Errorf("regjob: parse %q: %w", s.path, err)
	}
	// Sort by CreatedAt asc so trimming keeps the newest entries.
	sort.SliceStable(jobs, func(i, j int) bool {
		return jobs[i].CreatedAt < jobs[j].CreatedAt
	})
	if len(jobs) > s.memCap {
		jobs = jobs[len(jobs)-s.memCap:]
	}
	s.jobs = jobs
	return nil
}

// saveLoop coalesces save requests so a burst of UpdateStep calls produces
// at most one JSON serialization per ~50ms. It exits promptly on Close so
// tests don't leak the goroutine past TempDir cleanup.
func (s *memoryStore) saveLoop() {
	defer s.saveWG.Done()
	for {
		select {
		case <-s.stopCh:
			s.saveSnapshot()
			return
		case <-s.saveCh:
			// Coalesce a burst, but cut the sleep short if Close arrives.
			select {
			case <-time.After(50 * time.Millisecond):
			case <-s.stopCh:
				s.saveSnapshot()
				return
			}
			s.saveSnapshot()
		}
	}
}

// saveSnapshot does the actual write. saveMu serializes concurrent writes
// to avoid two goroutines racing on tmp + rename.
func (s *memoryStore) saveSnapshot() {
	if s.path == "" {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	snap := make([]*Job, len(s.jobs))
	for i, j := range s.jobs {
		clone := cloneJob(j)
		snap[i] = clone
	}
	s.mu.RUnlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
	}
}

// scheduleSave triggers a write without blocking. Multiple back-to-back
// UpdateStep calls coalesce into a single disk write.
func (s *memoryStore) scheduleSave() {
	select {
	case s.saveCh <- struct{}{}:
	default:
	}
}

func (s *memoryStore) Create(provider, proxy string, total, concurrency int, emails []string) *Job {
	if concurrency <= 0 {
		concurrency = 1
	}
	if total < 0 {
		total = 0
	}
	now := time.Now().UnixMilli()
	steps := make([]Step, total)
	for i := range steps {
		steps[i].Status = StepPending
		if i < len(emails) {
			steps[i].Email = emails[i]
		}
	}
	j := &Job{
		ID:          newID(),
		CreatedAt:   now,
		Provider:    provider,
		Proxy:       proxy,
		Concurrency: concurrency,
		Total:       total,
		State:       JobRunning,
		Steps:       steps,
	}
	s.mu.Lock()
	s.jobs = append(s.jobs, j)
	if len(s.jobs) > s.memCap {
		// Drop oldest. We never have to evict subscribers because subs are
		// indexed by job ID and a subscriber holding an evicted job will
		// simply see Get() return false on next poll; an evicted job that
		// was already terminal poses no new events.
		s.jobs = s.jobs[len(s.jobs)-s.memCap:]
	}
	s.mu.Unlock()
	s.scheduleSave()
	return cloneJob(j)
}

func (s *memoryStore) Get(id string) (*Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return cloneJob(j), true
		}
	}
	return nil, false
}

func (s *memoryStore) List(limit int) []*Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.jobs)
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]*Job, 0, n)
	// Iterate newest-first.
	for i := len(s.jobs) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, cloneJob(s.jobs[i]))
	}
	return out
}

// UpdateStep applies fn to the in-memory step, recomputes counters, and
// broadcasts an EventStep with a clone of the updated step.
func (s *memoryStore) UpdateStep(jobID string, idx int, fn func(*Step)) {
	s.mu.Lock()
	job := s.findLocked(jobID)
	if job == nil || idx < 0 || idx >= len(job.Steps) {
		s.mu.Unlock()
		return
	}
	prev := job.Steps[idx].Status
	st := &job.Steps[idx]
	fn(st)
	st.Message = truncateMessage(st.Message)

	// Recompute counters from scratch when a step transitions across the
	// terminal boundary. This is cheap (Total is small) and immune to
	// re-entrant updates (e.g. fail → ok retry).
	if isTerminal(st.Status) != isTerminal(prev) || st.Status != prev {
		ok, fail := 0, 0
		for _, s := range job.Steps {
			switch s.Status {
			case StepOK:
				ok++
			case StepFail:
				fail++
			}
		}
		job.OK = ok
		job.Fail = fail
		job.Done = ok + fail
	}

	stepCopy := *st
	subsCopy := snapshotSubs(s.subs[jobID])
	s.mu.Unlock()

	broadcast(subsCopy, StoreEvent{
		Kind:    EventStep,
		JobID:   jobID,
		StepIdx: idx,
		Payload: stepCopy,
	})
	s.scheduleSave()
}

// Finish flips the job to done (idempotent) and broadcasts EventDone.
// The disk snapshot is flushed synchronously so callers reading the file
// after Finish returns observe the terminal state.
func (s *memoryStore) Finish(jobID string) {
	s.mu.Lock()
	job := s.findLocked(jobID)
	if job == nil {
		s.mu.Unlock()
		return
	}
	if job.State == JobDone || job.State == JobCancelled {
		s.mu.Unlock()
		return
	}
	job.State = JobDone
	job.EndedAt = time.Now().UnixMilli()
	jobCopy := cloneJob(job)
	subsCopy := snapshotSubs(s.subs[jobID])
	s.mu.Unlock()

	broadcast(subsCopy, StoreEvent{
		Kind:    EventDone,
		JobID:   jobID,
		Payload: jobCopy,
	})
	s.saveSnapshot()
}

// Subscribe returns the current snapshot plus a channel of subsequent events.
// cancel must be called once the consumer is done so the subscription slot
// is released and the channel is closed.
func (s *memoryStore) Subscribe(jobID string) (*Job, <-chan StoreEvent, func(), error) {
	s.mu.Lock()
	job := s.findLocked(jobID)
	if job == nil {
		s.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("regjob: job %s not found", jobID)
	}
	snap := cloneJob(job)
	ch := make(chan StoreEvent, 32)
	if s.subs[jobID] == nil {
		s.subs[jobID] = map[int]chan StoreEvent{}
	}
	s.subSeq++
	id := s.subSeq
	s.subs[jobID][id] = ch
	s.mu.Unlock()

	cancel := func() {
		s.mu.Lock()
		if subs, ok := s.subs[jobID]; ok {
			if c, ok := subs[id]; ok {
				delete(subs, id)
				close(c)
			}
			if len(subs) == 0 {
				delete(s.subs, jobID)
			}
		}
		s.mu.Unlock()
	}
	return snap, ch, cancel, nil
}

// findLocked must be called with s.mu held (read or write).
func (s *memoryStore) findLocked(id string) *Job {
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func snapshotSubs(in map[int]chan StoreEvent) []chan StoreEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]chan StoreEvent, 0, len(in))
	for _, c := range in {
		out = append(out, c)
	}
	return out
}

// broadcast sends an event to each subscriber non-blockingly. Slow consumers
// will simply drop events; SSE consumers always re-fetch via /jobs/{id} on
// reconnect to recover state.
func broadcast(subs []chan StoreEvent, ev StoreEvent) {
	for _, c := range subs {
		select {
		case c <- ev:
		default:
			// Drop on full channel. Subscriber may catch up via snapshot.
		}
	}
}

func isTerminal(s StepStatus) bool {
	return s == StepOK || s == StepFail
}

func cloneJob(j *Job) *Job {
	if j == nil {
		return nil
	}
	out := *j
	if len(j.Steps) > 0 {
		out.Steps = make([]Step, len(j.Steps))
		copy(out.Steps, j.Steps)
	}
	return &out
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fall back to time-based id
		t := time.Now().UnixNano()
		return fmt.Sprintf("%x", t)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// Close stops the background save goroutine and flushes pending writes.
// Idempotent: safe to call from multiple goroutines or multiple times.
func (s *memoryStore) Close() {
	select {
	case <-s.stopCh:
		// Already closed.
	default:
		close(s.stopCh)
	}
	s.saveWG.Wait()
}

// Delete drops jobID from the in-memory ring, force-flushes the snapshot
// so the on-disk history matches, and removes any sidecar input file.
// Idempotent: an unknown jobID is a no-op.
func (s *memoryStore) Delete(jobID string) {
	if jobID == "" {
		return
	}
	s.mu.Lock()
	idx := -1
	for i, j := range s.jobs {
		if j.ID == jobID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		s.jobs = append(s.jobs[:idx], s.jobs[idx+1:]...)
	}
	// Detach subscribers — closing their channels lets HTTP handlers
	// notice the job vanished and break out of their for loop instead of
	// hanging on a never-arriving event.
	subs := s.subs[jobID]
	delete(s.subs, jobID)
	s.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
	s.saveSnapshot()
	_ = s.deleteSidecar(jobID)
}

// sidecarDir returns <dir-of-history>/.register_inputs. The directory is
// created lazily by SaveInputs so a fresh install never has an empty
// folder lying around.
func (s *memoryStore) sidecarDir() string {
	if s.path == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.path), ".register_inputs")
}

func (s *memoryStore) sidecarPath(jobID string) string {
	dir := s.sidecarDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, jobID+".json")
}

func (s *memoryStore) SaveInputs(jobID string, payload SidecarPayload) error {
	if jobID == "" {
		return fmt.Errorf("regjob: empty jobID")
	}
	path := s.sidecarPath(jobID)
	if path == "" {
		// No persistence path — accept silently so in-memory tests work.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("regjob: mkdir sidecar: %w", err)
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("regjob: marshal sidecar: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("regjob: write sidecar: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("regjob: rename sidecar: %w", err)
	}
	return nil
}

func (s *memoryStore) LoadInputs(jobID string) (SidecarPayload, bool) {
	path := s.sidecarPath(jobID)
	if path == "" {
		return SidecarPayload{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SidecarPayload{}, false
	}
	var out SidecarPayload
	if err := json.Unmarshal(data, &out); err != nil {
		return SidecarPayload{}, false
	}
	return out, true
}

func (s *memoryStore) deleteSidecar(jobID string) error {
	path := s.sidecarPath(jobID)
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
