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
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width">` +
		`<title>` + html.EscapeString(m.Subject) + `</title></head>` +
		`<body style="margin:0;padding:24px;background:#ffffff;` +
		`font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;` +
		`font-size:15px;line-height:1.5;color:#111111">` +
		`<div style="max-width:560px;margin:0 auto">`)
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
		b.WriteString("<p>" + strings.Join(lines, "<br>") + "</p>")
	}
	if lines := m.footerLines(); len(lines) > 0 {
		for i, l := range lines {
			lines[i] = linkify(l)
		}
		b.WriteString(`<hr style="border:0;border-top:1px solid #e5e5e5;margin:32px 0 16px">` +
			`<p style="font-size:12px;line-height:1.5;color:#666666">` +
			strings.Join(lines, "<br>") + `</p>`)
	}
	b.WriteString("</div></body></html>")
	return b.String()
}

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
		`<tr><td style="border-radius:6px;background:#111111">` +
		`<a href="` + esc + `" style="display:inline-block;padding:12px 20px;` +
		`font-weight:600;color:#ffffff;text-decoration:none;border-radius:6px">` +
		html.EscapeString(a.Label) + `</a></td></tr></table>` +
		`<p style="font-size:13px;color:#555555">Or open this address in your browser:<br>` +
		`<a href="` + esc + `" style="color:#555555;word-break:break-all">` + esc + `</a></p>`
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
		b.WriteString(`<a href="` + esc + `">` + esc + `</a>`)
		last = end
	}
	b.WriteString(html.EscapeString(line[last:]))
	return b.String()
}
