package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// The rule every entry point leans on: refuse only a rise, and only past the
// limit. Checked without a database because it is the whole of the policy, and
// the entry points below only prove they ask it.
func TestAQuotaOnlyRefusesARisePastIt(t *testing.T) {
	q := Quota{Apps: 2, CPUMillis: 1000}
	for _, tc := range []struct {
		name      string
		now, next Usage
		refused   bool
	}{
		{"under", Usage{Apps: 1}, Usage{Apps: 2}, false},
		{"exactly at it", Usage{CPUMillis: 500}, Usage{CPUMillis: 1000}, false},
		{"past it", Usage{Apps: 2}, Usage{Apps: 3}, true},
		{"already over and rising", Usage{CPUMillis: 1500}, Usage{CPUMillis: 1600}, true},
		// Somebody over a lowered quota has to be able to get back under.
		{"already over and falling", Usage{CPUMillis: 1500}, Usage{CPUMillis: 1200}, false},
		{"already over and unchanged", Usage{Apps: 5}, Usage{Apps: 5}, false},
		// Memory and storage are unlimited here, however much there is.
		{"unlimited dimension", Usage{MemoryBytes: 1}, Usage{MemoryBytes: 1 << 40}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := q.admit(tc.now, tc.next)
			if tc.refused && !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("admit = %v, want ErrQuotaExceeded", err)
			}
			if !tc.refused && err != nil {
				t.Fatalf("admit = %v, want allowed", err)
			}
		})
	}
}

// A blank limit is the namespace default, not nothing. Counted as nothing,
// leaving the field empty would be the way around every quota.
func TestAnAppWithNoLimitCountsAtTheDefault(t *testing.T) {
	cpu, mem := shape{Replicas: 3}.cost()
	if cpu != 3*defaultCPUMillis || mem != 3*defaultMemoryBytes {
		t.Fatalf("3 replicas with no limits = %dm, %d bytes; want 3 × the LimitRange default "+
			"(%dm, %d bytes)", cpu, mem, defaultCPUMillis, defaultMemoryBytes)
	}
	cpu, mem = shape{Replicas: 2, CPULimit: "1500m", MemoryLimit: "1Gi"}.cost()
	if cpu != 3000 || mem != 2<<30 {
		t.Fatalf("2 × (1500m, 1Gi) = %dm, %d bytes; want 3000m, 2Gi", cpu, mem)
	}
	if cpu, mem := (shape{Replicas: 0, CPULimit: "4"}).cost(); cpu != 0 || mem != 0 {
		t.Fatalf("a scaled-to-zero app commits %dm, %d bytes; want nothing", cpu, mem)
	}
}

func setQuota(t *testing.T, s *Service, ownerID string, q Quota) {
	t.Helper()
	if err := s.SetQuota(context.Background(), ownerID, q); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
}

func wantQuotaRefusal(t *testing.T, what string, err error, mentions string) {
	t.Helper()
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("%s = %v, want ErrQuotaExceeded", what, err)
	}
	if !strings.Contains(err.Error(), mentions) {
		t.Errorf("%s refused with %q, which does not say it was %s", what, err, mentions)
	}
}

func TestCreateIsHeldToTheQuota(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-quota-create")

	// No row at all: unlimited, which is every install that never set one.
	for _, name := range []string{"one", "two"} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
		}); err != nil {
			t.Fatalf("Create %s with no quota: %v", name, err)
		}
	}

	setQuota(t, s, id, Quota{Apps: 2})
	_, err := s.Create(ctx, id, CreateInput{Name: "three", Image: "nginx:alpine", Replicas: 1})
	wantQuotaRefusal(t, "a third app against a quota of two", err, "apps")
	if n, _ := s.Count(ctx, id); n != 2 {
		t.Fatalf("the refused create left %d apps, want 2", n)
	}

	// CPU counts replicas times the limit: 2 existing apps at the 100m default
	// is 200m, so 2 × 400m more is exactly 1000m and 2 × 401m is not.
	setQuota(t, s, id, Quota{CPUMillis: 1000})
	_, err = s.Create(ctx, id, CreateInput{
		Name: "wide", Image: "nginx:alpine", Replicas: 2, CPULimit: "401m",
	})
	wantQuotaRefusal(t, "2 × 401m on top of 200m against 1000m", err, "CPU")
	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "wide", Image: "nginx:alpine", Replicas: 2, CPULimit: "400m",
	}); err != nil {
		t.Fatalf("a create landing exactly on the quota was refused: %v", err)
	}

	// Zero in every field is the same as no quota.
	setQuota(t, s, id, Quota{})
	if _, err := s.Create(ctx, id, CreateInput{
		Name: "big", Image: "nginx:alpine", Replicas: 10, CPULimit: "2",
	}); err != nil {
		t.Fatalf("Create under an all-zero quota: %v", err)
	}
}

