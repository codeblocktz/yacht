package notify

import (
	"html"
	"regexp"
	"strings"
)

// HTML is the message's HTML part: its text, paragraph for paragraph, with
// every link a link, the action a button and the footer set small beneath.
//
// Messages stay text: this exists for the providers whose filters score a
// text-only message with a bare link as a phishing attempt, which is what a
// sign-in link from a new domain looks like to them. It adds no wording that
// Text lacks, and no images, tracking pixels or rewritten links, so the two
// parts never disagree and every link goes where it says.
func (m Message) HTML() string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en" style="color-scheme:light only;background:#ffffff">` +
		`<head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width">` +
		lightOnlyHead +
		`<title>` + html.EscapeString(m.Subject) + `</title></head>` +
		`<body class="body mail-bg" bgcolor="#ffffff" style="margin:0;padding:0;background:#ffffff;` +
		`font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;` +
		`font-size:15px;line-height:1.5;color:#111111">` +
		// The background is set again on a table the width of the window:
		// a client that drops <body>'s style, as Gmail and Outlook do, keeps
		// a table cell's, and bgcolor is what Outlook's Word engine reads.
		`<table role="presentation" class="mail-bg" width="100%" cellspacing="0" cellpadding="0" border="0" ` +
		`bgcolor="#ffffff" style="width:100%;background:#ffffff">` +
		`<tr><td class="mail-bg mail-ink" bgcolor="#ffffff" style="padding:24px;background:#ffffff;color:#111111">` +
		`<div style="max-width:560px;margin:0 auto;color:#111111">`)
	if m.HTMLContent != "" {
		b.WriteString(m.HTMLContent)
	}
	for _, para := range m.paragraphs() {
		para = strings.Trim(para, "\n")
		if para == "" {
			continue
		}
		if a := m.Action; a != nil && a.URL != "" && strings.TrimSpace(para) == a.URL {
			b.WriteString(button(*a))
			continue
		}
		lines := strings.Split(para, "\n")
		for i, l := range lines {
			lines[i] = linkify(l)
		}
		b.WriteString(`<p class="mail-ink" style="color:#111111">` + strings.Join(lines, "<br>") + "</p>")
	}
	if lines := m.footerLines(); len(lines) > 0 {
		for i, l := range lines {
			lines[i] = linkify(l)
		}
		b.WriteString(`<hr style="border:0;border-top:1px solid #e5e5e5;margin:32px 0 16px">` +
			`<p class="mail-muted" style="font-size:12px;line-height:1.5;color:#666666">` +
			strings.Join(lines, "<br>") + `</p>`)
	}
	b.WriteString("</div></td></tr></table></body></html>")
	return b.String()
}

// lightOnlyHead keeps a mail white, and its text dark, whatever the reader's
// client does in dark mode. A mail is drawn once for every reader, light or
// dark, and a receipt or a sign-in link that turns black in some clients and
// not in others reads as two different senders.
//
// The meta tags and color-scheme tell Apple Mail and the clients that follow
// it that the mail has only a light rendering, so they leave it alone. The
// clients that invert regardless are answered where they can be: Outlook.com
// marks what it recoloured with data-ogsc (text) and data-ogsb (backgrounds),
// and Gmail's apps can be told apart by the <u> they put before the body;
// both are given the light values back. There is deliberately no
// prefers-color-scheme rule: there is no dark version to switch to.
//
// HTMLContent may use the same classes: mail-bg for a white background,
// mail-ink for body text, mail-muted for the grey of small print.
const lightOnlyHead = `<meta name="color-scheme" content="light only">` +
	`<meta name="supported-color-schemes" content="light">` +
	`<style>:root{color-scheme:light only;supported-color-schemes:light}` +
	`html,body{background:#ffffff!important;color:#111111}` +
	`[data-ogsb] .mail-bg,u+.body .mail-bg{background:#ffffff!important;background-color:#ffffff!important}` +
	`[data-ogsc] .mail-ink,u+.body .mail-ink{color:#111111!important}` +
	`[data-ogsc] .mail-muted,u+.body .mail-muted{color:#666666!important}` +
	`[data-ogsb] .mail-btn,u+.body .mail-btn{background:#111111!important;background-color:#111111!important}` +
	`[data-ogsc] .mail-btn,u+.body .mail-btn{color:#ffffff!important}</style>`

// paragraphs is TextBody split for the HTML part — none when HTMLContent
// stands in for them.
func (m Message) paragraphs() []string {
	if m.HTMLContent != "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(m.TextBody, "\r\n", "\n"), "\n\n")
}

// textToHTML is kept for callers that have only text: a message with nothing
// but a body.
func textToHTML(text string) string {
	return Message{TextBody: text}.HTML()
}

// button is the action as a link styled to look pressable — a table cell, as
// the one shape every mail client draws — with the address beneath it as
// text, for whoever's client shows no button or who would rather see where it
// goes before pressing.
func button(a Action) string {
	esc := html.EscapeString(a.URL)
	return `<table role="presentation" cellspacing="0" cellpadding="0" border="0" style="margin:24px 0">` +
		`<tr><td class="mail-btn" bgcolor="#111111" style="border-radius:6px;background:#111111">` +
		`<a class="mail-btn" href="` + esc + `" style="display:inline-block;padding:12px 20px;` +
		`font-weight:600;color:#ffffff;text-decoration:none;border-radius:6px">` +
		html.EscapeString(a.Label) + `</a></td></tr></table>` +
		`<p class="mail-muted" style="font-size:13px;color:#555555">Or open this address in your browser:<br>` +
		`<a class="mail-muted" href="` + esc + `" style="color:#555555;word-break:break-all">` + esc + `</a></p>`
}

var linkRE = regexp.MustCompile(`https?://[^\s<>"]+`)

// linkify escapes a line and makes each URL in it an anchor. A URL is
// escaped as the text it is shown as and as the href it leads to, so a
// crafted one cannot leave the attribute.
func linkify(line string) string {
	var b strings.Builder
	last := 0
	for _, m := range linkRE.FindAllStringIndex(line, -1) {
		b.WriteString(html.EscapeString(line[last:m[0]]))
		u := strings.TrimRight(line[m[0]:m[1]], ".,;:!?)")
		end := m[0] + len(u)
		esc := html.EscapeString(u)
		// Coloured explicitly: a link left to the client is drawn in its
		// own colour, which in dark mode is a light blue on our white.
		b.WriteString(`<a href="` + esc + `" style="color:#0b57d0">` + esc + `</a>`)
		last = end
	}
	b.WriteString(html.EscapeString(line[last:]))
	return b.String()
}
