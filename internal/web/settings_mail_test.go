package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/mailstatus"
)

// Settings says whether mail is getting out: when it last did, what the last
// failure said, and how many failed today. Without it a refusing relay showed
// only in a log that is gone with the pod.
func TestSettingsShowsWhetherMailIsGettingOut(t *testing.T) {
	now := time.Now()
	h := testServer(t, Options{MailStatus: func(context.Context) (mailstatus.Status, error) {
		return mailstatus.Status{
			LastSent:    now.Add(-2 * time.Hour),
			LastFailure: now.Add(-10 * time.Minute),
			LastError:   "notify: cloudflare rejected the message: sender not verified (10001)",
			Failures24h: 3,
		}, nil
	}})
	body := get(t, h, "/settings").Body.String()
	for _, want := range []string{
		`id="mail-status"`, "Last sent", "Last failure",
		"notify: cloudflare rejected the message: sender not verified (10001)",
		"Failures, 24h", "<dd>3</dd>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings lacks %q", want)
		}
	}

	if strings.Contains(get(t, testServer(t, Options{}), "/settings").Body.String(), `id="mail-status"`) {
		t.Error("an install with no mail record shows a Mail panel")
	}
}
