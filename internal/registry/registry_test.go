package registry

import (
	"encoding/json"
	"strings"
	"testing"
)

// A host is where an image name starts, and nothing else.
//
// The reason these are refused at the form rather than tolerated: a reference
// like "https://ghcr.io/org/app:v1" is not wrong until something tries to pull
// it, which is inside a deploy, hours after the person who typed it left.
func TestAHostIsRejectedWhenItIsNotOne(t *testing.T) {
	for _, tc := range []struct{ host, why string }{
		{"", "empty"},
		{"https://ghcr.io", "carries a scheme"},
		{"ghcr.io/myorg", "carries a path"},
		{"GHCR.IO", "not lowercased by the caller"},
		{"ghcr.io:notaport", "port is not a number"},
		{"-ghcr.io", "starts with a dash"},
		{"ghcr..io", "empty label"},
	} {
		if err := ValidateHost(tc.host); err == nil {
			t.Errorf("%q was accepted (%s)", tc.host, tc.why)
		}
	}

	for _, host := range []string{
		"ghcr.io", "docker.io", "registry.example.com", "registry.example.com:5000",
		"localhost:5000",
	} {
		if err := ValidateHost(host); err != nil {
			t.Errorf("%q was refused: %v", host, err)
		}
	}
}

// The scheme and path mistakes get their own message.
//
// "not a registry host" is true and useless for the two things people actually
// type. Both are near-misses from copying a browser URL, and the fix is
// different in each case.
func TestTheCommonHostMistakesAreNamed(t *testing.T) {
	err := ValidateHost("https://ghcr.io")
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("a scheme is not called out: %v", err)
	}
	err = ValidateHost("ghcr.io/myorg")
	if err == nil || !strings.Contains(err.Error(), "repository") {
		t.Errorf("a path does not point at the repository field: %v", err)
	}
}

func TestARepositoryPathIsChecked(t *testing.T) {
	for _, repo := range []string{"", "myorg", "myorg/sub", "my-org", "my.org_1/two"} {
		if err := ValidateRepository(repo); err != nil {
			t.Errorf("%q was refused: %v", repo, err)
		}
	}
	for _, repo := range []string{"MyOrg", "my org", "/leading", "trailing/", "a//b", "a$b"} {
		if err := ValidateRepository(repo); err == nil {
			t.Errorf("%q was accepted", repo)
		}
	}
}

// An app's image path is derived, not chosen.
//
// One registry account holds every team's images. If the path were somebody's
// input they could pick another team's, and pushing over a tag the cluster
// already pulls is a way to have it run your image under their name.
func TestTwoTeamsWithTheSameAppNameDoNotShareAPath(t *testing.T) {
	s := Settings{Host: "ghcr.io", Repository: "acme"}

	one := s.ImageRef("owner-alpha", "web", "abc1234")
	two := s.ImageRef("owner-beta", "web", "abc1234")

	if one == two {
		t.Fatalf("two teams' \"web\" push to the same image: %s", one)
	}
	if !strings.HasPrefix(one, "ghcr.io/acme/") {
		t.Errorf("the configured host and repository are not used: %s", one)
	}
	if !strings.HasSuffix(one, ":abc1234") {
		t.Errorf("the revision is not the tag: %s", one)
	}
}

// A registry that hands out the root still works.
func TestAnEmptyRepositoryLeavesNoDoubleSlash(t *testing.T) {
	got := Settings{Host: "registry.example.com:5000"}.ImageRef("owner-1", "web", "rev")
	if strings.Contains(got, "//") {
		t.Errorf("empty repository produced %q", got)
	}
}

// Anything a registry path cannot hold is replaced rather than refused.
//
// These are identifiers this system already holds by the time they reach here,
// so a rejection would be a failure with nothing a person could do about it.
func TestAnIdentifierIsReducedToWhatARegistryAccepts(t *testing.T) {
	got := Settings{Host: "ghcr.io"}.ImageRef("Owner ID/../x", "My App", "v1.0 beta")
	if strings.ContainsAny(strings.TrimPrefix(got, "ghcr.io/"), " /") {
		t.Errorf("a path separator or space survived: %q", got)
	}
	if strings.Contains(got, "..") {
		t.Errorf("a traversal survived: %q", got)
	}
}

// The pull secret is the shape Kubernetes reads.
func TestTheDockerConfigCarriesTheCredential(t *testing.T) {
	b, err := Credentials{Host: "ghcr.io", Username: "bot", Password: "hunter2"}.
		DockerConfigJSON()
	if err != nil {
		t.Fatalf("DockerConfigJSON: %v", err)
	}

	var cfg struct {
		Auths map[string]struct {
			Username, Password, Auth string
		} `json:"auths"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	entry, ok := cfg.Auths["ghcr.io"]
	if !ok {
		t.Fatalf("no entry for the host: %s", b)
	}
	if entry.Username != "bot" || entry.Password != "hunter2" {
		t.Errorf("credential not carried: %+v", entry)
	}
	// The auth field is what most registries actually read; a config with only
	// username and password authenticates against some and not others.
	if entry.Auth == "" {
		t.Error("no base64 auth field")
	}
}

// Settings has no password field.
//
// Every page takes this type, so the absence is what makes printing the
// credential impossible rather than merely unlikely. A field added later
// without thinking would fail this.
func TestSettingsCannotCarryAPassword(t *testing.T) {
	b, err := json.Marshal(Settings{
		Host: "ghcr.io", Repository: "acme", Username: "bot",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(b)), "password") {
		t.Errorf("Settings has a password field: %s", b)
	}
}
