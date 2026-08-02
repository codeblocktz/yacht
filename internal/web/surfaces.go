package web

import (
	"context"
	"net/http"
)

// Surfaces is which optional pages this install actually has.
//
// The sidebar is built from a path and a context, with no access to the
// server's dependencies, so without this it offers every entry regardless of
// whether the route behind it was mounted. A link to a 404 is a worse answer
// than a missing link: it says the feature exists and is broken.
//
// This is the same failure the Networking panel had, and the DNS page had it
// silently from the day it was written — a page with no way in.
type Surfaces struct {
	DNS      bool
	Registry bool
}

// surfacesKey addresses them on the request. An unexported struct type, so
// nothing outside this package can plant a set of surfaces on a context and
// have the chrome offer pages that are not mounted.
type surfacesKey struct{}

// SurfacesFromContext returns the optional pages available to this request.
func SurfacesFromContext(ctx context.Context) Surfaces {
	s, _ := ctx.Value(surfacesKey{}).(Surfaces)
	return s
}

// withSurfaces makes them available to whatever renders the chrome.
func (s *Server) withSurfaces(next http.Handler) http.Handler {
	// Computed once at wiring time rather than per request: these depend on
	// which dependencies the server was built with, and those do not change
	// while it is running.
	available := Surfaces{
		DNS:      s.joiner != nil,
		Registry: s.registries != nil,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), surfacesKey{}, available)))
	})
}
