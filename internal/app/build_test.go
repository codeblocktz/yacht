package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/codeblocktz/yacht/internal/store/dbgen"
)

// A build that prints without end keeps its log to the install's limit, says
// once where it was cut, and is not appended to after: writers racing past
// the limit together still leave one marker and nothing beyond it.
func TestABuildLogStopsAtItsLimitWithOneMarker(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &stubBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-build-log-limit")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const limit = 1000
	s.opts.MaxBuildLogBytes = limit
	marker := buildLogTruncated(limit)
	row := abandonedBuild(t, s, ownerID, a)
	logf := s.buildLogger(ctx, row.ID)

	// Several bytes to a character, so the limit is held in bytes and never
	// splits one: a line is kept whole or not at all.
	const line = "étape ✓ 构建 0123\n"
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 40 {
				logf(line)
			}
		})
	}
	wg.Wait()
	logf("printed after the limit\n")

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	if !got.LogTruncated {
		t.Fatal("a log past its limit is not marked truncated")
	}
	if n := strings.Count(got.Log, marker); n != 1 {
		t.Fatalf("truncation marker written %d times, want once", n)
	}
	kept, ok := strings.CutSuffix(got.Log, marker)
	if !ok {
		t.Fatalf("log does not end at its marker: %q", got.Log[max(0, len(got.Log)-200):])
	}
	if len(kept) > limit || len(kept) <= limit-len(line) {
		t.Fatalf("kept %d bytes before the marker, want the most whole lines within %d", len(kept), limit)
	}
	if strings.Count(kept, line)*len(line) != len(kept) {
		t.Fatal("a line was cut part way through")
	}
	if !utf8.ValidString(got.Log) {
		t.Fatal("the cut left a character in pieces")
	}
	if !strings.Contains(marker, "1000 bytes") {
		t.Fatalf("marker does not say the limit: %q", marker)
	}
}

// A reader following a build's log is sent what is new since it last looked,
// counted so that output in any script resumes where it stopped.
func TestABuildLogIsReadFromWhereTheLastReadEnded(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &stubBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-build-log-follow")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := abandonedBuild(t, s, ownerID, a)
	logf := s.buildLogger(ctx, row.ID)

	logf("Étape 1 — clonage ✓\n")
	first, err := s.BuildLogFrom(ctx, ownerID, row.DeploymentID, 0)
	if err != nil {
		t.Fatalf("BuildLogFrom: %v", err)
	}
	if first.Text != "Étape 1 — clonage ✓\n" || !first.Running {
		t.Fatalf("first read = %#v", first)
	}
	logf("步骤 2 构建\n")
	second, err := s.BuildLogFrom(ctx, ownerID, row.DeploymentID, first.Next)
	if err != nil {
		t.Fatalf("BuildLogFrom: %v", err)
	}
	if second.Text != "步骤 2 构建\n" {
		t.Fatalf("second read = %q, want only what was new", second.Text)
	}
	idle, err := s.BuildLogFrom(ctx, ownerID, row.DeploymentID, second.Next)
	if err != nil {
		t.Fatalf("BuildLogFrom: %v", err)
	}
	if idle.Text != "" || idle.Next != second.Next {
		t.Fatalf("a read with nothing new = %#v", idle)
	}
	if _, err := s.BuildLogFrom(ctx, "owner-someone-else", row.DeploymentID, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another owner read the log: %v", err)
	}
}

// A log cut at its limit lost the build's last lines, which say why it
// failed: the failure is written after the marker when the build finishes.
// What runs after the build, the pre-deploy command, is written into a
// reserve past the limit, and what does not fit there is dropped.
func TestATruncatedBuildLogStillSaysWhyAndWhatRanAfter(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Builder: &stubBuilder{}, Images: stubImages{}})
	ownerID := owner(t, s, pool, "owner-build-log-tail")
	a, err := createAndDeploy(t, s, ctx, ownerID, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 80,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	const limit = 1000
	s.opts.MaxBuildLogBytes = limit
	marker := buildLogTruncated(limit)
	row := abandonedBuild(t, s, ownerID, a)
	logf := s.buildLogger(ctx, row.ID)
	for range 100 {
		logf("compiling module ...\n")
	}

	const reason = "exit status 1: npm ERR! missing script: build"
	if _, err := s.q.FinishBuild(ctx, dbgen.FinishBuildParams{
		ID: row.ID, Status: BuildFailed, Message: reason,
	}); err != nil {
		t.Fatalf("FinishBuild: %v", err)
	}
	tail := s.buildLogTail(ctx, row.ID)
	tail("Running pre-deploy: ./migrate\n")
	tail(strings.Repeat("x", BuildLogTailReserve) + "\n")
	tail("migrated\n")

	got, err := s.q.GetBuild(ctx, dbgen.GetBuildParams{OwnerID: ownerID, ID: row.ID})
	if err != nil {
		t.Fatalf("GetBuild: %v", err)
	}
	_, after, ok := strings.Cut(got.Log, marker)
	if !ok {
		t.Fatal("the log was not cut")
	}
	if want := reason + "\nRunning pre-deploy: ./migrate\nmigrated\n"; after != want {
		t.Fatalf("after the marker = %q, want %q", after, want)
	}
}
