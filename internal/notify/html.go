package notify

import (
	"html"
	"regexp"
	"strings"
)

// textToHTML is a message's plain text as a small HTML document: the same
// words, paragraph for paragraph, with every link a link.
//
// Messages stay text: this exists for the providers whose filters score a
// text-only message with a bare link as a phishing attempt, which is what a
// sign-in link from a new domain looks like to them. It adds no wording,
// images or tracking, so the two versions never disagree.
func textToHTML(text string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width"></head>` +
		`<body style="font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;` +
		`font-size:15px;line-height:1.5;color:#111">`)
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		para = strings.Trim(para, "\n")
		if para == "" {
			continue
		}
		lines := strings.Split(para, "\n")
		for i, l := range lines {
			lines[i] = linkify(l)
		}
		b.WriteString("<p>" + strings.Join(lines, "<br>") + "</p>")
	}
	b.WriteString("</body></html>")
	return b.String()
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
