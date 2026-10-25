package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Verify checks a delivery's X-Hub-Signature-256 against the App's webhook
// secret: an HMAC-SHA256 of the body, compared in constant time. Nothing in
// the body is read before this says yes.
func (a *App) Verify(signature string, body []byte) bool {
	sig, ok := strings.CutPrefix(strings.TrimSpace(signature), "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.WebhookSecret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// The parts of the App's events acted on.

// PushEvent is a push to a repository an installation reaches.
type PushEvent struct {
	Ref          string          `json:"ref"`
	After        string          `json:"after"`
	Deleted      bool            `json:"deleted"`
	Repository   EventRepository `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// EventRepository is a repository as an event names it.
type EventRepository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
}

// InstallationEvent is the App installed, uninstalled, suspended or
// unsuspended on an account.
type InstallationEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID      int64   `json:"id"`
		Account Account `json:"account"`
	} `json:"installation"`
}

// InstallationRepositoriesEvent is repositories added to or taken from an
// installation.
type InstallationRepositoriesEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Added   []EventRepository `json:"repositories_added"`
	Removed []EventRepository `json:"repositories_removed"`
}

// PullRequestEvent is a pull request opened, updated, or closed: what a
// project's PR environments are made and taken down by.
type PullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Title string `json:"title"`
		Head  struct {
			Ref  string          `json:"ref"`
			SHA  string          `json:"sha"`
			Repo EventRepository `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref  string          `json:"ref"`
			Repo EventRepository `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository   EventRepository `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}
