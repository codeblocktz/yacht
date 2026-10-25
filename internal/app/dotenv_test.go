package app

import (
	"strings"
	"testing"
)

// A .env file as the tools people paste from write one.
func TestParseEnvReadsWhatPeoplePaste(t *testing.T) {
	got, err := ParseEnv(strings.Join([]string{
		"# a comment",
		"",
		"export PLAIN=value",
		"SPACED =  padded value  ",
		"COMMENTED=value # said after a space",
		"COLOUR=#fff",
		"URL=https://example.com/#anchor",
		`DOUBLE="a \"quoted\" line\nand a second # kept"`,
		"SINGLE='taken $as \\n written'",
		`MULTI="first`,
		`second"`,
		"EMPTY=",
		"REF=${{db.DATABASE_URL}}",
		"PLAIN=again",
		"WINDOWS=crlf\r",
	}, "\n"))
	if err != nil {
		t.Fatalf("ParseEnv: %v", err)
	}
	want := []EnvLine{
		{"PLAIN", "again"},
		{"SPACED", "padded value"},
		{"COMMENTED", "value"},
		{"COLOUR", "#fff"},
		{"URL", "https://example.com/#anchor"},
		{"DOUBLE", "a \"quoted\" line\nand a second # kept"},
		{"SINGLE", `taken $as \n written`},
		{"MULTI", "first\nsecond"},
		{"EMPTY", ""},
		{"REF", "${{db.DATABASE_URL}}"},
		{"WINDOWS", "crlf"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A line that is not a variable is refused with where it is.
func TestParseEnvSaysWhichLineIsWrong(t *testing.T) {
	for text, says := range map[string]string{
		"A=1\nnot a variable": "line 2",
		"1BAD=x":              "line 1",
		`A="never closed`:     "never closes",
		`A="closed" trailing`: "after the closing quote",
		"B=1\nA='open\nstill": "never closes",
	} {
		if _, err := ParseEnv(text); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("ParseEnv(%q) = %v, want it to say %q", text, err, says)
		}
	}
}

// What the raw editor shows reads back to the same values, and leaves the
// secrets out.
func TestFormatEnvRoundTrips(t *testing.T) {
	vars := []Variable{
		{Key: "A", Value: "plain"},
		{Key: "B", Value: "  spaced  "},
		{Key: "C", Value: "two\nlines"},
		{Key: "D", Value: `quote " and \ slash`},
		{Key: "E", Value: "x # y"},
		{Key: "F", Value: ""},
		{Key: "S", Secret: true},
	}
	text := FormatEnv(vars)
	if strings.Contains(text, "S=") {
		t.Errorf("a secret is in the raw text:\n%s", text)
	}
	back, err := ParseEnv(text)
	if err != nil {
		t.Fatalf("ParseEnv(FormatEnv): %v\n%s", err, text)
	}
	if len(back) != len(vars)-1 {
		t.Fatalf("read back %+v", back)
	}
	for i, l := range back {
		if l.Key != vars[i].Key || l.Value != vars[i].Value {
			t.Errorf("%s read back as %q, want %q", l.Key, l.Value, vars[i].Value)
		}
	}
}
