package regjob

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func tempStorePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "register_history.json")
}

// newTestStore is the standard fixture: open a temp-pathed store and arrange
// for it to be closed on test teardown so the saveLoop goroutine releases
// its hold on TempDir before t.TempDir's cleanup runs.
func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := NewStore(tempStorePath(t), 100)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestStoreCreateAssignsIDAndShape(t *testing.T) {
	s := newTestStore(t)
	emails := []string{"a@x", "b@x"}
	job := s.Create("microsoft", "", 2, 1, emails)
	if job.ID == "" {
		t.Fatalf("missing ID")
	}
	if job.Total != 2 || len(job.Steps) != 2 {
		t.Fatalf("steps not pre-sized: total=%d len=%d", job.Total, len(job.Steps))
	}
	if job.Steps[0].Email != "a@x" || job.Steps[1].Email != "b@x" {
		t.Fatalf("emails not seeded: %+v", job.Steps)
	}
	if job.Steps[0].Status != StepPending {
		t.Fatalf("initial status: got %q", job.Steps[0].Status)
	}
	if job.State != JobRunning {
		t.Fatalf("initial state: got %q", job.State)
	}
	if job.Provider != "microsoft" {
		t.Fatalf("provider not recorded: %q", job.Provider)
	}
}

func TestStoreAddCapsAt100(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 105; i++ {
		s.Create("microsoft", "", 0, 1, nil)
	}
	got := s.List(1000)
	if len(got) != 100 {
		t.Fatalf("expected 100 jobs after cap, got %d", len(got))
	}
}

func TestStorePersistAndReload(t *testing.T) {
	path := tempStorePath(t)
	s, err := NewStore(path, 100)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	job := s.Create("microsoft", "", 1, 1, []string{"keep@x"})
	s.UpdateStep(job.ID, 0, func(st *Step) {
		st.Status = StepOK
		st.UserID = "u1"
	})
	s.Finish(job.ID)
	s.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected store file at %s: %v", path, err)
	}

	s2, err := NewStore(path, 100)
	if err != nil {
		t.Fatalf("reload NewStore: %v", err)
	}
	t.Cleanup(s2.Close)
	got := s2.List(1000)
	if len(got) != 1 {
		t.Fatalf("reloaded list len=%d", len(got))
	}
	if got[0].ID != job.ID {
		t.Fatalf("id mismatch: %s != %s", got[0].ID, job.ID)
	}
	if got[0].State != JobDone {
		t.Fatalf("state not persisted: %q", got[0].State)
	}
	if got[0].Steps[0].Status != StepOK || got[0].Steps[0].UserID != "u1" {
		t.Fatalf("step not persisted: %+v", got[0].Steps[0])
	}
}

func TestStorePersistTrimsToMemoryCapOnReload(t *testing.T) {
	path := tempStorePath(t)
	s, err := NewStore(path, 100)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for i := 0; i < 110; i++ {
		s.Create("microsoft", "", 0, 1, nil)
	}
	// Close to flush snapshot synchronously before reopening.
	s.Close()

	s2, err := NewStore(path, 100)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	t.Cleanup(s2.Close)
	got := s2.List(1000)
	if len(got) > 100 {
		t.Fatalf("reload should respect memory cap, got %d", len(got))
	}
	if len(got) == 0 {
		t.Fatalf("reload should have loaded persisted jobs, got 0")
	}
}

func TestStoreAtomicWriteLeavesNoTmpFile(t *testing.T) {
	path := tempStorePath(t)
	s, err := NewStore(path, 100)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(s.Close)
	for i := 0; i < 5; i++ {
		s.Create("microsoft", "", 0, 1, nil)
	}
	// flush
	s.Finish(s.List(1)[0].ID)

	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("tmp file left behind: %s", e.Name())
		}
	}
}

func TestStoreConcurrentUpdateStep(t *testing.T) {
	s := newTestStore(t)
	emails := make([]string, 200)
	for i := range emails {
		emails[i] = fmt.Sprintf("u%03d@x", i)
	}
	job := s.Create("microsoft", "", len(emails), 8, emails)

	var wg sync.WaitGroup
	for i := range emails {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s.UpdateStep(job.ID, idx, func(st *Step) {
				st.Status = StepOK
			})
		}(i)
	}
	wg.Wait()

	got, ok := s.Get(job.ID)
	if !ok {
		t.Fatal("Get failed")
	}
	if got.OK != len(emails) {
		t.Fatalf("OK counter: want %d got %d", len(emails), got.OK)
	}
	if got.Done != len(emails) {
		t.Fatalf("Done counter: want %d got %d", len(emails), got.Done)
	}
}

