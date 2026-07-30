package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSingleOwnerAlwaysResolves(t *testing.T) {
	p := NewSingleOwner(Owner{ID: "owner-1", DisplayName: "Eric"})
	got, err := p.Resolve(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ID != "owner-1" {
		t.Errorf("id = %q, want owner-1", got.ID)
	}
}

func TestSingleOwnerFallsBackToLocalDefault(t *testing.T) {
	p := NewSingleOwner(Owner{})
	got, _ := p.Resolve(context.Background(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got.ID != DefaultOwnerID {
		t.Errorf("id = %q, want %q", got.ID, DefaultOwnerID)
	}
	if !got.Valid() {
		t.Error("fallback owner must be valid")
	}
}

// An empty token would authenticate every request, so construction must fail
// rather than produce a provider that silently allows everything.
func TestStaticTokenRejectsEmptyToken(t *testing.T) {
	if _, err := NewStaticToken(Owner{ID: "owner-1"}, ""); err == nil {
		t.Error("expected an error for an empty token")
	}
}

func TestStaticToken(t *testing.T) {
	p, err := NewStaticToken(Owner{ID: "owner-1"}, "s3cret")
	if err != nil {
		t.Fatalf("NewStaticToken: %v", err)
	}

	cases := []struct {
		name    string
		header  string
		wantErr bool
	}{
		{"valid bearer", "Bearer s3cret", false},
		{"case-insensitive scheme", "bearer s3cret", false},
		{"wrong token", "Bearer nope", true},
		{"missing header", "", true},
		{"no scheme", "s3cret", true},
		{"wrong scheme", "Basic s3cret", true},
		{"empty value", "Bearer ", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			owner, err := p.Resolve(context.Background(), r)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if owner.Valid() {
					t.Error("must not return a valid owner alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if owner.ID != "owner-1" {
				t.Errorf("id = %q, want owner-1", owner.ID)
			}
		})
	}
}

func TestContextRoundTrip(t *testing.T) {
	ctx := NewContext(context.Background(), Owner{ID: "owner-1"})
	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: not found")
	}
	if got.ID != "owner-1" {
		t.Errorf("id = %q, want owner-1", got.ID)
	}
}

func TestFromContextRejectsEmptyOwner(t *testing.T) {
	// An owner with no ID must not read as present, or a downstream query
	// scopes to the empty string and matches nothing — or worse, everything.
	ctx := NewContext(context.Background(), Owner{})
	if _, ok := FromContext(ctx); ok {
		t.Error("an owner with no ID must not count as present")
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Error("bare context must not yield an owner")
	}
}

func TestMiddlewareInjectsOwner(t *testing.T) {
	var seen Owner
	h := Middleware(NewSingleOwner(Owner{ID: "owner-1"}))(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			seen = MustFromContext(r.Context())
		}),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if seen.ID != "owner-1" {
		t.Errorf("handler saw id %q, want owner-1", seen.ID)
	}
}

func TestMiddlewareRejectsUnresolved(t *testing.T) {
	failing := ProviderFunc(func(context.Context, *http.Request) (Owner, error) {
		return Owner{}, ErrUnauthenticated
	})

	var called bool
	h := Middleware(failing)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("handler must not run when identity cannot be resolved")
	}
}

// A provider that returns no error but an empty owner is a bug in the
// provider; the middleware must still refuse rather than pass an unusable
// owner downstream.
func TestMiddlewareRejectsInvalidOwnerWithoutError(t *testing.T) {
	sloppy := ProviderFunc(func(context.Context, *http.Request) (Owner, error) {
		return Owner{}, nil
	})

	var called bool
	h := Middleware(sloppy)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("handler must not run for an invalid owner")
	}
}

func TestMustFromContextPanicsWhenUnmounted(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected a panic when no owner is in context")
		}
	}()
	MustFromContext(context.Background())
}

// The seam must accept an implementation the engine knows nothing about —
// this stands in for the multi-tenant provider a wrapping layer supplies.
func TestProviderIsReplaceable(t *testing.T) {
	custom := ProviderFunc(func(_ context.Context, r *http.Request) (Owner, error) {
		id := r.Header.Get("X-Owner")
		if id == "" {
			return Owner{}, ErrUnauthenticated
		}
		return Owner{ID: id, DisplayName: "resolved elsewhere"}, nil
	})

	var seen Owner
	h := Middleware(custom)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = MustFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Owner", "acct-42")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if seen.ID != "acct-42" {
		t.Errorf("id = %q, want acct-42", seen.ID)
	}
	if !errors.Is(ErrUnauthenticated, ErrUnauthenticated) {
		t.Error("sanity: sentinel error comparison")
	}
}
