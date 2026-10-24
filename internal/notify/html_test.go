package notify

import (
	"strings"
	"testing"
)

func TestTextToHTMLKeepsTheWordsAndLinksTheLinks(t *testing.T) {
	got := textToHTML("Follow this link to sign in:\n\nhttps://app.example.test/auth/abc?x=1&y=2.\n\nIgnore <this> if\nit was not you.")
	for _, want := range []string{
		"<p>Follow this link to sign in:</p>",
		`<p><a href="https://app.example.test/auth/abc?x=1&amp;y=2">https://app.example.test/auth/abc?x=1&amp;y=2</a>.</p>`,
		"<p>Ignore &lt;this&gt; if<br>it was not you.</p>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("html lacks %q:\n%s", want, got)
		}
	}
}

// A URL cannot leave its attribute, however it is written.
func TestTextToHTMLCannotBeBrokenOutOf(t *testing.T) {
	got := textToHTML(`https://evil.test/"onmouseover="alert(1) and <script>x</script>`)
	if strings.Contains(got, `"onmouseover`) || strings.Contains(got, "<script>") {
		t.Errorf("html escapes nothing:\n%s", got)
	}
}
