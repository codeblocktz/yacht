package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// alwaysLeftOut is never uploaded, whatever the ignore files say: a
// repository's history is not source, and the link directory names the app
// and nothing the build needs.
var alwaysLeftOut = []string{".git", ".hg", ".svn"}

// Archive is a directory packed for `up`.
type Archive struct {
	Data  []byte
	Files int
	// Expanded is the total size of the files, before compression.
	Expanded int64
}

// pack tars and gzips a directory the way `docker build` would see it: what
// .gitignore and .dockerignore leave out stays out.
//
// Both files are read, because a project usually has one or the other and
// either is a statement of what is not source. Every .gitignore down the
// tree applies to its own directory, as git reads them. .dockerignore is read
// with gitignore's rules too, which differ in one way worth knowing: a bare
// name there matches at any depth, where Docker matches it at the root only —
// so this can leave out a little more than docker build would, never less.
//
// Symbolic links are kept as links rather than followed, so a link to
// somewhere outside the directory uploads nothing from there; the engine
// refuses one that points outside, and says which.
func pack(root, linkDir string) (Archive, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Archive{}, err
	}
	var patterns []gitignore.Pattern
	for _, name := range append(append([]string{}, alwaysLeftOut...), linkDir) {
		patterns = append(patterns, gitignore.ParsePattern("/"+name, nil))
	}
	patterns = append(patterns, readIgnore(filepath.Join(root, ".dockerignore"), nil)...)

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	out := Archive{}

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			patterns = append(patterns, readIgnore(filepath.Join(p, ".gitignore"), nil)...)
			return nil
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if gitignore.NewMatcher(patterns).Match(parts, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			patterns = append(patterns, readIgnore(filepath.Join(p, ".gitignore"), parts)...)
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(),
			})
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink, Name: name, Linkname: filepath.ToSlash(target),
				Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(),
			})
		case info.Mode().IsRegular():
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: name, Size: info.Size(),
				Mode: int64(info.Mode().Perm()), ModTime: info.ModTime(),
			}); err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
			out.Files++
			out.Expanded += info.Size()
			return nil
		}
		// A socket, a device, a pipe: nothing a build reads, and nothing the
		// engine would accept.
		return nil
	})
	if err != nil {
		return Archive{}, err
	}
	if err := tw.Close(); err != nil {
		return Archive{}, err
	}
	if err := zw.Close(); err != nil {
		return Archive{}, err
	}
	if out.Files == 0 {
		return Archive{}, errors.New("there are no files to upload here — everything is ignored, or the directory is empty")
	}
	out.Data = buf.Bytes()
	return out, nil
}

// readIgnore reads one ignore file's patterns, for the directory it is in.
func readIgnore(path string, domain []string) []gitignore.Pattern {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ps []gitignore.Pattern
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, domain))
	}
	return ps
}

// humanSize is a size as somebody reads it.
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
