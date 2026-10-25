package app

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// Source is what an app was created from.
//
// Stored on the app rather than inferred, because it decides things that
// cannot be worked out from the image later: whether a hostname is issued,
// which uid the container runs as, and where its data lives.
type Source string

const (
	// SourceImage is a container image the person chose. Yacht knows nothing
	// about what is inside it, so it configures nothing on its behalf.
	SourceImage Source = "image"

	// SourceTemplate is a stack rather than a single app. It never reaches an
	// app row: the template page creates each app with that app's own source.
	SourceTemplate Source = "template"

	// SourceGit is a repository Yacht builds into an image and then deploys.
	// The image on the app row is what the last build produced, so it is set
	// by the build rather than chosen.
	SourceGit Source = "git"

	// SourcePostgres is a Postgres database. Yacht knows what the image is, so
	// it can mount the data directory, run as the right user, and generate the
	// credentials — none of which a person should have to look up.
	SourcePostgres Source = "postgres"

	// SourceMySQL, SourceRedis and SourceMongo are the other databases
	// Railway's Add › Database offers, configured the same way and for the
	// same reason.
	SourceMySQL Source = "mysql"
	SourceRedis Source = "redis"
	SourceMongo Source = "mongo"
)

// Database reports whether a source is one of the database engines.
//
// A database gets the Database and Backups tabs, a logo on its card, and a
// place in Add › Database rather than among the other sources. Asked of the
// source rather than inferred from the image, for the reason Source is stored
// at all: an image somebody chose that happens to be postgres is theirs to
// run, and Yacht knows nothing about its credentials.
func (s Source) Database() bool {
	switch s {
	case SourcePostgres, SourceMySQL, SourceRedis, SourceMongo:
		return true
	}
	return false
}

// Blueprint is everything a source decides on the app's behalf.
//
// The point of the type is that a source is data rather than a branch: adding
// one is filling this in, and every part of the system that consumes a
// blueprint keeps working without knowing the new source exists.
type Blueprint struct {
	Source Source

	// Label and Description are what the picker shows.
	Label       string
	Description string

	// Available reports whether this source can be deployed at all.
	// Unavailable is listed with its reason rather than hidden: a menu that
	// silently omits things reads as a product that does not have them.
	Available bool
	Because   string

	Image string
	Port  int32

	// Internal keeps the workload off the public internet. A database speaks
	// its own protocol on its own port, so an HTTP hostname pointed at it
	// would be a route to something that cannot answer.
	Internal bool

	// RunAsUser, FSGroup and ScratchPaths carry what the image needs in order
	// to run under a restricted security context. They are facts about a
	// specific image, which is exactly why only a source that names the image
	// can supply them.
	RunAsUser    int64
	FSGroup      int64
	ScratchPaths []string

	// Volume is storage created with the app rather than attached afterwards.
	// A database with no data directory is not a database somebody has to
	// finish configuring; it is one that has not been deployed.
	Volume *VolumeInput

	// Env and generated secrets. GeneratedSecrets name variables whose values
	// are minted at create time and never shown — a password nobody chose is
	// one nobody can have reused elsewhere.
	Env              map[string]string
	GeneratedSecrets []string

	// ConnectionTemplate builds the in-cluster address other apps use, with
	// %s for the generated password and then the host.
	ConnectionTemplate string
	ConnectionKey      string

	// PasswordKeys are further variables holding the generated password, and
	// HostKeys variables holding the address other apps reach this one at.
	// Each engine's own clients look for their own spelling — psql reads
	// PGPASSWORD and PGHOST — and Railway sets every one of them, so an app
	// moved from there finds the names it already uses.
	PasswordKeys []string
	HostKeys     []string

	// Args replace the image's command arguments. Only for an engine that
	// takes its configuration nowhere else: Redis has no variable for a
	// password, only a flag. $(NAME) is the app's variable NAME, expanded by
	// the kubelet, which is how a generated secret reaches a flag without
	// being written into the pod template.
	Args []string
}

// Capabilities is what this install can actually do.
//
// Passed in rather than discovered here, because whether a source works is a
// fact about how this install is configured and this file is a description of
// what the sources are. The zero value claims nothing, which is the right
// default for a caller that has not asked: a picker that offers building on an
// install with nowhere to push produces a create that fails at the end.
type Capabilities struct {
	// CanBuild reports whether there is an image registry to push to. Without
	// one a build has nowhere to put the result and the cluster nothing to
	// pull, so the source is listed and refused rather than offered.
	CanBuild bool
}

