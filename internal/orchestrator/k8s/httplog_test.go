package k8s

import (
	"strings"
	"testing"
	"time"
)

// The pod log API serves only the controller's current file, which the
// kubelet rotates every 10 MiB, so what was read says where it begins: the
// first access line in it, whoever that request was for (QA OBSERVE-1).
func TestTheControllersLogSaysWhereItBegins(t *testing.T) {
	log := strings.Join([]string{
		`time="2026-10-30T19:40:15Z" level=info msg="Configuration loaded"`,
		`{"StartUTC":"2026-10-30T19:40:16.5Z","RequestHost":"other.up.example.test","RequestMethod":"GET","RequestPath":"/","DownstreamStatus":200}`,
		`{"StartUTC":"2026-10-30T19:40:20Z","RequestHost":"Web.up.example.test","RequestMethod":"GET","RequestPath":"/slow","DownstreamStatus":200}`,
		`{"StartUTC":"2026-10-30T19:40:18Z","RequestHost":"other.up.example.test","RequestMethod":"GET","RequestPath":"/","DownstreamStatus":200}`,
	}, "\n")
	got := scanAccessLog(strings.NewReader(log), map[string]bool{"web.up.example.test": true})
	if !got.saw {
		t.Error("no access line was seen")
	}
	if len(got.lines) != 1 || got.lines[0].Path != "/slow" {
		t.Errorf("lines = %+v, want only the app's", got.lines)
	}
	if want := time.Date(2026, 10, 30, 19, 40, 16, 5e8, time.UTC); !got.from.Equal(want) {
		t.Errorf("from = %v, want %v, the first request in the log", got.from, want)
	}
}
