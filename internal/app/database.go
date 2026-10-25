package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/codeblocktz/yacht/internal/database"
	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// ErrNotADatabase is an app the Database and Backups tabs were asked about
// that was not deployed from a database source.
var ErrNotADatabase = errors.New("app: not a database")

// PrivateHost is the name other workloads in the cluster reach the app by.
func (a App) PrivateHost() string {
	return a.Name + "." + a.Namespace + ".svc.cluster.local"
}

// DatabaseCredentials is how to sign in to a database app, read from its own
// variables at the moment of asking.
//
// From the variables rather than the blueprint, because they are what the app
// is running with: a password somebody replaced under Variables is the one in
// the container's environment, and the one the database was told at its first
// start unless they also changed it there.
type DatabaseCredentials struct {
	Source   Source
	Host     string
	Port     int32
	User     string
	Password string
	Database string

	// URLKey is the variable other apps are given, and URL its value: what
	// Connect shows and what ${{name.URLKey}} resolves to.
	URLKey string
	URL    string
}

// databaseKeys are where each engine's credentials are among its variables,
// and what to assume where a variable was deleted.
type databaseKeys struct {
	user, userDefault string
	password          string
	name, nameDefault string
}

var databaseKeysBySource = map[Source]databaseKeys{
	SourcePostgres: {user: "POSTGRES_USER", userDefault: "postgres", password: "POSTGRES_PASSWORD",
		name: "POSTGRES_DB", nameDefault: ""},
	SourceMySQL: {userDefault: "root", password: "MYSQL_ROOT_PASSWORD",
		name: "MYSQL_DATABASE"},
	SourceRedis: {userDefault: "default", password: "REDIS_PASSWORD"},
	SourceMongo: {user: "MONGO_INITDB_ROOT_USERNAME", password: "MONGO_INITDB_ROOT_PASSWORD"},
}

// DatabaseCredentials opens a database app's credentials.
//
// Secrets are opened through envFor, the one place they are opened, and go to
// the caller and nowhere else: the dashboard dials the database with them, or
// shows the connection string to somebody allowed to see it.
func (s *Service) DatabaseCredentials(ctx context.Context, ownerID, name string) (DatabaseCredentials, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return DatabaseCredentials{}, err
	}
	return s.databaseCredentials(ctx, a)
}

func (s *Service) databaseCredentials(ctx context.Context, a App) (DatabaseCredentials, error) {
	keys, ok := databaseKeysBySource[a.Source]
	if !ok {
		return DatabaseCredentials{}, ErrNotADatabase
	}
	plain, secrets, err := s.envFor(ctx, s.q, a.OwnerID, a.ID)
	if err != nil {
		return DatabaseCredentials{}, err
	}
	read := func(key, fallback string) string {
		if key == "" {
			return fallback
		}
		if v, ok := secrets[key]; ok {
			return v
		}
		if v, ok := plain[key]; ok && v != "" {
			return v
		}
		return fallback
	}
	c := DatabaseCredentials{
		Source:   a.Source,
		Host:     a.PrivateHost(),
		Port:     orchestrator.ServicePort(a.Internal, a.Port),
		User:     read(keys.user, keys.userDefault),
		Password: read(keys.password, ""),
	}
	c.Database = read(keys.name, keys.nameDefault)
	if c.Database == "" && a.Source == SourcePostgres {
		// The image's own rule: POSTGRES_DB defaults to the user's name.
		c.Database = c.User
	}
	if b, err := BlueprintFor(a.Source); err == nil {
		c.URLKey = b.ConnectionKey
		c.URL = read(b.ConnectionKey, "")
	}
	return c, nil
}

// DatabaseBrowser is the data browser for a database app, pointed at its
// Service with its own credentials.
//
// The address is the Service's cluster address rather than its DNS name: the
// engine may run as a process on a node, where cluster DNS does not resolve
// and the cluster address still routes — the same path the waker hands held
// requests down (see orchestrator.ServiceAddresser).
//
// Only Postgres for now. MySQL is next and would sit beside it; Redis and
// MongoDB have no tables, and say so rather than showing an empty list.
func (s *Service) DatabaseBrowser(ctx context.Context, ownerID, name string) (database.Browser, error) {
	a, err := s.Get(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	if !a.Source.Database() {
		return nil, ErrNotADatabase
	}
	if a.Source != SourcePostgres {
		return nil, database.ErrNoBrowser
	}
	creds, err := s.databaseCredentials(ctx, a)
	if err != nil {
		return nil, err
	}
	addr, err := s.databaseAddress(ctx, a)
	if err != nil {
		return nil, err
	}
	return database.Postgres(database.Target{
		Addr: addr, User: creds.User, Password: creds.Password, Database: creds.Database,
	}, database.Limits{}), nil
}

// ErrDatabaseUnreachable is a database the engine has no way to dial: an
// orchestrator that cannot say where a workload's Service answers.
var ErrDatabaseUnreachable = errors.New(
	"app: the engine cannot reach this cluster's services from where it runs")

func (s *Service) databaseAddress(ctx context.Context, a App) (string, error) {
	addr, ok := s.orch.(orchestrator.ServiceAddresser)
	if !ok {
		return "", ErrDatabaseUnreachable
	}
	got, err := addr.ServiceAddress(ctx, a.Ref())
	if err != nil {
		if errors.Is(err, orchestrator.ErrNotFound) {
			return "", fmt.Errorf("app: %s has no Service yet — it has not been deployed", a.Name)
		}
		return "", err
	}
	return got, nil
}

// databaseNames are what Add › Database calls each engine's app, as Railway
// does; a second one in the team is postgres-2.
var databaseNames = map[Source]string{
	SourcePostgres: "postgres", SourceMySQL: "mysql", SourceRedis: "redis", SourceMongo: "mongo",
}

// DeployDatabase is Add › Database: one engine, named after itself, into a
// project, with nothing asked. Everything a database needs is in its
// blueprint, so there is nothing to ask; a name somebody prefers is a rename
// away, and the form with every option is still there for whoever wants it.
func (s *Service) DeployDatabase(ctx context.Context, ownerID string, src Source, projectID uuid.UUID) (App, error) {
	base, ok := databaseNames[src]
	if !ok {
		return App{}, ErrNotADatabase
	}
	// Twice: a name free when looked at can be taken by the time it is
	// created, by somebody pressing the same button in another tab.
	for attempt := 0; ; attempt++ {
		name, err := s.freeName(ctx, ownerID, base)
		if err != nil {
			return App{}, err
		}
		a, err := s.Create(ctx, ownerID, CreateInput{
			Source: src, Name: name, Replicas: 1, ProjectID: projectID,
		})
		if errors.Is(err, ErrNameTaken) && attempt == 0 {
			continue
		}
		return a, err
	}
}