// The reason the check is inside the create's transaction. Two creates that
// each fit on their own must not both be admitted when together they do not.
func TestConcurrentCreatesCannotBothFit(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-quota-race")
	setQuota(t, s, id, Quota{Apps: 1})

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.Create(ctx, id, CreateInput{
				Name: "racer-" + string(rune('a'+i)), Image: "nginx:alpine", Replicas: 1,
			})
		}()
	}
	wg.Wait()

	admitted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case !errors.Is(err, ErrQuotaExceeded):
			t.Errorf("a racing create failed for another reason: %v", err)
		}
	}
	if n, _ := s.Count(ctx, id); admitted != 1 || n != 1 {
		t.Fatalf("%d creates admitted and %d apps stored against a quota of one", admitted, n)
	}
}

func TestScaleIsHeldToTheQuotaButScalingDownIsNot(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-quota-scale")

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 1, Port: 8080,
		CPULimit: "500m", MemoryLimit: "256Mi",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	setQuota(t, s, id, Quota{MemoryBytes: 512 << 20})

	if _, err := s.Scale(ctx, id, "web", 2); err != nil {
		t.Fatalf("scaling to exactly the memory quota: %v", err)
	}
	mustExecute(t, s, id, "web")
	_, err := s.Scale(ctx, id, "web", 3)
	wantQuotaRefusal(t, "a third replica", err, "memory")
	if a, _ := s.Get(ctx, id, "web"); a.Replicas != 2 {
		t.Fatalf("the refused scale left %d replicas, want 2", a.Replicas)
	}

	// The operator lowers the quota under what is running. Nothing is taken
	// down, and the way back under is open.
	setQuota(t, s, id, Quota{MemoryBytes: 128 << 20})
	if _, err := s.Scale(ctx, id, "web", 1); err != nil {
		t.Fatalf("scaling down while over the quota was refused: %v", err)
	}
}

func TestLimitChangesAreHeldToTheQuota(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-quota-update")

	if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
		Name: "web", Image: "nginx:1.26", Replicas: 1, Port: 8080, CPULimit: "500m",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	setQuota(t, s, id, Quota{CPUMillis: 1000})

	_, err := s.Update(ctx, id, "web", UpdateInput{Image: "nginx:1.26", Port: 8080, CPULimit: "1500m"})
	wantQuotaRefusal(t, "raising the limit past the quota", err, "CPU")

	// Leaving the limit blank is not a way around it: blank is the default,
	// which here is lower, so it is allowed — and counted.
	if _, err := s.Update(ctx, id, "web", UpdateInput{Image: "nginx:1.26", Port: 8080}); err != nil {
		t.Fatalf("dropping to the default limit: %v", err)
	}
	mustExecute(t, s, id, "web")
	if u, _ := s.TeamUsage(ctx, id); u.Usage.CPUMillis != defaultCPUMillis {
		t.Fatalf("an app with no limit is counted at %dm, want the %dm default",
			u.Usage.CPUMillis, defaultCPUMillis)
	}

	// Over a lowered quota, a change that does not raise anything still goes
	// through. A team cannot be stopped from shipping a fix because the
	// operator shrank its room.
	setQuota(t, s, id, Quota{CPUMillis: 50})
	if _, err := s.Update(ctx, id, "web", UpdateInput{Image: "nginx:1.27", Port: 8080}); err != nil {
		t.Fatalf("an image change while over the quota was refused: %v", err)
	}
	mustExecute(t, s, id, "web")
	if _, err := s.Update(ctx, id, "web", UpdateInput{Image: "nginx:1.27", Port: 8080, CPULimit: "50m"}); err != nil {
		t.Fatalf("lowering a limit while over the quota was refused: %v", err)
	}
}

