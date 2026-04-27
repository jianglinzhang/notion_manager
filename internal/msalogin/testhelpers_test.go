package msalogin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// recordingTokenServer is a tiny httptest helper that captures the
// form body posted to it (so tests can assert on scope / grant_type
// etc.) and replies with a configurable status + body.
//
// Default reply: 200 OK with the body passed to the constructor.
// Override post-construction via setStatus / setBody before the
// request lands.
type recordingTokenServer struct {
	t *testing.T
	*httptest.Server

	mu     sync.Mutex
	status int
	body   string

	lastForm url.Values
}

func newRecordingTokenServer(t *testing.T, defaultBody string) *recordingTokenServer {
	r := &recordingTokenServer{
		t:      t,
		status: 200,
		body:   defaultBody,
	}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		form, _ := url.ParseQuery(string(raw))
		r.mu.Lock()
		r.lastForm = form
		status, body := r.status, r.body
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return r
}

func (r *recordingTokenServer) setStatus(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = code
}

func (r *recordingTokenServer) setBody(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.body = body
}
