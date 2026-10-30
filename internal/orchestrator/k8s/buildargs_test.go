package k8s

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/codeblocktz/yacht/internal/orchestrator"
)

// runBuildArgsScript runs the build's own script against a Dockerfile and a
// directory of variables, the way the Dockerfile step does, and returns what
// it printed and the arguments it left for the build.
//
// The script is run rather than read, because what matters is what a shell
// makes of it: whether a value with a newline in it is still one argument.
func runBuildArgsScript(t *testing.T, dockerfile string, vars map[string]string) (string, []string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run the build script with")
	}
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk to run the build script with")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	args := filepath.Join(dir, "args")
	for _, d := range []string{src, args} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	for k, v := range vars {
		if err := os.WriteFile(filepath.Join(args, k), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// What the build does with "$@", made visible: each argument ended by a
	// NUL, which no argument can contain.
	script := "dockerfile=Dockerfile\n" + buildArgsScript + `
: > "$OUT"; [ $# -eq 0 ] || printf '%s\0' "$@" > "$OUT"
`
	out := filepath.Join(dir, "out")
	cmd := exec.Command(sh, "-euc", script)
	cmd.Dir = src
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "BUILD_ARGS=" + args, "TMPDIR=" + dir, "OUT=" + out}
	printed, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, printed)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var flags []string
	if len(raw) > 0 {
		flags = strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	}
	return string(printed), flags
}

// declaredIn is the names the script reports for a Dockerfile.
func declaredIn(t *testing.T, dockerfile string) []string {
	t.Helper()
	printed, _ := runBuildArgsScript(t, dockerfile, nil)
	for _, line := range strings.Split(printed, "\n") {
		if names, ok := strings.CutPrefix(line, buildArgsMarker); ok {
			return strings.Fields(names)
		}
	}
	t.Fatalf("no %s line in:\n%s", buildArgsMarker, printed)
	return nil
}

func TestDeclaredArgsAreReadFromEveryStage(t *testing.T) {
	cases := []struct {
		name, dockerfile string
		want             []string
	}{
		{
			name:       "none",
			dockerfile: "FROM alpine\nRUN echo hi\n",
			want:       nil,
		},
		{
			name: "every stage, defaults, several on a line, any case",
			dockerfile: "ARG ELIXIR_VERSION=1.17\n" +
				"FROM elixir:${ELIXIR_VERSION} AS build\n" +
				"ARG MIX_ENV=prod APP_HOSTS\n" +
				"  arg   lower_case\n" +
				"FROM alpine\n" +
				"ARG APP_HOSTS\n" +
				"ARG PHX_HOST=\"example.com\"\n",
			want: []string{"ELIXIR_VERSION", "MIX_ENV", "APP_HOSTS", "lower_case", "PHX_HOST"},
		},
		{
			name: "a quoted default with spaces and what looks like another name",
			dockerfile: "FROM alpine\n" +
				"ARG GREETING=\"hello NOT_A_NAME=1\" REAL\n" +
				"ARG SINGLE='a b' AFTER\n",
			want: []string{"GREETING", "REAL", "SINGLE", "AFTER"},
		},
		{
			name: "comments, including inside a continuation",
			dockerfile: "# ARG COMMENTED\n" +
				"FROM alpine\n" +
				"ARG FIRST \\\n" +
				"# ARG ALSO_COMMENTED\n" +
				"    SECOND=2 \\\n" +
				"\n" +
				"    THIRD\n" +
				"RUN echo ARG NOT_AN_ARG\n",
			want: []string{"FIRST", "SECOND", "THIRD"},
		},
		{
			name: "the escape directive",
			dockerfile: "# escape=`\n" +
				"FROM alpine\n" +
				"ARG ONE `\n" +
				"    TWO\n" +
				"ARG PATHLIKE=C:\\dir\n",
			want: []string{"ONE", "TWO", "PATHLIKE"},
		},
		{
			name: "a heredoc's body is not an instruction",
			dockerfile: "FROM alpine\n" +
				"RUN <<EOF\n" +
				"ARG INSIDE_HEREDOC\n" +
				"EOF\n" +
				"COPY <<-\"ONE\" <<TWO /x\n" +
				"\tARG IN_ONE\n" +
				"\tONE\n" +
				"ARG IN_TWO\n" +
				"TWO\n" +
				"ARG AFTER_HEREDOCS\n",
			want: []string{"AFTER_HEREDOCS"},
		},
		{
			name:       "CRLF line endings",
			dockerfile: "FROM alpine\r\nARG WINDOWS \\\r\n  LINES\r\n",
			want:       []string{"WINDOWS", "LINES"},
		},
		{
			name:       "declared twice, listed once",
			dockerfile: "FROM a AS one\nARG TWICE\nFROM b\nARG TWICE=x\n",
			want:       []string{"TWICE"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := declaredIn(t, c.dockerfile)
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("declared %q, want %q", got, c.want)
			}
		})
	}
}

