package notify

import (
	"regexp"
	"strings"
	"testing"
)

func TestTextToHTMLKeepsTheWordsAndLinksTheLinks(t *testing.T) {
	got := textToHTML("Follow this link to sign in:\n\nhttps://app.example.test/auth/abc?x=1&y=2.\n\nIgnore <this> if\nit was not you.")
	for _, want := range []string{
		`<p class="mail-ink" style="color:#111111">Follow this link to sign in:</p>`,
		`<p class="mail-ink" style="color:#111111"><a class="mail-link" href="https://app.example.test/auth/abc?x=1&amp;y=2" style="color:#0b57d0">https://app.example.test/auth/abc?x=1&amp;y=2</a>.</p>`,
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
		`<body class="body mail-bg" id="body" bgcolor="#ffffff" style="margin:0;padding:0;background:#ffffff;background-image:linear-gradient(#ffffff,#ffffff);`,
		`<table role="presentation" class="mail-bg" width="100%" cellspacing="0" cellpadding="0" border="0" bgcolor="#ffffff" style="width:100%;background:#ffffff;background-image:linear-gradient(#ffffff,#ffffff)">`,
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

// The Gmail apps' dark mode recolours backgrounds and text whatever the mail
// says. Every white surface is a white image too, which it leaves alone; the
// text is inside Ink, whose blend modes give it back its colour; the button
// is outside Ink, its colour an image, its label kept white.
func TestHTMLSurvivesGmailsDarkMode(t *testing.T) {
	got := Message{
		TextBody: "Sign in:\n\nhttps://app.example.test/auth/x\n\nThanks.",
		Action:   &Action{Label: "Sign in", URL: "https://app.example.test/auth/x"},
		Footer:   Footer{Brand: "datahuru", Website: "https://datahuru.test"},
	}.HTML()
	white := "background-image:linear-gradient(#ffffff,#ffffff)"
	for _, tag := range []string{"<html ", "<body ", `<table role="presentation" class="mail-bg"`, `<td class="mail-bg`} {
		i := strings.Index(got, tag)
		if i < 0 || !strings.Contains(strings.SplitN(got[i:], ">", 2)[0], white) {
			t.Errorf("%s is not white as an image", tag)
		}
	}
	// Gmail's block is a <style> of its own: Gmail drops a whole block over
	// a selector it does not take, as Outlook's attribute selectors are.
	gmail := regexp.MustCompile(`<style>(u\+\.body [^<]*)</style>`).FindStringSubmatch(got)
	if gmail == nil || strings.Contains(gmail[1], "[data-") || strings.Contains(gmail[1], ":root") {
		t.Fatalf("no <style> of Gmail's own:\n%s", got)
	}
	for _, want := range []string{
		"u+.body .gmail-blend-invert,u+#body .gmail-blend-invert,u+.body .gmail-blend-difference,u+#body .gmail-blend-difference{background:#000000;mix-blend-mode:difference}",
		"u+.body .gmail-blend-screen,u+#body .gmail-blend-screen{background:#000000;mix-blend-mode:screen}",
		"u+.body .gmail-blend-invert .mail-ink,u+#body .gmail-blend-invert .mail-ink{color:#eeeeee!important}",
	} {
		if !strings.Contains(gmail[1], want) {
			t.Errorf("Gmail's block lacks %q", want)
		}
	}
	// Every paragraph is inside Ink; the button is not, and is near-black as
	// an image, with its label blended white.
	for _, para := range []string{"Sign in:</p>", "Thanks.</p>", "Or open this address", "datahuru<br>"} {
		i := strings.Index(got, para)
		open := strings.LastIndex(got[:i], `<div class="gmail-blend-invert"><div class="gmail-blend-difference">`)
		if i < 0 || open < 0 || strings.Contains(got[open:i], "</div></div>") {
			t.Errorf("%q is not inside Ink", para)
		}
	}
	btn := strings.Index(got, `<td class="mail-btn"`)
	if btn < 0 || strings.LastIndex(got[:btn], `<div class="gmail-blend-invert">`) > strings.LastIndex(got[:btn], "</div></div>") {
		t.Fatal("the button is inside Ink, which would draw it as a negative")
	}
	for _, want := range []string{
		`background:#111111;background-image:linear-gradient(#111111,#111111)`,
		`<span class="gmail-blend-screen" style="display:inline-block"><span class="gmail-blend-difference" style="display:inline-block;color:#ffffff">Sign in</span></span>`,
	} {
		if !strings.Contains(got[btn:], want) {
			t.Errorf("the button lacks %q", want)
		}
	}
	// Each class Ink's text uses is given its inverse in Gmail; an element
	// inside Ink with a colour and no such class would be drawn inverted.
	for _, block := range regexp.MustCompile(`(?s)<div class="gmail-blend-difference">(.*?)</div></div>`).FindAllStringSubmatch(got, -1) {
		for _, el := range regexp.MustCompile(`<(p|a|hr|span|td)\b[^>]*>`).FindAllString(block[1], -1) {
			if strings.Contains(el, "color:") && !regexp.MustCompile(`class="mail-(ink|muted|soft|link|rule)"`).MatchString(el) {
				t.Errorf("inside Ink with a colour Gmail does not invert back: %s", el)
			}
		}
	}
}
