package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// A repository's tags come back whole: the challenge answered once, and every
// page the registry names read with the token the first one was given.
func TestARepositorysTagsAreListedAcrossPages(t *testing.T) {
	var host string
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if got := r.URL.Query().Get("scope"); got != "repository:library/postgres:pull" {
				t.Errorf("token asked for scope %q", got)
			}
			fmt.Fprint(w, `{"token":"t0k"}`)
		case r.Header.Get("Authorization") != "Bearer t0k":
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm="http://%s/token",service="registry",scope="repository:library/postgres:pull"`, host))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path != "/v2/library/postgres/tags/list":
			t.Errorf("asked for %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Query().Get("last") == "":
			w.Header().Set("Link", `</v2/library/postgres/tags/list?last=16-alpine&n=2>; rel="next"`)
			fmt.Fprint(w, `{"name":"library/postgres","tags":["16","16-alpine"]}`)
		default:
			fmt.Fprint(w, `{"name":"library/postgres","tags":["16.15-alpine","17"]}`)
		}
	})
	parsed, err := parseReference(host + "/library/postgres:16-alpine")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.tags(context.Background(), parsed)
	if err != nil {
		t.Fatalf("tags: %v", err)
	}
	if want := []string{"16", "16-alpine", "16.15-alpine", "17"}; !slices.Equal(got, want) {
		t.Errorf("tags are %q, want %q", got, want)
	}
}

// A next page on another host is refused rather than followed: it would be
// a way to aim the token at it.
func TestATagListsNextPageStaysOnItsRegistry(t *testing.T) {
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<http://elsewhere.example/v2/x/tags/list?last=a>; rel="next"`)
		fmt.Fprint(w, `{"tags":["a"]}`)
	})
	parsed, _ := parseReference(host + "/acme/web")
	if _, err := client.tags(context.Background(), parsed); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("an off-registry next page gave %v, want ErrUnreachable", err)
	}
}

func TestAMissingRepositoryHasNoTags(t *testing.T) {
	host, client := fakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	parsed, _ := parseReference(host + "/acme/web")
	if _, err := client.tags(context.Background(), parsed); !errors.Is(err, ErrManifestNotFound) {
		t.Fatalf("a 404 gave %v, want ErrManifestNotFound", err)
	}
}