func TestStorageIsHeldToTheQuota(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	id := owner(t, s, pool, "svc-quota-storage")

	for _, name := range []string{"web", "db"} {
		if _, err := createAndDeploy(t, s, ctx, id, CreateInput{
			Name: name, Image: "nginx:alpine", Replicas: 1, Port: 8080,
		}); err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
	}
	setQuota(t, s, id, Quota{StorageBytes: 2 << 30})

	if _, err := s.AttachVolume(ctx, id, "web", VolumeInput{
		Name: "data", MountPath: "/data", SizeBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("attaching 1 GiB of 2: %v", err)
	}
	if err := s.ResizeVolume(ctx, id, "web", "data", 2<<30); err != nil {
		t.Fatalf("growing to exactly the quota: %v", err)
	}
	err := s.ResizeVolume(ctx, id, "web", "data", 3<<30)
	wantQuotaRefusal(t, "growing past the quota", err, "storage")
	_, err = s.AttachVolume(ctx, id, "db", VolumeInput{
		Name: "data", MountPath: "/data", SizeBytes: 1 << 30,
	})
	wantQuotaRefusal(t, "a second volume past the quota", err, "storage")

	if u, _ := s.TeamUsage(ctx, id); u.Usage.StorageBytes != 2<<30 {
		t.Fatalf("storage committed = %d after two refusals, want the 2 GiB that was admitted",
			u.Usage.StorageBytes)
	}

	// Unlimited again, and the same growth goes through.
	setQuota(t, s, id, Quota{})
	if err := s.ResizeVolume(ctx, id, "web", "data", 3<<30); err != nil {
		t.Fatalf("growing with no quota: %v", err)
	}
}

// A stack is refused whole. Discovering the quota on its second app would
// leave a database running with nothing to talk to it.
func TestATemplateThatDoesNotFitIsRefusedWhole(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{Keeper: testKeeper(t)})
	id := owner(t, s, pool, "svc-quota-template")
	setQuota(t, s, id, Quota{Apps: 1})

	_, err := s.DeployTemplate(ctx, id, "postgres-app", "shop")
	wantQuotaRefusal(t, "a two-app stack against a quota of one", err, "apps")
	if n, _ := s.Count(ctx, id); n != 0 {
		t.Fatalf("the refused stack left %d apps behind", n)
	}
	projects, err := s.Projects(ctx, id)
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	for _, p := range projects {
		if p.Slug == "shop" {
			t.Fatal("the refused stack left its project behind")
		}
	}

	// Its database brings a volume, and that is counted as well.
	setQuota(t, s, id, Quota{StorageBytes: 512 << 20})
	_, err = s.DeployTemplate(ctx, id, "postgres-app", "shop")
	wantQuotaRefusal(t, "a stack whose database needs 1 GiB against 512 MiB", err, "storage")

	setQuota(t, s, id, Quota{Apps: 2, StorageBytes: 1 << 30})
	if _, err := s.DeployTemplate(ctx, id, "postgres-app", "shop"); err != nil {
		t.Fatalf("a stack that fits exactly: %v", err)
	}
}

// A release carries its replicas, so going back to one from before a scale
// down is a rise like any other.
func TestRollingBackIsHeldToTheQuota(t *testing.T) {
	ctx := context.Background()
	s, _, _, id, a, first := rollbackFixture(t, "svc-quota-rollback")

	// first ran two replicas at the default limit; the app now runs one.
	setQuota(t, s, id, Quota{CPUMillis: defaultCPUMillis})
	err := s.Rollback(ctx, id, a.Name, first.ID)
	wantQuotaRefusal(t, "rolling back to two replicas against room for one", err, "CPU")
	if got, _ := s.Get(ctx, id, a.Name); got.Replicas != 1 || got.Image != "nginx:1.27" {
		t.Fatalf("the refused rollback changed the app: %d replicas of %s", got.Replicas, got.Image)
	}

	setQuota(t, s, id, Quota{})
	if err := s.Rollback(ctx, id, a.Name, first.ID); err != nil {
		t.Fatalf("Rollback with no quota: %v", err)
	}
}

func TestTeamUsagesReadsEveryTeam(t *testing.T) {
	ctx := context.Background()
	s, _, pool := testService(t, Options{})
	a := owner(t, s, pool, "svc-quota-list-a")
	b := owner(t, s, pool, "svc-quota-list-b")

	if _, err := createAndDeploy(t, s, ctx, a, CreateInput{
		Name: "web", Image: "nginx:alpine", Replicas: 2, Port: 8080, CPULimit: "250m",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	setQuota(t, s, b, Quota{Apps: 3, CPUMillis: 2000})

	all, err := s.TeamUsages(ctx)
	if err != nil {
		t.Fatalf("TeamUsages: %v", err)
	}
	got := map[string]TeamUsage{}
	for _, u := range all {
		got[u.TeamID] = u
	}
	if u := got[a]; u.Usage.Apps != 1 || u.Usage.CPUMillis != 500 ||
		u.Usage.MemoryBytes != 2*defaultMemoryBytes || !u.Quota.Unlimited() || u.QuotaSetAt != nil {
		t.Errorf("team a = %+v, want one app committing 500m and 2 × the default memory, no quota", u)
	}
	if u := got[b]; u.Usage != (Usage{}) || u.Quota != (Quota{Apps: 3, CPUMillis: 2000}) || u.QuotaSetAt == nil {
		t.Errorf("team b = %+v, want nothing committed and its quota", u)
	}

	one, err := s.TeamUsage(ctx, a)
	if err != nil {
		t.Fatalf("TeamUsage: %v", err)
	}
	if one.Usage != got[a].Usage {
		t.Errorf("one team's usage %+v disagrees with the list's %+v", one.Usage, got[a].Usage)
	}
	if _, err := s.TeamUsage(ctx, "svc-quota-no-such-team"); !errors.Is(err, ErrNoSuchTeam) {
		t.Errorf("TeamUsage of a team that does not exist = %v, want ErrNoSuchTeam", err)
	}
	if err := s.SetQuota(ctx, "svc-quota-no-such-team", Quota{Apps: 1}); !errors.Is(err, ErrNoSuchTeam) {
		t.Errorf("SetQuota on a team that does not exist = %v, want ErrNoSuchTeam", err)
	}
	if err := s.SetQuota(ctx, a, Quota{Apps: -1}); err == nil {
		t.Error("a negative quota was stored")
	}
}
