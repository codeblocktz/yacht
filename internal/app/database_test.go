package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/codeblocktz/yacht/internal/database"
	"github.com/codeblocktz/yacht/internal/orchestrator"
	"github.com/codeblocktz/yacht/internal/secret"
	"github.com/codeblocktz/yacht/internal/store"
)

// addressedOrchestrator answers ServiceAddress with one fixed address, the
// way the cluster answers with a Service's cluster IP.
type addressedOrchestrator struct {
	*recordingOrchestrator
	addr string
}

func (o *addressedOrchestrator) ServiceAddress(context.Context, orchestrator.Ref) (string, error) {
	return o.addr, nil
}

func databaseService(t *testing.T, addr string) (*Service, *addressedOrchestrator, string) {
	t.Helper()
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run app service tests")
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
	k, err := secret.NewKeeper(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)))
	if err != nil {
		t.Fatal(err)
	}
	orch := &addressedOrchestrator{
		recordingOrchestrator: &recordingOrchestrator{Noop: orchestrator.NewNoop()}, addr: addr,
	}
	s := NewService(pool, orch, log, Options{Keeper: k, Manifests: staticManifests{}})
	return s, orch, owner(t, s, pool, "svc-databases")
}

// Each engine arrives with the variables Railway gives it, so an app moved
// from there finds every name it already reads, and with what its image needs
// to start under the restricted profile.
func TestEveryDatabaseArrivesWithRailwaysVariables(t *testing.T) {
	s, orch, id := databaseService(t, "")
	ctx := context.Background()

	for _, tc := range []struct {
		source  Source
		plain   map[string]string
		secrets []string
		urlKey  string
		scheme  string
		uid     int64
		args    bool
	}{
		{
			source: SourcePostgres,
			plain: map[string]string{
				"PGUSER": "yacht", "PGDATABASE": "yacht", "PGPORT": "5432",
				"POSTGRES_USER": "yacht", "POSTGRES_DB": "yacht",
				"PGDATA": "/var/lib/postgresql/data/pgdata", "PGHOST": "",
			},
			secrets: []string{"POSTGRES_PASSWORD", "PGPASSWORD", "DATABASE_URL"},
			urlKey:  "DATABASE_URL", scheme: "postgres://yacht:", uid: 70,
		},
		{
			source: SourceMySQL,
			plain: map[string]string{
				"MYSQL_DATABASE": "yacht", "MYSQLDATABASE": "yacht",
				"MYSQLUSER": "root", "MYSQLPORT": "3306", "MYSQLHOST": "",
			},
			secrets: []string{"MYSQL_ROOT_PASSWORD", "MYSQLPASSWORD", "MYSQL_URL"},
			urlKey:  "MYSQL_URL", scheme: "mysql://root:", uid: 999,
		},
		{
			source:  SourceRedis,
			plain:   map[string]string{"REDISUSER": "default", "REDISPORT": "6379", "REDISHOST": ""},
			secrets: []string{"REDIS_PASSWORD", "REDISPASSWORD", "REDIS_URL"},
			urlKey:  "REDIS_URL", scheme: "redis://default:", uid: 999, args: true,
		},
		{
			source: SourceMongo,
			plain: map[string]string{
				"MONGO_INITDB_ROOT_USERNAME": "mongo", "MONGOUSER": "mongo",
				"MONGOPORT": "27017", "MONGOHOST": "",
			},
			secrets: []string{"MONGO_INITDB_ROOT_PASSWORD", "MONGOPASSWORD", "MONGO_URL"},
			urlKey:  "MONGO_URL", scheme: "mongodb://mongo:", uid: 999,
		},
	} {
		t.Run(string(tc.source), func(t *testing.T) {
			name := "db-" + string(tc.source)
			a, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: tc.source, Name: name})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if !a.Internal || a.Host != "" || len(a.Volumes) != 1 {
				t.Errorf("internal=%v host=%q volumes=%d; a database is private and has storage",
					a.Internal, a.Host, len(a.Volumes))
			}
			spec := orch.lastAppSpec()
			for key, want := range tc.plain {
				if want == "" {
					want = a.PrivateHost()
				}
				if got := spec.Env[key]; got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			for _, key := range tc.secrets {
				if spec.Secrets[key] == "" {
					t.Errorf("%s was not delivered as a secret", key)
				}
				if _, plain := spec.Env[key]; plain {
					t.Errorf("%s is in the pod template's plain environment", key)
				}
			}
			// The aliases are the same password, not a second one.
			pw := spec.Secrets[tc.secrets[0]]
			if spec.Secrets[tc.secrets[1]] != pw {
				t.Errorf("%s differs from %s", tc.secrets[1], tc.secrets[0])
			}
			wantURL := tc.scheme + pw + "@" + a.PrivateHost() + ":"
			if got := spec.Secrets[tc.urlKey]; !strings.HasPrefix(got, wantURL) {
				t.Errorf("%s = %q, want it to start %q", tc.urlKey, got, wantURL)
			}
			if spec.RunAsUser != tc.uid || spec.FSGroup != tc.uid {
				t.Errorf("runs as %d/%d, want %d", spec.RunAsUser, spec.FSGroup, tc.uid)
			}
			if tc.args {
				joined := strings.Join(spec.Args, " ")
				if !strings.Contains(joined, "--requirepass $(REDIS_PASSWORD)") {
					t.Errorf("args = %q; the password reaches Redis as a flag the kubelet expands", joined)
				}
				if strings.Contains(joined, pw) {
					t.Error("the password itself is written into the pod template's arguments")
				}
			} else if len(spec.Args) != 0 {
				t.Errorf("args = %q, want the image's own command", spec.Args)
			}

			creds, err := s.DatabaseCredentials(ctx, id, name)
			if err != nil {
				t.Fatal(err)
			}
			if creds.Password != pw || creds.URLKey != tc.urlKey || creds.URL != spec.Secrets[tc.urlKey] ||
				creds.Host != a.PrivateHost() {
				t.Errorf("credentials = %+v", creds)
			}
		})
	}

	// A release carries the arguments, so a rollback starts Redis the same way.
	redis, _ := s.Get(ctx, id, "db-redis")
	releases, err := s.Releases(ctx, id, redis.ID, 1)
	if err != nil || len(releases) != 1 || len(releases[0].Args) == 0 {
		t.Fatalf("releases = %+v, %v; want one carrying the arguments", releases, err)
	}

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Name: "web", Image: "nginx:1.27", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DatabaseCredentials(ctx, id, "web"); !errors.Is(err, ErrNotADatabase) {
		t.Errorf("an image app's credentials: %v, want ErrNotADatabase", err)
	}
	if _, err := s.DatabaseBrowser(ctx, id, "db-redis"); !errors.Is(err, database.ErrNoBrowser) {
		t.Errorf("Redis's browser: %v, want ErrNoBrowser", err)
	}
}

