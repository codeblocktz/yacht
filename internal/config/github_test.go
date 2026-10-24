package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func githubKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func githubEnv(key string) map[string]string {
	return map[string]string{
		"YACHT_BASE_URL":                  "https://yacht.example.test",
		"YACHT_GITHUB_APP_ID":             "123456",
		"YACHT_GITHUB_APP_SLUG":           "example-deploys",
		"YACHT_GITHUB_APP_PRIVATE_KEY":    key,
		"YACHT_GITHUB_APP_WEBHOOK_SECRET": "0123456789abcdef0123",
		"YACHT_GITHUB_APP_CLIENT_ID":      "Iv23liexample",
		"YACHT_GITHUB_APP_CLIENT_SECRET":  "client-secret",
	}
}

func TestNoGitHubAppByDefault(t *testing.T) {
	setEnv(t, nil)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHubAppEnabled() {
		t.Fatal("a GitHub App is on with nothing set")
	}
}

// An env file holds one line per variable, so the key's newlines may be
// written \n; the key may also be a file.
func TestGitHubAppLoadsWithItsKeyInlineOrInAFile(t *testing.T) {
	key := githubKeyPEM(t)

	setEnv(t, githubEnv(strings.ReplaceAll(key, "\n", `\n`)))
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	app, err := c.GitHubApp()
	if err != nil || app.PrivateKey == nil || app.AppID != 123456 || app.Slug != "example-deploys" {
		t.Fatalf("GitHubApp = %+v, %v", app, err)
	}
	if !strings.Contains(c.String(), "github_app=example-deploys") ||
		strings.Contains(c.String(), "client-secret") {
		t.Errorf("String() = %q, want the App named and nothing secret", c.String())
	}

	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	env := githubEnv("")
	env["YACHT_GITHUB_APP_PRIVATE_KEY_FILE"] = path
	setEnv(t, env)
	if c, err = Load(); err != nil {
		t.Fatalf("Load with a key file: %v", err)
	}
	if _, err := c.GitHubApp(); err != nil {
		t.Fatalf("GitHubApp with a key file: %v", err)
	}
}

func TestPartialGitHubAppIsRefusedByName(t *testing.T) {
	key := githubKeyPEM(t)
	for _, tc := range []struct {
		name   string
		change map[string]string
		want   string
	}{
		{"no client secret", map[string]string{"YACHT_GITHUB_APP_CLIENT_SECRET": ""}, "YACHT_GITHUB_APP_CLIENT_SECRET"},
		{"no key", map[string]string{"YACHT_GITHUB_APP_PRIVATE_KEY": ""}, "YACHT_GITHUB_APP_PRIVATE_KEY"},
		{"bad key", map[string]string{"YACHT_GITHUB_APP_PRIVATE_KEY": "not a key"}, "YACHT_GITHUB_APP_PRIVATE_KEY"},
		{"both keys", map[string]string{"YACHT_GITHUB_APP_PRIVATE_KEY_FILE": "/etc/x.pem"}, "both set"},
		{"id not a number", map[string]string{"YACHT_GITHUB_APP_ID": "yacht"}, "YACHT_GITHUB_APP_ID"},
		{"slug not a slug", map[string]string{"YACHT_GITHUB_APP_SLUG": "My App"}, "YACHT_GITHUB_APP_SLUG"},
		{"short webhook secret", map[string]string{"YACHT_GITHUB_APP_WEBHOOK_SECRET": "short"}, "YACHT_GITHUB_APP_WEBHOOK_SECRET"},
		{"no base URL", map[string]string{"YACHT_BASE_URL": ""}, "YACHT_BASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := githubEnv(key)
			for k, v := range tc.change {
				env[k] = v
			}
			// t.Setenv with "" leaves it set to empty, which env() reads as
			// unset — the same as an operator leaving the line blank.
			setEnv(t, env)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %s", err, tc.want)
			}
		})
	}
}
