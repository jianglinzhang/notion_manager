// Package regjob provides a job-tracking primitive for bulk Microsoft SSO
// account registration. It owns:
//
//   - Job / Step value types describing one bulk run
//   - Store: an in-memory + disk-backed registry of recent jobs that also
//     broadcasts step updates to SSE subscribers
//   - Runner: a concurrent worker that drives msalogin.Client.Login() across
//     a list of tokens, writing one Account JSON file per success
//
// The package is decoupled from net/http and from msalogin so it can be unit
// tested in isolation: callers inject a LoginFunc and an onSuccess callback.
package regjob

// StepStatus is the lifecycle of a single email's registration attempt.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepOK      StepStatus = "ok"
	StepFail    StepStatus = "fail"
)

// JobState is the lifecycle of a whole bulk run.
type JobState string

const (
	JobRunning   JobState = "running"
	JobDone      JobState = "done"
	JobCancelled JobState = "cancelled"
)

// Step is the per-email row of a Job.
//
// The JSON shape is what the dashboard receives over SSE and via the history
// list; any rename here must be mirrored in web/src/api.ts.
type Step struct {
	Email     string     `json:"email"`
	Status    StepStatus `json:"status"`
	Message   string     `json:"message,omitempty"`
	SpaceID   string     `json:"space_id,omitempty"`
	UserID    string     `json:"user_id,omitempty"`
	File      string     `json:"file,omitempty"`
	StartedAt int64      `json:"started_at,omitempty"` // unix ms
	EndedAt   int64      `json:"ended_at,omitempty"`   // unix ms
}

// Job is a single bulk register run. Steps is sized at Total when the job is
// created and is mutated in-place via Store.UpdateStep so subscribers can
// observe progress without copying the whole slice on every update.
//
// Provider is the providers.Provider.ID() that drove this Job. Older
// histories without a provider are rendered by the dashboard as "microsoft"
// (the only provider available before this field existed).
//
// Proxy, when non-empty, is the upstream proxy URL the runner used for
// every Login call in this Job. It's persisted with the Job so the
// dashboard can a) display it in the history view and b) default the
// retry job's proxy to the original value.
type Job struct {
	ID          string   `json:"id"`
	CreatedAt   int64    `json:"created_at"` // unix ms
	EndedAt     int64    `json:"ended_at,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Proxy       string   `json:"proxy,omitempty"`
	Concurrency int      `json:"concurrency"`
	Total       int      `json:"total"`
	OK          int      `json:"ok"`
	Fail        int      `json:"fail"`
	Done        int      `json:"done"`
	State       JobState `json:"state"`
	Steps       []Step   `json:"steps"`
}

// EventKind classifies a StoreEvent payload.
type EventKind string

const (
	EventSnapshot EventKind = "snapshot" // initial event delivered to a new subscriber
	EventStep     EventKind = "step"     // a single Step was updated
	EventDone     EventKind = "done"     // job transitioned to a terminal state
)

// StoreEvent is what Store broadcasts to subscribers. The receiver should
// always JSON-encode Payload before sending it to the wire.
type StoreEvent struct {
	Kind    EventKind   `json:"kind"`
	JobID   string      `json:"job_id"`
	StepIdx int         `json:"step_idx,omitempty"` // only valid for EventStep
	Payload interface{} `json:"payload"`            // *Job for snapshot/done, Step for step
}

// MaxStepMessageBytes caps the failure message stored per step. Microsoft
// error pages can balloon to tens of KB; we keep only the first 1024 bytes.
const MaxStepMessageBytes = 1024

// truncateMessage clips a (potentially huge) error string to MaxStepMessageBytes,
// appending an ellipsis marker so callers can tell it was cut off.
func truncateMessage(s string) string {
	if len(s) <= MaxStepMessageBytes {
		return s
	}
	return s[:MaxStepMessageBytes] + "...(truncated)"
}