// A value reaches BuildKit exactly as it was written, as one argument, and
// only for a name the Dockerfile declares.
func TestBuildArgFlagsKeepValuesWholeAndPassOnlyWhatIsDeclared(t *testing.T) {
	vars := map[string]string{
		"SPACES":     "a b  c",
		"QUOTES":     `it's "quoted" $HOME ` + "`id`",
		"EQUALS":     "postgres://u:p@h/db?sslmode=require&x=a=b",
		"NEWLINES":   "-----BEGIN KEY-----\nabc\n-----END KEY-----\n\n",
		"EMPTY":      "",
		"UNDECLARED": "must not be passed",
	}
	dockerfile := "FROM alpine\nARG SPACES QUOTES EQUALS\nARG NEWLINES\nARG EMPTY\nARG MISSING\n"
	printed, flags := runBuildArgsScript(t, dockerfile, vars)

	want := []string{
		"--opt", "build-arg:SPACES=" + vars["SPACES"],
		"--opt", "build-arg:QUOTES=" + vars["QUOTES"],
		"--opt", "build-arg:EQUALS=" + vars["EQUALS"],
		"--opt", "build-arg:NEWLINES=" + vars["NEWLINES"],
		"--opt", "build-arg:EMPTY=",
	}
	if !reflect.DeepEqual(flags, want) {
		t.Errorf("flags:\n got %q\nwant %q", flags, want)
	}
	// The names, so somebody can see what the build was given, and never
	// a value.
	if !strings.Contains(printed, "Build arguments from this app's variables: SPACES QUOTES EQUALS NEWLINES EMPTY\n") {
		t.Errorf("the log does not name the arguments:\n%s", printed)
	}
	for k, v := range vars {
		if v != "" && strings.Contains(printed, v) {
			t.Errorf("the log shows the value of %s:\n%s", k, printed)
		}
	}
}

func TestNoVariablesMeansNoBuildArgs(t *testing.T) {
	_, flags := runBuildArgsScript(t, "FROM alpine\nARG A\n", nil)
	if len(flags) != 0 {
		t.Errorf("flags = %q, want none", flags)
	}
}

func TestTheBuildArgsMarkerIsReadAndKeptOutOfTheLog(t *testing.T) {
	log := "Building Dockerfile\n" + buildArgsMarker + " APP_HOSTS MIX_ENV\n#1 load\n"
	var (
		result orchestrator.BuildResult
		seen   markersSeen
		shown  string
	)
	scanBuildLog(strings.NewReader(log), func(s string) { shown += s }, &result, &seen)
	if !seen.args || !reflect.DeepEqual(result.BuildArgs, []string{"APP_HOSTS", "MIX_ENV"}) {
		t.Errorf("result %+v, seen %+v", result, seen)
	}
	if strings.Contains(shown, buildArgsMarker) {
		t.Errorf("the marker is in the log: %q", shown)
	}

	// Declaring nothing is an answer, and different from not saying.
	result, seen = orchestrator.BuildResult{}, markersSeen{}
	scanBuildLog(strings.NewReader(buildArgsMarker+"\n"), nil, &result, &seen)
	if !seen.args || result.BuildArgs == nil || len(result.BuildArgs) != 0 {
		t.Errorf("an empty declaration: %#v", result.BuildArgs)
	}
}

// The buildpack step gives every variable to the lifecycle and says so.
func TestBuildpacksGetEveryVariableAsPlatformEnv(t *testing.T) {
	script := buildpackStep(orchestrator.BuildRequest{Image: "r/x:1"}, true).Args[0]
	for _, want := range []string{
		`for f in "$BUILD_ARGS"/*; do`,
		`> "/workspace/platform/env/${f##*/}"`,
		buildArgsMarker + orchestrator.BuildArgsAll,
		"-platform=/workspace/platform",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the buildpack step does not have %q", want)
		}
	}
	// After the check that a Dockerfile already built, so a Dockerfile
	// build is not reported as one given everything.
	if strings.Index(script, buildArgsMarker) < strings.Index(script, dockerfileMarker) {
		t.Error("the buildpack step reports its arguments before it knows it is the one building")
	}
}