// Blueprints returns every source with nothing optional available.
//
// Kept for callers that only need the list — the labels, the descriptions, the
// order. Anything deciding whether a source can be used wants BlueprintsFor.
func Blueprints() []Blueprint { return BlueprintsFor(Capabilities{}) }

// BlueprintsFor returns every source, in the order a picker should show them.
//
// Unavailable ones are included. What a product does not do yet is worth
// stating plainly in the place somebody looks for it, rather than leaving them
// to conclude it was never considered.
func BlueprintsFor(c Capabilities) []Blueprint {
	return []Blueprint{
		{
			Source:      SourceImage,
			Label:       "Docker Image",
			Description: "Run a container image from any registry.",
			Available:   true,
			Port:        8080,
		},
		{
			Source:      SourcePostgres,
			Label:       "PostgreSQL",
			Description: "A Postgres 16 database, with its storage and credentials set up.",
			Available:   true,
			Image:       "postgres:16-alpine",
			Port:        5432,
			Internal:    true,

			// The uid the postgres user has in the alpine image. Verified
			// against a cluster rather than read off a page: the security
			// posture refuses the image outright without it.
			RunAsUser: 70,
			FSGroup:   70,

			// Postgres opens a Unix socket here, and the root filesystem is
			// read-only.
			ScratchPaths: []string{"/var/run/postgresql"},

			Volume: &VolumeInput{
				Name: "data", MountPath: "/var/lib/postgresql/data",
				SizeBytes: 1 << 30,
			},
			Env: map[string]string{
				"POSTGRES_USER": "yacht",
				"POSTGRES_DB":   "yacht",
				// A subdirectory of the mount, not the mount itself: the mount
				// point is not empty on every storage class, and initdb
				// refuses a directory that is not.
				"PGDATA": "/var/lib/postgresql/data/pgdata",
				// What psql and libpq read. PGHOST in the database's own
				// container is harmless: the entrypoint clears it for the
				// scripts it runs against the server it is setting up.
				"PGUSER":     "yacht",
				"PGDATABASE": "yacht",
				"PGPORT":     "5432",
			},
			GeneratedSecrets:   []string{"POSTGRES_PASSWORD"},
			PasswordKeys:       []string{"PGPASSWORD"},
			HostKeys:           []string{"PGHOST"},
			ConnectionKey:      "DATABASE_URL",
			ConnectionTemplate: "postgres://yacht:%s@%s:5432/yacht?sslmode=disable",
		},
		{
			Source:      SourceMySQL,
			Label:       "MySQL",
			Description: "A MySQL 8 database, with its storage and credentials set up.",
			Available:   true,
			Image:       "mysql:8",
			Port:        3306,
			Internal:    true,

			// The mysql user in the image. Checked by running the image as it,
			// read-only, with a fresh root-owned volume: as anyone else the
			// entrypoint cannot write the data directory it initialises.
			RunAsUser: 999,
			FSGroup:   999,

			// mysqld's socket, lock and pid files go here, and it refuses to
			// start without them.
			ScratchPaths: []string{"/var/run/mysqld"},

			// The mount point itself, unlike Postgres: MySQL 8 initialises a
			// directory holding lost+found without complaint, which is what an
			// ext4 claim arrives with.
			Volume: &VolumeInput{
				Name: "data", MountPath: "/var/lib/mysql",
				SizeBytes: 1 << 30,
			},
			Env: map[string]string{
				"MYSQL_DATABASE": "yacht",
				"MYSQLDATABASE":  "yacht",
				"MYSQLUSER":      "root",
				"MYSQLPORT":      "3306",
			},
			GeneratedSecrets:   []string{"MYSQL_ROOT_PASSWORD"},
			PasswordKeys:       []string{"MYSQLPASSWORD"},
			HostKeys:           []string{"MYSQLHOST"},
			ConnectionKey:      "MYSQL_URL",
			ConnectionTemplate: "mysql://root:%s@%s:3306/yacht",
		},
		{
			Source:      SourceRedis,
			Label:       "Redis",
			Description: "A Redis 7 server with a password, saving to its own storage.",
			Available:   true,
			Image:       "redis:7-alpine",
			Port:        6379,
			Internal:    true,

			// The redis user in the alpine image. Its group is 1000, but the
			// group a process runs as only has to own what it writes, and the
			// volume belongs to fsGroup.
			RunAsUser: 999,
			FSGroup:   999,

			Volume: &VolumeInput{
				Name: "data", MountPath: "/data",
				SizeBytes: 1 << 30,
			},
			Env: map[string]string{
				"REDISUSER": "default",
				"REDISPORT": "6379",
			},
			GeneratedSecrets:   []string{"REDIS_PASSWORD"},
			PasswordKeys:       []string{"REDISPASSWORD"},
			HostKeys:           []string{"REDISHOST"},
			ConnectionKey:      "REDIS_URL",
			ConnectionTemplate: "redis://default:%s@%s:6379",

			// Without an append-only file a restart keeps only the last
			// snapshot, which is minutes of writes lost to every redeploy. The
			// password is on the command line because Redis reads it nowhere
			// else; anybody who can list the container's processes already has
			// its environment.
			Args: []string{
				"redis-server",
				"--requirepass", "$(REDIS_PASSWORD)",
				"--appendonly", "yes",
			},
		},
		{
			Source:      SourceMongo,
			Label:       "MongoDB",
			Description: "A MongoDB 7 database, with its storage and credentials set up.",
			Available:   true,
			Image:       "mongo:7",
			Port:        27017,
			Internal:    true,

			// The mongodb user in the image. The entrypoint's first start, the
			// one that creates the root user, keeps its socket and temporary
			// config in /tmp, which is always writable.
			RunAsUser: 999,
			FSGroup:   999,

			Volume: &VolumeInput{
				Name: "data", MountPath: "/data/db",
				SizeBytes: 1 << 30,
			},
			Env: map[string]string{
				"MONGO_INITDB_ROOT_USERNAME": "mongo",
				"MONGOUSER":                  "mongo",
				"MONGOPORT":                  "27017",
			},
			GeneratedSecrets:   []string{"MONGO_INITDB_ROOT_PASSWORD"},
			PasswordKeys:       []string{"MONGOPASSWORD"},
			HostKeys:           []string{"MONGOHOST"},
			ConnectionKey:      "MONGO_URL",
			ConnectionTemplate: "mongodb://mongo:%s@%s:27017",
		},
		{
			Source:      SourceGit,
			Label:       "Git repository",
			Description: "Build from a repository on GitHub, GitLab, Gitea or Forgejo, and deploy the result.",
			Available:   c.CanBuild,
			Because:     "no image registry is configured for this install",
			// No image: a build produces one. No port either — that is the
			// person's to say, because only they know what their code listens
			// on, and guessing produces a service that routes to nothing.
			Port: 8080,
		},
		{
			// Available, but not a source in the way the others are: a template
			// makes several apps rather than one, so the picker sends it to its
			// own page instead of the create form. BlueprintFor still refuses
			// it, which is what stops it reaching Create as if it were an image.
			Source:      SourceTemplate,
			Label:       "Template",
			Description: "Deploy a preconfigured stack, already wired together.",
			Available:   true,
		},
	}
}

// BlueprintFor returns the blueprint for a source.
//
// A template has no blueprint. It is not a thing an app can be created from —
// it creates apps — so asking for one is a wiring mistake rather than a state
// to handle, and it is refused here where every create path passes through.
func BlueprintFor(src Source) (Blueprint, error) {
	return BlueprintForWith(src, Capabilities{})
}

// BlueprintForWith returns the blueprint for a source on this install.
func BlueprintForWith(src Source, c Capabilities) (Blueprint, error) {
	if src == SourceTemplate {
		return Blueprint{}, errors.New(
			"app: a template makes several apps and cannot be one")
	}
	for _, b := range BlueprintsFor(c) {
		if b.Source == src {
			if !b.Available {
				return Blueprint{}, fmt.Errorf("app: %s is not available — %s", b.Label, b.Because)
			}
			return b, nil
		}
	}
	return Blueprint{}, fmt.Errorf("app: unknown source %q", src)
}

// generatedSecret mints a password nobody chose.
//
// URL-safe, because it goes into a connection string and a password that has
// to be escaped is one that eventually is not.
func generatedSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("app: generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
