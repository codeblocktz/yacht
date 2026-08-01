package app

import (
	"crypto/rand"
	"encoding/base64"
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

	// SourcePostgres is a Postgres database. Yacht knows what the image is, so
	// it can mount the data directory, run as the right user, and generate the
	// credentials — none of which a person should have to look up.
	SourcePostgres Source = "postgres"
)

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
	// %s for the generated password.
	ConnectionTemplate string
	ConnectionKey      string
}

// Blueprints returns every source, in the order a picker should show them.
//
// Unavailable ones are included. What a product does not do yet is worth
// stating plainly in the place somebody looks for it, rather than leaving them
// to conclude it was never considered.
func Blueprints() []Blueprint {
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
			Label:       "Postgres",
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
			},
			GeneratedSecrets:   []string{"POSTGRES_PASSWORD"},
			ConnectionKey:      "DATABASE_URL",
			ConnectionTemplate: "postgres://yacht:%s@%s:5432/yacht?sslmode=disable",
		},
		{
			Source:      "git",
			Label:       "GitHub Repository",
			Description: "Build from a repository and deploy the result.",
			Because:     "the build pipeline is not built yet",
		},
		{
			Source:      "template",
			Label:       "Template",
			Description: "Deploy a preconfigured stack.",
			Because:     "no templates are defined yet",
		},
	}
}

// BlueprintFor returns the blueprint for a source.
func BlueprintFor(src Source) (Blueprint, error) {
	for _, b := range Blueprints() {
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
