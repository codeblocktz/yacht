package mailstatus

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/codeblocktz/yacht/internal/notify"
	"github.com/codeblocktz/yacht/internal/store"
)

// A record that outlives the process: what one Store wrote, a new one reads
// back — last sent, last failure with its words, and the failures of the day.
func TestStatusSurvivesARestart(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run the mail record against Postgres")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(ctx, dsn, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The table is the install's, shared with whatever else runs against this
	// database, so this test lives far in the future where nothing else writes.
	base := time.Date(2100, 1, 1, 12, 0, 0, 0, time.UTC)
	purge := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM mail_deliveries WHERE sent_at >= $1`, base.AddDate(0, 0, -30))
	}
	purge()
	t.Cleanup(purge)

	at := base
	s := New(pool, log)
	s.now = func() time.Time { return at }

	refused := errors.New("notify: cloudflare rejected the message: sender not verified (10001)")
	var fail error
	m := s.Wrap(notify.MailerFunc(func(context.Context, notify.Message) error { return fail }))
	send := func(when time.Time, err error) {
		t.Helper()
		at, fail = when, err
		if got := m.Send(ctx, notify.Message{To: "a@example.test", Subject: "s", TextBody: "b"}); !errors.Is(got, err) {
			t.Fatalf("Send = %v, want the transport's own %v", got, err)
		}
	}
	send(base.Add(-30*time.Hour), refused) // outside the day
	send(base.Add(-3*time.Hour), nil)
	send(base.Add(-2*time.Hour), refused)
	send(base.Add(-1*time.Hour), refused)

	restarted := New(pool, log)
	restarted.now = func() time.Time { return base }
	st, err := restarted.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.LastSent.Equal(base.Add(-3 * time.Hour)) {
		t.Errorf("LastSent = %v, want %v", st.LastSent, base.Add(-3*time.Hour))
	}
	if !st.LastFailure.Equal(base.Add(-1*time.Hour)) || st.LastError != refused.Error() {
		t.Errorf("last failure = %v %q", st.LastFailure, st.LastError)
	}
	if st.Failures24h != 2 {
		t.Errorf("Failures24h = %d, want 2", st.Failures24h)
	}
}
