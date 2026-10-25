package apitoken

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/codeblocktz/yacht/internal/account"
	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Signing a CLI in through a browser — the OAuth device flow, in the shape
// GitHub's and Railway's CLIs use. The CLI asks for a login and is given two
// codes: a device code it keeps and polls with, and a short user code it
// prints and opens the browser at. A person signed in to the dashboard sees
// the same user code there, checks it matches their terminal, and approves.
// The CLI's next poll is handed a new token, once.

// LoginTTL is how long a login waits to be approved.
const LoginTTL = 10 * time.Minute

var (
	// ErrLoginPending is a login nobody has approved yet: poll again.
	ErrLoginPending = errors.New("apitoken: waiting for the login to be approved")
	// ErrLoginDenied is a login somebody refused.
	ErrLoginDenied = errors.New("apitoken: the login was refused")
	// ErrLoginExpired is a login that waited too long, was already used, or
	// never existed — the CLI's answer to all three is to start again.
	ErrLoginExpired = errors.New("apitoken: the login expired — start again")
)

// Login is a pending CLI login as the approval page shows it.
type Login struct {
	UserCode   string
	ClientName string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Approved   bool
}

// StartLogin opens a login for a CLI, returning the device code it polls
// with and the user code it shows.
func (s *Service) StartLogin(ctx context.Context, clientName string) (device, user string, expires time.Time, err error) {
	clientName = strings.TrimSpace(clientName)
	if utf8.RuneCountInString(clientName) > 80 || strings.ContainsAny(clientName, "\r\n") {
		clientName = ""
	}
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", time.Time{}, fmt.Errorf("apitoken: generate device code: %w", err)
	}
	device = base64.RawURLEncoding.EncodeToString(b)
	expires = time.Now().Add(LoginTTL)
	// A collision between two pending user codes is refused by the unique
	// index; trying again with another is all it takes.
	for range 5 {
		user, err = userCode()
		if err != nil {
			return "", "", time.Time{}, err
		}
		_, err = s.q.CreateCLILogin(ctx, dbgen.CreateCLILoginParams{
			DeviceHash: hash(device), UserCode: user, ClientName: clientName, ExpiresAt: expires,
		})
		if err == nil {
			// Expired logins are swept here rather than by a loop of their
			// own: one starting is the only time there is anything to sweep.
			_ = s.q.DeleteExpiredCLILogins(ctx)
			return device, user, expires, nil
		}
	}
	return "", "", time.Time{}, fmt.Errorf("apitoken: start login: %w", err)
}

// userCodeAlphabet leaves out what reads as something else — 0 and O, 1 and
// I and L — because the person is comparing two codes by eye.
const userCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// userCode is eight characters as XXXX-XXXX: about forty bits, against a
// login that lasts ten minutes and an approval page behind a session.
func userCode() (string, error) {
	var b strings.Builder
	for i := range 8 {
		if i == 4 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(userCodeAlphabet))))
		if err != nil {
			return "", fmt.Errorf("apitoken: generate user code: %w", err)
		}
		b.WriteByte(userCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// NormaliseUserCode is what somebody typed, as the code is stored.
func NormaliseUserCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.ReplaceAll(code, " ", "")
	if len(code) == 8 && !strings.Contains(code, "-") {
		code = code[:4] + "-" + code[4:]
	}
	return code
}

// LoginByCode is a pending login, for the approval page to describe.
func (s *Service) LoginByCode(ctx context.Context, code string) (Login, error) {
	row, err := s.q.GetCLILoginByUserCode(ctx, NormaliseUserCode(code))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Login{}, ErrLoginExpired
		}
		return Login{}, fmt.Errorf("apitoken: read login: %w", err)
	}
	return Login{
		UserCode: row.UserCode, ClientName: row.ClientName,
		CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt, Approved: row.ApprovedAt.Valid,
	}, nil
}

// ApproveLogin approves a login for a person in a team, with the role the
// token will carry. The role is checked against theirs now, and again when
// the token is minted, because the poll may come minutes later.
func (s *Service) ApproveLogin(
	ctx context.Context, code string, actor uuid.UUID, ownerID string, role account.Role,
) error {
	if role != account.RoleMember && role != account.RoleAdmin {
		return ErrRole
	}
	got, err := s.q.GetMembership(ctx, dbgen.GetMembershipParams{UserID: actor, OwnerID: ownerID})
	if err != nil {
		return ErrForbidden
	}
	if !account.Role(got.Role).AtLeast(role) {
		return ErrRole
	}
	r := string(role)
	n, err := s.q.ApproveCLILogin(ctx, dbgen.ApproveCLILoginParams{
		ApprovedBy: pgtype.UUID{Bytes: actor, Valid: true}, OwnerID: &ownerID, Role: &r,
		UserCode: NormaliseUserCode(code),
	})
	if err != nil {
		return fmt.Errorf("apitoken: approve login: %w", err)
	}
	if n != 1 {
		return ErrLoginExpired
	}
	return nil
}

// DenyLogin refuses one, so the CLI stops waiting and says so.
func (s *Service) DenyLogin(ctx context.Context, code string) error {
	n, err := s.q.DenyCLILogin(ctx, NormaliseUserCode(code))
	if err != nil {
		return fmt.Errorf("apitoken: deny login: %w", err)
	}
	if n != 1 {
		return ErrLoginExpired
	}
	return nil
}

// PollLogin is the CLI asking whether its login was approved. When it was,
// the token is made now and returned, once: the login is spent in the same
// transaction, under a lock, so a second poll racing the first gets
// ErrLoginExpired rather than a second token.
func (s *Service) PollLogin(ctx context.Context, device string) (string, Token, error) {
	if device == "" {
		return "", Token{}, ErrLoginExpired
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", Token{}, fmt.Errorf("apitoken: begin poll: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	row, err := q.LockCLILoginByDevice(ctx, hash(device))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", Token{}, ErrLoginExpired
		}
		return "", Token{}, fmt.Errorf("apitoken: read login: %w", err)
	}
	switch {
	case row.Denied:
		return "", Token{}, ErrLoginDenied
	case row.ConsumedAt.Valid || time.Now().After(row.ExpiresAt):
		return "", Token{}, ErrLoginExpired
	case !row.ApprovedAt.Valid || row.OwnerID == nil || row.Role == nil:
		return "", Token{}, ErrLoginPending
	}
	name := "CLI"
	if row.ClientName != "" {
		name = "CLI on " + row.ClientName
	}
	raw, tok, err := s.create(ctx, q, CreateInput{
		OwnerID: *row.OwnerID, Name: truncate(name, 60), Role: account.Role(*row.Role),
		Origin: "cli", Actor: uuid.UUID(row.ApprovedBy.Bytes),
	})
	if err != nil {
		return "", Token{}, err
	}
	if err := q.ConsumeCLILogin(ctx, row.ID); err != nil {
		return "", Token{}, fmt.Errorf("apitoken: spend login: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", Token{}, fmt.Errorf("apitoken: commit poll: %w", err)
	}
	return raw, tok, nil
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
