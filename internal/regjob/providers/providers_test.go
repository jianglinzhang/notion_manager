package providers

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// fakeProvider is a stub used only in registry tests. The real Microsoft
// integration is exercised in providers/microsoft.
type fakeProvider struct {
	id          string
	display     string
	hint        string
	concurrency int
	parseFn     func(string) ([]Credential, error)
	loginFn     func(context.Context, Credential, *Credential, LoginOptions) (*Session, error)
}

func (f *fakeProvider) ID() string                  { return f.id }
func (f *fakeProvider) Display() string             { return f.display }
func (f *fakeProvider) FormatHint() string          { return f.hint }
func (f *fakeProvider) RecommendedConcurrency() int { return f.concurrency }
func (f *fakeProvider) Parse(s string) ([]Credential, error) {
	if f.parseFn != nil {
		return f.parseFn(s)
	}
	return nil, nil
}
func (f *fakeProvider) Login(ctx context.Context, c Credential, b *Credential, opts LoginOptions) (*Session, error) {
	if f.loginFn != nil {
		return f.loginFn(ctx, c, b, opts)
	}
	return nil, errors.New("not implemented")
}

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	p := &fakeProvider{id: "microsoft", display: "Microsoft", hint: "h", concurrency: 1}
	r.Register(p)

	got, ok := r.Get("microsoft")
	if !ok || got.ID() != "microsoft" {
		t.Fatalf("Get(microsoft) = (%v,%v), want microsoft,true", got, ok)
	}

	// Case-insensitive lookup.
	if _, ok := r.Get("Microsoft"); !ok {
		t.Fatalf("Get is not case-insensitive on ID")
	}
	// Trim whitespace.
	if _, ok := r.Get("  microsoft  "); !ok {
		t.Fatalf("Get does not trim whitespace")
	}

	if _, ok := r.Get("google"); ok {
		t.Fatalf("Get(google) should be false on empty registry")
	}
}

func TestRegistryListPreservesOrder(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{id: "microsoft", display: "Microsoft"})
	r.Register(&fakeProvider{id: "google", display: "Google"})
	r.Register(&fakeProvider{id: "github", display: "GitHub"})

	ids := r.IDs()
	want := []string{"microsoft", "google", "github"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("IDs() = %v, want %v", ids, want)
	}

	infos := r.List()
	if len(infos) != 3 {
		t.Fatalf("List returned %d items", len(infos))
	}
	for i, info := range infos {
		if info.ID != want[i] {
			t.Fatalf("List[%d].ID = %q, want %q", i, info.ID, want[i])
		}
		if !info.Enabled {
			t.Fatalf("List[%d].Enabled false (registered providers should be enabled)", i)
		}
	}
}

func TestRegistryDuplicatePanics(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{id: "microsoft"})
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on duplicate Register")
		}
	}()
	r.Register(&fakeProvider{id: "microsoft"})
}

func TestRegistryEmptyIDPanics(t *testing.T) {
	r := NewRegistry()
	defer func() {
		if recover() == nil {
			t.Fatalf("expected panic on empty ID")
		}
	}()
	r.Register(&fakeProvider{id: ""})
}

func TestRegistryNilRegisterIsNoop(t *testing.T) {
	r := NewRegistry()
	r.Register(nil)
	if len(r.IDs()) != 0 {
		t.Fatalf("nil Register should not add anything")
	}
}