const testSecretValue = "s3cr3t-p4ssw0rd-VALUE"

// The variables reach the build as files from a Secret of the build's own,
// which goes with it: no value is in the Job, its env or its arguments, and
// the clone does not mount them.
func TestBuildVariablesReachTheBuildThroughASecret(t *testing.T) {
	o, client := testOrchestrator(t)
	var job *batchv1.Job
	var secret *corev1.Secret
	client.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		job = a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		return false, nil, nil
	})
	client.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if s := a.(k8stesting.CreateAction).GetObject().(*corev1.Secret); strings.HasSuffix(s.Name, "-args") {
			secret = s.DeepCopy()
		}
		return false, nil, nil
	})

	req := tokenBuild()
	req.RepoToken = ""
	req.Variables = map[string]string{
		"DATABASE_URL": "postgres://app:" + testSecretValue + "@db/app",
		"APP_HOSTS":    "example.com www.example.com",
		"MULTI":        "line one\nline two\n",
	}
	req.Redact = []string{"postgres://app:" + testSecretValue + "@db/app"}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := o.Build(ctx, req); err == nil {
		t.Fatal("a build with no pod succeeded")
	}
	if job == nil || secret == nil {
		t.Fatalf("job %v, secret %v — want both created", job != nil, secret != nil)
	}
	for k, v := range req.Variables {
		if string(secret.Data[k]) != v {
			t.Errorf("the Secret holds %q for %s", secret.Data[k], k)
		}
	}
	spec, _ := json.Marshal(job)
	for k, v := range req.Variables {
		if strings.Contains(string(spec), v) || strings.Contains(string(spec), testSecretValue) {
			t.Errorf("the value of %s is in the Job", k)
		}
	}

	pod := job.Spec.Template.Spec
	var vol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == buildArgsVolume {
			vol = &pod.Volumes[i]
		}
	}
	if vol == nil || vol.Secret == nil || vol.Secret.SecretName != secret.Name ||
		vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o440 {
		t.Fatalf("variables volume = %+v", vol)
	}
	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == buildArgsVolume {
				mounted = m.ReadOnly && m.MountPath == buildArgsPath
			}
		}
		if (c.Name != cloneContainer) != mounted {
			t.Errorf("%s mounts the variables: %t — only the two build steps may, read-only", c.Name, mounted)
		}
	}

	// Gone with the build.
	if _, err := client.CoreV1().Secrets(BuildNamespace).
		Get(context.Background(), secret.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the variables' Secret outlived the build: %v", err)
	}
}

// An app with no variables gets no Secret and no mount, and the script is
// the same either way.
func TestABuildWithNoVariablesHasNoVariablesVolume(t *testing.T) {
	req := tokenBuild()
	req.RepoToken = ""
	job := buildJob("build-x", req, buildRegistrySecret, "", "")
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == buildArgsVolume {
			t.Fatal("a build with no variables has a variables volume")
		}
	}
	step := job.Spec.Template.Spec.InitContainers[1]
	if !strings.Contains(step.Args[0], `"$@"`) {
		t.Error("the Dockerfile build does not pass on the arguments")
	}
}

func TestSecretVariablesAreRedactedFromTheLog(t *testing.T) {
	var got string
	logf := redactingLog(func(s string) { got += s }, "", testSecretValue, "on")
	logf("connecting with " + testSecretValue + " on port 5432\n")
	if got != "connecting with *** on port 5432\n" {
		t.Errorf("log = %q: want the secret hidden and a too-short value left alone", got)
	}
}

// BuildKit prints a RUN with its arguments expanded, and the log arrives a
// line at a time, so a secret of several lines is hidden line by line.
func TestAMultiLineSecretIsRedactedLineByLine(t *testing.T) {
	key := "-----BEGIN KEY-----\nMIIEabcdef\n-----END KEY-----\n"
	var got string
	logf := redactingLog(func(s string) { got += s }, key)
	for _, line := range strings.SplitAfter(`#7 RUN echo "`+key+`" > /k`, "\n") {
		logf(line)
	}
	for _, part := range strings.Split(strings.TrimSpace(key), "\n") {
		if strings.Contains(got, part) {
			t.Errorf("%q is in the log:\n%s", part, got)
		}
	}
}
