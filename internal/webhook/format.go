package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Payload is the JSON event a webhook is sent, unless its URL is Discord's or
// Slack's. Built from the event alone, so there is nothing in it a receiver
// should not see: names, ids, a status, and why a deploy failed — never a
// variable, and never a header.
type Payload struct {
	Type      string    `json:"type"`
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`

	Project *PayloadRef `json:"project,omitempty"`
	App     *PayloadRef `json:"app,omitempty"`

	Deployment *PayloadDeployment `json:"deployment,omitempty"`
	Message    string             `json:"message,omitempty"`
}

// PayloadRef names a project or an app, with a link to it on the dashboard.
type PayloadRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

// PayloadDeployment describes the deploy an event is about.
type PayloadDeployment struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Trigger string `json:"trigger,omitempty"`
	Error   string `json:"error,omitempty"`
}

// kindOf is how a URL's payload is shaped. Discord's and Slack's incoming
// webhooks refuse anything but their own message format, so sending them the
// JSON event would fail every time with nothing to show for it.
func kindOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "json"
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case (host == "discord.com" || host == "discordapp.com" ||
		strings.HasSuffix(host, ".discord.com")) && strings.HasPrefix(u.Path, "/api/webhooks/"):
		return "discord"
	case host == "hooks.slack.com":
		return "slack"
	}
	return "json"
}

// title is one line saying what happened, for a chat message.
func title(p Payload) string {
	app := ""
	if p.App != nil {
		app = p.App.Name
	}
	switch p.Type {
	case "deploy.succeeded":
		return "Deploy of " + app + " succeeded"
	case "deploy.failed":
		return "Deploy of " + app + " failed"
	case "deploy.crashed":
		return "Deploy of " + app + " crashed"
	case "deploy.removed":
		return "Deployment of " + app + " removed"
	case "app.crashed":
		return app + " is crashing"
	case EventTest:
		return "Test webhook"
	}
	return p.Type
}

// colour is the Discord embed's stripe: green for good news, red for bad,
// grey for neither.
func colour(event string) int {
	switch event {
	case "deploy.succeeded":
		return 0x22c55e
	case "deploy.failed", "deploy.crashed", "app.crashed":
		return 0xef4444
	}
	return 0x64748b
}

// render is the body a webhook is sent for one payload.
func render(kind, brand string, p Payload) ([]byte, error) {
	line := title(p)
	detail := p.Message
	if p.Deployment != nil && p.Deployment.Error != "" {
		detail = p.Deployment.Error
	}
	project := ""
	if p.Project != nil {
		project = p.Project.Name
	}
	link := ""
	if p.App != nil {
		link = p.App.URL
	}
	switch kind {
	case "discord":
		embed := map[string]any{
			"title": line, "color": colour(p.Type),
			"timestamp": p.Timestamp.UTC().Format(time.RFC3339),
		}
		if detail != "" {
			embed["description"] = clipText(detail, 2000)
		}
		if link != "" {
			embed["url"] = link
		}
		if project != "" {
			embed["footer"] = map[string]string{"text": project}
		}
		return json.Marshal(map[string]any{"username": brand, "embeds": []any{embed}})
	case "slack":
		text := "*" + slackEscape(line) + "*"
		if link != "" {
			text = "<" + link + "|" + slackEscape(line) + ">"
		}
		if project != "" {
			text += " in " + slackEscape(project)
		}
		if detail != "" {
			text += "\n" + slackEscape(clipText(detail, 2000))
		}
		return json.Marshal(map[string]any{"text": text})
	}
	return json.Marshal(p)
}

// slackEscape keeps a name from being read as Slack's own markup.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// sign is the Standard Webhooks signature over one delivery: HMAC-SHA256,
// keyed by the secret's base64 part, over "id.timestamp.body", sent as
// "v1,<base64>". A receiver recomputes it and compares, and checks the
// timestamp is recent so an old delivery cannot be replayed.
func sign(secret, id string, ts time.Time, body []byte) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return "", fmt.Errorf("webhook: signing secret is not base64: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + strconv.FormatInt(ts.Unix(), 10) + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Verify checks a Standard Webhooks signature header against a body, the way
// a receiver would: any v1 signature in the space-separated list, compared in
// constant time. Here for the tests, and for anybody writing a receiver in Go
// who wants to see it done.
func Verify(secret, id, timestamp string, body []byte, header string) bool {
	sec, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	want, err := sign(secret, id, time.Unix(sec, 0), body)
	if err != nil {
		return false
	}
	for _, sig := range strings.Fields(header) {
		if hmac.Equal([]byte(sig), []byte(want)) {
			return true
		}
	}
	return false
}