func TestStoreSubscribeEmitsSnapshotThenIncremental(t *testing.T) {
	s := newTestStore(t)
	job := s.Create("microsoft", "", 2, 1, []string{"a@x", "b@x"})
	// Pre-existing progress should appear in the snapshot.
	s.UpdateStep(job.ID, 0, func(st *Step) { st.Status = StepOK })

	snap, ch, cancel, err := s.Subscribe(job.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	if snap == nil {
		t.Fatalf("snapshot nil")
	}
	if snap.Steps[0].Status != StepOK {
		t.Fatalf("snapshot did not include prior update: %+v", snap.Steps[0])
	}

	done := make(chan StoreEvent, 4)
	go func() {
		for ev := range ch {
			done <- ev
			if ev.Kind == EventDone {
				return
			}
		}
	}()
	s.UpdateStep(job.ID, 1, func(st *Step) { st.Status = StepFail; st.Message = "boom" })
	s.Finish(job.ID)

	deadline := time.After(2 * time.Second)
	saw := map[EventKind]bool{}
	for len(saw) < 2 {
		select {
		case ev := <-done:
			saw[ev.Kind] = true
		case <-deadline:
			t.Fatalf("timeout, saw=%v", saw)
		}
	}
	if !saw[EventStep] || !saw[EventDone] {
		t.Fatalf("missing events: %v", saw)
	}
}

func TestStoreSubscribeMultipleSubscribers(t *testing.T) {
	s := newTestStore(t)
	job := s.Create("microsoft", "", 1, 1, []string{"a@x"})

	var wg sync.WaitGroup
	var stepEvents atomic.Int32
	for i := 0; i < 3; i++ {
		wg.Add(1)
		_, ch, cancel, err := s.Subscribe(job.ID)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		go func() {
			defer wg.Done()
			defer cancel()
			deadline := time.After(2 * time.Second)
			for {
				select {
				case ev, ok := <-ch:
					if !ok {
						return
					}
					if ev.Kind == EventStep {
						stepEvents.Add(1)
					}
					if ev.Kind == EventDone {
						return
					}
				case <-deadline:
					return
				}
			}
		}()
	}

	s.UpdateStep(job.ID, 0, func(st *Step) { st.Status = StepOK })
	s.Finish(job.ID)
	wg.Wait()

	if got := stepEvents.Load(); got != 3 {
		t.Fatalf("each subscriber should get a step event, got %d", got)
	}
}

func TestStoreSubscribeCancelClosesChannel(t *testing.T) {
	s := newTestStore(t)
	job := s.Create("microsoft", "", 1, 1, []string{"a@x"})
	_, ch, cancel, err := s.Subscribe(job.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	// Updates after cancel must not block the producer even though the
	// subscriber's channel is gone.
	s.UpdateStep(job.ID, 0, func(st *Step) { st.Status = StepOK })

	select {
	case _, ok := <-ch:
		if ok {
			// May get one buffered event; drain and re-check.
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatalf("channel still open after cancel")
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatalf("channel not closed after cancel")
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("channel not closed after cancel")
	}
}

func TestStoreUpdateStepCountsTransitions(t *testing.T) {
	s := newTestStore(t)
	job := s.Create("microsoft", "", 2, 1, []string{"a@x", "b@x"})

	s.UpdateStep(job.ID, 0, func(st *Step) { st.Status = StepRunning })
	got, _ := s.Get(job.ID)
	if got.OK != 0 || got.Fail != 0 || got.Done != 0 {
		t.Fatalf("running shouldn't count: %+v", got)
	}

	s.UpdateStep(job.ID, 0, func(st *Step) { st.Status = StepOK })
	s.UpdateStep(job.ID, 1, func(st *Step) { st.Status = StepFail })
	got, _ = s.Get(job.ID)
	if got.OK != 1 || got.Fail != 1 || got.Done != 2 {
		t.Fatalf("counters wrong: %+v", got)
	}
}

func TestStoreUpdateStepTruncatesLongMessage(t *testing.T) {
	s := newTestStore(t)
	job := s.Create("microsoft", "", 1, 1, []string{"a@x"})

	long := make([]byte, MaxStepMessageBytes*5)
	for i := range long {
		long[i] = 'A'
	}
	s.UpdateStep(job.ID, 0, func(st *Step) {
		st.Status = StepFail
		st.Message = string(long)
	})

	got, _ := s.Get(job.ID)
	if len(got.Steps[0].Message) <= MaxStepMessageBytes {
		t.Logf("truncated len=%d", len(got.Steps[0].Message))
	}
	if len(got.Steps[0].Message) > MaxStepMessageBytes+32 {
		t.Fatalf("message not truncated, len=%d", len(got.Steps[0].Message))
	}
}

func TestStoreFinishMarksDoneState(t *testing.T) {
	s := newTestStore(t)
	job := s.Create("microsoft", "", 1, 1, []string{"a@x"})
	if before, _ := s.Get(job.ID); before.State != JobRunning {
		t.Fatalf("initial state: %s", before.State)
	}
	s.UpdateStep(job.ID, 0, func(st *Step) { st.Status = StepOK })
	s.Finish(job.ID)
	got, _ := s.Get(job.ID)
	if got.State != JobDone {
		t.Fatalf("after Finish, state=%s", got.State)
	}
	if got.EndedAt == 0 {
		t.Fatalf("EndedAt not set")
	}
}

func TestStoreListLimit(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		s.Create("microsoft", "", 0, 1, nil)
	}
	got := s.List(3)
	if len(got) != 3 {
		t.Fatalf("limit not honored: %d", len(got))
	}
	got = s.List(0)
	if len(got) != 5 {
		t.Fatalf("limit=0 should return all: %d", len(got))
	}
}
