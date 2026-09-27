package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// Deploy on push.
//
// A Git host calls one URL per app when a branch moves; if the branch is the
// one the app builds from, that is a redeploy. Everything a person's redeploy
// goes through — admission, the fenced build, the rollout check — a push goes
// through too. The only new thing here is deciding whether a request is a push
// this app should act on.

// Hook refusals the web layer turns into status codes.
var (
	// ErrHookUnauthorized covers an unknown hook and a signature that does not
	// match alike. Telling the two apart would tell a caller which app ids
	// have a hook without their knowing its secret.
	ErrHookUnauthorized = errors.New("app: this push is not signed with the app's webhook secret")

	// ErrNotGitApp means the app has no repository for a push to be about.
	ErrNotGitApp = errors.New("app: only an app built from a repository can deploy on push")
)

// PushTriggerPrefix starts the trigger, and revision, of a push deployment.
const PushTriggerPrefix = "push:"

// Hook is what the settings page shows about an app's webhook. The secret is
// not in it: it is shown once, when it is made, and never read back to a page.
type Hook struct {
	Enabled   bool
	CreatedAt time.Time

	// LastDelivery is what the last push did, in a sentence, and when.
	LastDelivery   string
	LastDeliveryAt time.Time

	// Pending is a push waiting for the deploy in flight to end.
	Pending bool
}

// Push is one delivery, as the web layer received it.
type Push struct {
	// Event is the host's own name for it: X-GitHub-Event, X-Gitea-Event or
	// X-Gitlab-Event. Empty when none was sent.
	Event string

	// Signature is X-Hub-Signature-256 (GitHub, Gitea, Forgejo, Bitbucket
	// Server) or X-Gitea-Signature: an HMAC-SHA256 of the body.
	Signature string

	// Token is X-Gitlab-Token, which GitLab sends as the secret itself.
	Token string

	Body []byte
}

// GitHook reports an app's webhook.
func (s *Service) GitHook(ctx context.Context, ownerID, name string) (Hook, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return Hook{}, err
	}
	row, err := s.q.GetAppHook(ctx, dbgen.GetAppHookParams{OwnerID: ownerID, AppID: a.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Hook{}, nil
	}
	if err != nil {
		return Hook{}, fmt.Errorf("app: read webhook: %w", err)
	}
	h := Hook{
		Enabled: true, CreatedAt: row.CreatedAt,
		LastDelivery: row.LastDelivery, Pending: row.PendingAt.Valid,
	}
	if row.LastDeliveryAt.Valid {
		h.LastDeliveryAt = row.LastDeliveryAt.Time
	}
	return h, nil
}

// EnableGitHook gives an app a webhook, or a new secret for the one it has,
// and returns the secret. It is the only time the secret is available in the
// clear: the page shows it once, to be pasted into the Git host.
func (s *Service) EnableGitHook(ctx context.Context, ownerID, name string) (string, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return "", err
	}
	if a.Source != SourceGit {
		return "", ErrNotGitApp
	}
	if !s.keeper.Configured() {
		return "", ErrNoSecretKey
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("app: generate webhook secret: %w", err)
	}
	secret := hex.EncodeToString(raw)
	sealed, err := s.keeper.Seal(secret)
	if err != nil {
		return "", fmt.Errorf("app: seal webhook secret: %w", err)
	}
	if _, err := s.q.UpsertAppHook(ctx, dbgen.UpsertAppHookParams{
		AppID: a.ID, OwnerID: ownerID, Secret: sealed,
	}); err != nil {
		return "", fmt.Errorf("app: store webhook: %w", err)
	}
	s.log.Info("webhook secret issued", slog.String("app", name))
	return secret, nil
}

// DisableGitHook removes an app's webhook. A Git host still calling it is
// refused from then on, the same as one that never had the secret.
func (s *Service) DisableGitHook(ctx context.Context, ownerID, name string) error {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return err
	}
	if _, err := s.q.DeleteAppHook(ctx, dbgen.DeleteAppHookParams{OwnerID: ownerID, AppID: a.ID}); err != nil {
		return fmt.Errorf("app: remove webhook: %w", err)
	}
	s.log.Info("webhook removed", slog.String("app", name))
	return nil
}

// pushPayload is the part of a push event every host agrees on. GitHub, Gitea,
// Forgejo and GitLab all send ref and after; GitHub and Gitea add deleted.
type pushPayload struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
}

// DeliverPush acts on one delivery and returns what it did, in a sentence the
// Git host shows beside the delivery and the settings page shows as the last
// one. The error is for a delivery that was refused; one that was accepted and
// deliberately ignored — another branch, a tag — is not an error.
func (s *Service) DeliverPush(ctx context.Context, appID uuid.UUID, p Push) (string, error) {
	row, err := s.q.GetHookForDelivery(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrHookUnauthorized
	}
	if err != nil {
		return "", fmt.Errorf("app: read webhook: %w", err)
	}
	secret, err := s.keeper.Open(row.Secret)
	if err != nil {
		return "", fmt.Errorf("app: open webhook secret: %w", err)
	}
	if !pushSigned(secret, p) {
		return "", ErrHookUnauthorized
	}
	a := toApp(row.App)

	result, err := s.actOnPush(ctx, a, p)
	if err != nil {
		return "", err
	}
	if recErr := s.q.RecordHookDelivery(ctx, dbgen.RecordHookDeliveryParams{
		AppID: a.ID, Result: result,
	}); recErr != nil {
		s.log.Warn("record webhook delivery", slog.String("app", a.Name),
			slog.String("error", recErr.Error()))
	}
	return result, nil
}