// The browser signs in with the app's own variables at the address the
// orchestrator gives, end to end against a real Postgres standing in for the
// database's pod.
func TestTheBrowserSignsInAsTheDatabaseItself(t *testing.T) {
	dsn := os.Getenv("YACHT_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set YACHT_TEST_DATABASE_URL to run app service tests")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	s, _, id := databaseService(t, net.JoinHostPort(u.Hostname(), port))
	ctx := context.Background()

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{Source: SourcePostgres, Name: "pg"}); err != nil {
		t.Fatal(err)
	}
	// What the database's entrypoint would have made at its first start: a
	// role and a database, under the names the app's variables give, with
	// the generated password.
	const role = "yacht_browse_owner"
	for _, kv := range [][2]string{{"POSTGRES_USER", role}, {"POSTGRES_DB", role}} {
		// Saved even with the first deploy still settling: the browser
		// reads the variables, not the running pod.
		if err := s.SetVariable(ctx, id, "pg", VariableInput{Key: kv[0], Value: kv[1]}); err != nil &&
			!errors.Is(err, ErrSavedWhileDeploying) {
			t.Fatal(err)
		}
	}
	creds, err := s.DatabaseCredentials(ctx, id, "pg")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	cleanup := func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+role+" WITH (FORCE)")
		_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+role)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD "+
		"'"+creds.Password+"'"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+role+" OWNER "+role); err != nil {
		t.Fatal(err)
	}

	b, err := s.DatabaseBrowser(ctx, id, "pg")
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Query(ctx, "SELECT current_user, current_database()")
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Rows[0][0].Text + "/" + res.Rows[0][1].Text; got != role+"/"+role {
		t.Errorf("signed in as %s, want %s/%s", got, role, role)
	}
}

// One click, no form: the engine named after itself, the next free name when
// that is taken, into the project it was added from.
func TestAddDatabaseDeploysOneEngineNamedAfterItself(t *testing.T) {
	s, _, id := databaseService(t, "")
	ctx := context.Background()
	p, err := s.CreateProject(ctx, id, "Shop")
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.DeployDatabase(ctx, id, SourcePostgres, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.DeployDatabase(ctx, id, SourcePostgres, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Name != "postgres" || second.Name != "postgres-2" {
		t.Errorf("names = %s, %s", first.Name, second.Name)
	}
	if first.ProjectID != p.ID || first.Source != SourcePostgres || first.Replicas != 1 {
		t.Errorf("app = %+v", first)
	}
	if _, err := s.DeployDatabase(ctx, id, SourceImage, p.ID); !errors.Is(err, ErrNotADatabase) {
		t.Errorf("an image through Add › Database: %v", err)
	}
}
