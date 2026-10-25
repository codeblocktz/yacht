package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// DefaultPort is where an app is sent traffic when nothing says otherwise:
// the port most frameworks take from PORT, which the app is given.
const DefaultPort int32 = 8080

// DeployFromGitHub is the one-click deploy: a repository the team's GitHub
// connection reaches, built from its default branch and its root, and run
// with nothing asked. Its name is the repository's, made free if taken; its
// port is the one its Dockerfile exposes, or DefaultPort. Everything else —
// variables, branch, directory, domain — is changed on the app afterwards,
// as somebody looking at it running knows what it needs and somebody
// filling in a form beforehand guesses.
func (s *Service) DeployFromGitHub(
	ctx context.Context, ownerID string, ref GitHubRef, projectID uuid.UUID,
) (App, error) {
	if s.gh == nil {
		return App{}, errors.New("app: this install has no GitHub App")
	}
	if !ref.Set() {
		return App{}, errors.New("app: choose a repository")
	}
	repo, err := s.resolveGitHubRepo(ctx, ownerID, Repo{GitHub: ref})
	if err != nil {
		return App{}, err
	}
	name, err := s.freeName(ctx, ownerID, repoName(repo.URL))
	if err != nil {
		return App{}, err
	}
	port := DefaultPort
	if df, err := s.gh.File(ctx, ref.InstallationID, ref.RepoID, "Dockerfile"); err == nil {
		if p := ExposedPort(df); p > 0 {
			port = p
		}
	}
	return s.Create(ctx, ownerID, CreateInput{
		Source: SourceGit, Name: name, Port: port, Replicas: 1,
		Repo: Repo{GitHub: ref}, ProjectID: projectID,
	})
}

// repoName is an app name from a repository's URL: its last part, as a name
// may be written.
func repoName(url string) string {
	base := strings.TrimSuffix(path.Base(strings.TrimSuffix(url, "/")), ".git")
	name := Slugify(base)
	if len(name) > maxNameLen {
		name = strings.Trim(name[:maxNameLen], "-")
	}
	if name == "" {
		name = "app"
	}
	return name
}

// maxNameLen is the longest name CreateInput.Validate takes.
const maxNameLen = 40

// freeName is name, or name-2, name-3 and so on, whichever no app of the
// team has.
func (s *Service) freeName(ctx context.Context, ownerID, name string) (string, error) {
	for i := 1; i < 100; i++ {
		candidate := name
		if i > 1 {
			suffix := "-" + strconv.Itoa(i)
			base := name
			if len(base)+len(suffix) > maxNameLen {
				base = strings.Trim(base[:maxNameLen-len(suffix)], "-")
			}
			candidate = base + suffix
		}
		_, err := s.Get(ctx, ownerID, candidate)
		if errors.Is(err, ErrNotFound) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("app: every name like %q is taken", name)
}

// ExposedPort is the first port a Dockerfile's last stage exposes — the
// stage that is the image — or 0 when it exposes none it names as a number.
func ExposedPort(dockerfile []byte) int32 {
	var port int32
	sc := bufio.NewScanner(bytes.NewReader(dockerfile))
	var line strings.Builder
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		// A continued instruction is one line.
		if cont, ok := strings.CutSuffix(text, "\\"); ok {
			line.WriteString(cont + " ")
			continue
		}
		line.WriteString(text)
		f := strings.Fields(line.String())
		line.Reset()
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "FROM":
			port = 0
		case "EXPOSE":
			if port != 0 {
				continue
			}
			for _, p := range f[1:] {
				p, _, _ = strings.Cut(p, "/")
				if n, err := strconv.ParseInt(p, 10, 32); err == nil && n > 0 && n <= 65535 {
					port = int32(n)
					break
				}
			}
		}
	}
	return port
}