// pushSigned checks a delivery against the secret, in constant time, in
// whichever form its host signs.
func pushSigned(secret string, p Push) bool {
	if p.Token != "" {
		return subtle.ConstantTimeCompare([]byte(p.Token), []byte(secret)) == 1
	}
	sig := strings.TrimPrefix(strings.TrimSpace(p.Signature), "sha256=")
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(p.Body)
	return hmac.Equal(got, mac.Sum(nil))
}

// actOnPush decides what an authenticated delivery means for the app.
func (s *Service) actOnPush(ctx context.Context, a App, p Push) (string, error) {
	switch strings.ToLower(strings.TrimSpace(p.Event)) {
	case "ping":
		// GitHub's test delivery when a hook is created. Answering it is how
		// the person setting it up learns the URL and secret are right.
		return "Webhook connected.", nil
	case "", "push", "push hook":
	default:
		return fmt.Sprintf("Ignored a %q event — only pushes deploy.", p.Event), nil
	}
	if a.Source != SourceGit {
		return "", ErrNotGitApp
	}

	var payload pushPayload
	if err := json.Unmarshal(p.Body, &payload); err != nil {
		return "", fmt.Errorf("app: read push payload: %w", err)
	}
	branch, isBranch := strings.CutPrefix(payload.Ref, "refs/heads/")
	switch {
	case !isBranch:
		return fmt.Sprintf("Ignored a push to %s — only branches deploy.", orUnknown(payload.Ref)), nil
	case branch != a.Repo.Ref():
		return fmt.Sprintf("Ignored a push to %s — this app builds %s.", branch, a.Repo.Ref()), nil
	case payload.Deleted || strings.Trim(payload.After, "0") == "":
		return fmt.Sprintf("Ignored the deletion of %s.", branch), nil
	}

	revision := shortRevision(payload.After)
	err := s.admitPush(ctx, a, revision)
	if errors.Is(err, ErrOperationInFlight) {
		// Built when the deploy in flight ends, rather than dropped: that build
		// may already have cloned the commit before this one.
		if markErr := s.q.MarkHookPending(ctx, dbgen.MarkHookPendingParams{
			AppID: a.ID, Revision: revision,
		}); markErr != nil {
			return "", fmt.Errorf("app: hold push behind the deploy in flight: %w", markErr)
		}
		return fmt.Sprintf("Queued %s — it deploys when the deploy in flight ends.", revision), nil
	}
	if err != nil {
		return "", err
	}
	s.log.Info("push admitted", slog.String("app", a.Name), slog.String("revision", revision))
	return fmt.Sprintf("Deploying %s.", revision), nil
}

// admitPush admits a build of the app's branch, as a person's redeploy would.
// The build reads the branch as it is when it runs, which is this push or a
// later one — never an earlier one.
func (s *Service) admitPush(ctx context.Context, a App, revision string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app: begin push admission: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	// Whatever was waiting is covered by this build too.
	if _, err := q.TakeHookPending(ctx, a.ID); err != nil {
		return fmt.Errorf("app: clear waiting push: %w", err)
	}
	if _, err := s.admitDeploymentTx(ctx, q, a.OwnerID, a,
		PushTriggerPrefix+revision, uuid.Nil, true, "webhook"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app: commit push admission: %w", err)
	}
	return nil
}

// admitPendingPushes admits pushes that arrived during a deploy which has
// since ended. Called from the admission loop, so it runs wherever deploys do.
func (s *Service) admitPendingPushes(ctx context.Context) error {
	rows, err := s.q.ListAdmissiblePushes(ctx, 20)
	if err != nil {
		return fmt.Errorf("app: list waiting pushes: %w", err)
	}
	for _, row := range rows {
		a := toApp(row.App)
		err := s.admitPush(ctx, a, row.PendingRevision)
		switch {
		case errors.Is(err, ErrOperationInFlight):
			// Somebody deployed in between. Still waiting; next pass.
			continue
		case err != nil:
			return err
		}
		result := fmt.Sprintf("Deploying %s, which waited for the deploy before it.", row.PendingRevision)
		if recErr := s.q.RecordHookDelivery(ctx, dbgen.RecordHookDeliveryParams{
			AppID: a.ID, Result: result,
		}); recErr != nil {
			s.log.Warn("record webhook delivery", slog.String("app", a.Name),
				slog.String("error", recErr.Error()))
		}
		s.log.Info("waiting push admitted", slog.String("app", a.Name),
			slog.String("revision", row.PendingRevision))
	}
	return nil
}

func shortRevision(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func orUnknown(s string) string {
	if s == "" {
		return "an unnamed ref"
	}
	return s
}
