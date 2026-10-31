// Package mailstatus keeps a record of whether the install's mail is getting
// out, for Settings to show.
//
// A send error used to reach only the log, and the dashboard's log is gone
// when its pod is replaced: a relay that began refusing mail stayed invisible
// until somebody said their sign-in link never came. Recorded in Postgres it
// survives a restart, and Settings can say when mail last went out and what
// the last failure said.
package mailstatus

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/codeblocktz/yacht/internal/notify"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Status is what Settings shows about mail.
type Status struct {
	// LastSent is when a message last went out, zero if none has in the
	// week the record keeps.
	LastSent time.Time
	// LastFailure and LastError are the most recent failure, zero and empty
	// if there has been none.
	LastFailure time.Time
	LastError   string
	// Failures24h is how many sends failed in the last day.
	Failures24h int
}

// keep is how long a delivery is remembered: long enough for "last sent" to
// mean something on a quiet install, short enough that the table stays small.
const keep = 7 * 24 * time.Hour

// maxError bounds the stored error. Relays can answer with a page of text.
const maxError = 1000

// Store records deliveries and reads them back.
type Store struct {
	q   *dbgen.Queries
	log *slog.Logger
	now func() time.Time
}

// New returns a Store on pool.
func New(pool *pgxpool.Pool, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{q: dbgen.New(pool), log: log, now: time.Now}
}

// Wrap returns a Mailer that sends through next and records each attempt.
// The send's own error is returned untouched; a failure to record it is only
// logged, since mail that went out must not be reported as mail that did not.
func (s *Store) Wrap(next notify.Mailer) notify.Mailer {
	return notify.MailerFunc(func(ctx context.Context, msg notify.Message) error {
		err := next.Send(ctx, msg)
		s.record(ctx, err)
		return err
	})
}

func (s *Store) record(ctx context.Context, sendErr error) {
	// Recorded even when the request that sent it has ended: a visitor who
	// closed the tab still had their mail sent, or not.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	now := s.now()
	var text *string
	if sendErr != nil {
		t := sendErr.Error()
		if len(t) > maxError {
			t = t[:maxError] + "…"
		}
		text = &t
	}
	if err := errors.Join(
		s.q.RecordMailDelivery(ctx, dbgen.RecordMailDeliveryParams{SentAt: now, Error: text}),
		s.q.PruneMailDeliveries(ctx, now.Add(-keep)),
	); err != nil {
		s.log.Error("record mail delivery", slog.String("error", err.Error()))
	}
}

// Status reads the record.
func (s *Store) Status(ctx context.Context) (Status, error) {
	var st Status
	sent, err := s.q.LastMailSent(ctx)
	if err != nil {
		return Status{}, err
	}
	if sent.After(time.Unix(0, 0)) {
		st.LastSent = sent
	}
	last, err := s.q.LastMailFailure(ctx)
	if err != nil {
		return Status{}, err
	}
	if len(last) > 0 {
		st.LastFailure, st.LastError = last[0].SentAt, last[0].Error
	}
	n, err := s.q.CountMailFailuresSince(ctx, s.now().Add(-24*time.Hour))
	if err != nil {
		return Status{}, err
	}
	st.Failures24h = int(n)
	return st, nil
}
