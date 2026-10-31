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

// HTMLContent stands in for the paragraphs drawn from the text, and the
// install's footer still follows it; the text part is untouched.
func TestHTMLContentReplacesTheParagraphsNotTheFooter(t *testing.T) {
	m := Message{
		TextBody:    "Invoice CB-2026-000001 for TZS 10,000.",
		HTMLContent: `<table><tr><td>Invoice CB-2026-000001</td></tr></table>`,
		Footer:      Footer{Brand: "datahuru", Company: "hosting by Codeblock Limited"},
	}
	got := m.HTML()
	if !strings.Contains(got, m.HTMLContent) {
		t.Errorf("html lacks the content:\n%s", got)
	}
	if strings.Contains(got, "<p>Invoice CB-2026-000001 for TZS 10,000.</p>") {
		t.Errorf("html draws the text as well as the content:\n%s", got)
	}
	if !strings.Contains(got, "hosting by Codeblock Limited") {
		t.Errorf("html lost the footer:\n%s", got)
	}
	if !strings.Contains(m.Text(), "Invoice CB-2026-000001 for TZS 10,000.") {
		t.Errorf("text lost the body:\n%s", m.Text())
	}
}
