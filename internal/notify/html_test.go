package notify

import (
	"strings"
	"testing"
)

func TestTextToHTMLKeepsTheWordsAndLinksTheLinks(t *testing.T) {
	got := textToHTML("Follow this link to sign in:\n\nhttps://app.example.test/auth/abc?x=1&y=2.\n\nIgnore <this> if\nit was not you.")
	for _, want := range []string{
		`<p class="mail-ink" style="color:#111111">Follow this link to sign in:</p>`,
		`<p class="mail-ink" style="color:#111111"><a href="https://app.example.test/auth/abc?x=1&amp;y=2" style="color:#0b57d0">https://app.example.test/auth/abc?x=1&amp;y=2</a>.</p>`,
		`<p class="mail-ink" style="color:#111111">Ignore &lt;this&gt; if<br>it was not you.</p>`,
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
	if strings.Contains(got, "Invoice CB-2026-000001 for TZS 10,000.</p>") {
		t.Errorf("html draws the text as well as the content:\n%s", got)
	}
	if !strings.Contains(got, "hosting by Codeblock Limited") {
		t.Errorf("html lost the footer:\n%s", got)
	}
	if !strings.Contains(m.Text(), "Invoice CB-2026-000001 for TZS 10,000.") {
		t.Errorf("text lost the body:\n%s", m.Text())
	}
}

// A mail is white with dark text whatever the reader's client does in dark
// mode: it says it has only a light rendering, and has no dark one to switch
// to.
func TestHTMLIsAlwaysLight(t *testing.T) {
	got := Message{
		TextBody: "Sign in:\n\nhttps://app.example.test/auth/x",
		Action:   &Action{Label: "Sign in", URL: "https://app.example.test/auth/x"},
		Footer:   Footer{Brand: "datahuru", Website: "https://datahuru.test"},
	}.HTML()
	for _, want := range []string{
		`<meta name="color-scheme" content="light only">`,
		`<meta name="supported-color-schemes" content="light">`,
		`:root{color-scheme:light only`,
		`<body class="body mail-bg" bgcolor="#ffffff" style="margin:0;padding:0;background:#ffffff;`,
		`<table role="presentation" class="mail-bg" width="100%" cellspacing="0" cellpadding="0" border="0" bgcolor="#ffffff"`,
		`[data-ogsc] .mail-ink`, `[data-ogsb] .mail-bg`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("html lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "prefers-color-scheme") {
		t.Errorf("html has a dark-mode rule:\n%s", got)
	}
	// Every paragraph says its colour, so none is left to the client's.
	for _, p := range strings.Split(got, "<p")[1:] {
		if !strings.Contains(strings.SplitN(p, ">", 2)[0], "color:#") {
			t.Errorf("a paragraph with no colour of its own: <p%s", strings.SplitN(p, ">", 2)[0])
		}
	}
	for _, a := range strings.Split(got, "<a ")[1:] {
		if !strings.Contains(strings.SplitN(a, ">", 2)[0], "color:#") {
			t.Errorf("a link with no colour of its own: <a %s", strings.SplitN(a, ">", 2)[0])
		}
	}
}
