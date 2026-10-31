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
	b.WriteString(`<!doctype html><html lang="en" style="color-scheme:light only;` + White + `">` +
		`<head><meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width">` +
		lightOnlyHead +
		`<title>` + html.EscapeString(m.Subject) + `</title></head>` +
		`<body class="body mail-bg" id="body" bgcolor="#ffffff" style="margin:0;padding:0;` + White + `;` +
		`font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;` +
		`font-size:15px;line-height:1.5;color:#111111">` +
		// The background is set again on a table the width of the window:
		// a client that drops <body>'s style, as Gmail and Outlook do, keeps
		// a table cell's, and bgcolor is what Outlook's Word engine reads.
		`<table role="presentation" class="mail-bg" width="100%" cellspacing="0" cellpadding="0" border="0" ` +
		`bgcolor="#ffffff" style="width:100%;` + White + `">` +
		`<tr><td class="mail-bg mail-ink" bgcolor="#ffffff" style="padding:24px;` + White + `;color:#111111">` +
		`<div style="max-width:560px;margin:0 auto;color:#111111">`)
	if m.HTMLContent != "" {
		b.WriteString(m.HTMLContent)
	}
	// Text is written in runs, each of them Ink; the button, which must not
	// be inverted with them, stands between.
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(Ink(run.String()))
			run.Reset()
		}
	}
	for _, para := range m.paragraphs() {
		para = strings.Trim(para, "\n")
		if para == "" {
			continue
		}
		if a := m.Action; a != nil && a.URL != "" && strings.TrimSpace(para) == a.URL {
			flush()
			b.WriteString(button(*a))
			continue
		}
		lines := strings.Split(para, "\n")
		for i, l := range lines {
			lines[i] = linkify(l)
		}
		run.WriteString(`<p class="mail-ink" style="color:#111111">` + strings.Join(lines, "<br>") + "</p>")
	}
	flush()
	if lines := m.footerLines(); len(lines) > 0 {
		for i, l := range lines {
			lines[i] = linkify(l)
		}
		b.WriteString(Ink(`<hr class="mail-rule" style="border:0;border-top:1px solid #e5e5e5;margin:32px 0 16px">` +
			`<p class="mail-muted" style="font-size:12px;line-height:1.5;color:#666666">` +
			strings.Join(lines, "<br>") + `</p>`))
	}
	b.WriteString("</div></td></tr></table></body></html>")
	return b.String()
}

// White is a white background as an inline style: the colour, and the same
// white again as an image. The Gmail apps' dark mode recolours every
// background-color, whatever the mail says about colour schemes, but leaves
// background images alone, so a white gradient stays white beneath it. Use
// it on every element that must stay white.
const White = "background:#ffffff;background-image:linear-gradient(#ffffff,#ffffff)"

// Ink wraps HTML — text, its links and rules, but no images and no buttons
// — so that it keeps its own colours on a White background in the Gmail
// apps' dark mode.
//
// Gmail inverts text colours as it inverts backgrounds, and a light
// inversion of dark text on a background that stayed white is text nobody
// can read. Gmail's own selector, u + .body, which no other client matches,
// gives the two wrappers a black background and mix-blend-mode difference,
// and gives what is inside them its colours inverted (see lightOnlyHead):
//
//   - the inner wrapper is difference against the outer one. Light, it is
//     black on black and the text's inverted colour. Dark, Gmail has made
//     both backgrounds white and turned the text back to roughly its own
//     colour, and white less that is the inverted colour again. Either way
//     the group is the inverted text on black.
//   - the outer wrapper is difference against the White behind it: white
//     less black is white, and white less the inverted colour is the colour.
//
// So the text is drawn in its own colour, on white, whether Gmail inverted
// it or not. It is the trick for keeping a button's white label white
// (screen over difference, see button), turned for dark text on white: a
// screen against white is white, and would erase the text.
//
// An element inside Ink must take its colour from a class lightOnlyHead
// inverts — mail-ink, mail-muted, mail-soft, mail-link, mail-rule — or it is
// drawn inverted in Gmail. An image inside it would be drawn as a negative.
func Ink(content string) string {
	return `<div class="gmail-blend-invert"><div class="gmail-blend-difference">` + content + `</div></div>`
}

// lightOnlyHead keeps a mail white, and its text dark, whatever the reader's
// client does in dark mode. A mail is drawn once for every reader, light or
// dark, and a receipt or a sign-in link that turns black in some clients and
// not in others reads as two different senders.
//
// Three blocks, each for its own clients, as Gmail drops a whole <style>
// that has a selector it does not take, such as an attribute selector:
//
//   - Gmail's apps, told apart by the <u> they put before the body (u +
//     .body; u + #body in the versions that keep ids). The blend modes and
//     inverted colours of Ink, and the label of a Button (see there).
//   - color-scheme, for Apple Mail and the clients that follow it: the mail
//     has only a light rendering, so they leave it alone.
//   - Outlook.com, which marks what it recoloured with data-ogsc (text) and
//     data-ogsb (backgrounds): the light values back.
//
// There is deliberately no prefers-color-scheme rule: there is no dark
// version to switch to.
//
// HTMLContent may use the same classes: mail-bg for a white background
// (with White inline), and inside Ink mail-ink for body text (#111111),
// mail-muted for small print (#666666), mail-soft for a softer grey
// (#555555), mail-link for a link (#0b57d0; ink in Gmail) and mail-rule for
// a rule's border (#e5e5e5). In Gmail each is drawn in that grey, whatever
// its inline colour, so a sender's own shade of ink is #111111 there.
var lightOnlyHead = `<meta name="color-scheme" content="light only">` +
	`<meta name="supported-color-schemes" content="light">` +
	`<style>` + gmailCSS + `</style>` +
	`<style>:root{color-scheme:light only;supported-color-schemes:light}` +
	`html,body{background:#ffffff!important;color:#111111}</style>` +
	`<style>[data-ogsb] .mail-bg{background:#ffffff!important;background-color:#ffffff!important}` +
	`[data-ogsc] .mail-ink{color:#111111!important}` +
	`[data-ogsc] .mail-muted{color:#666666!important}` +
	`[data-ogsc] .mail-soft{color:#555555!important}` +
	`[data-ogsb] .mail-btn{background:#111111!important;background-color:#111111!important}` +
	`[data-ogsc] .mail-btn{color:#ffffff!important}</style>`

// gmailCSS is lightOnlyHead's block for the Gmail apps. Inside Ink each
// colour is its inverse, channel by channel, which Ink's second difference
// turns back. Only greys: Gmail's dark mode keeps a colour's hue as it
// inverts its lightness, which is a channel-by-channel inverse for a grey
// alone, so a blue link would come back orange. A link is ink there,
// underlined as every link is.
var gmailCSS = gmailRules([][2]string{
	{`.gmail-blend-invert,.gmail-blend-difference`, `background:#000000;mix-blend-mode:difference`},
	{`.gmail-blend-screen`, `background:#000000;mix-blend-mode:screen`},
	{`.gmail-blend-invert`, `color:#eeeeee`},
	{`.gmail-blend-invert .mail-ink`, `color:#eeeeee!important`},
	{`.gmail-blend-invert .mail-muted`, `color:#999999!important`},
	{`.gmail-blend-invert .mail-soft`, `color:#aaaaaa!important`},
	{`.gmail-blend-invert .mail-link`, `color:#eeeeee!important`},
	{`.gmail-blend-invert .mail-rule`, `border-color:#1a1a1a!important`},
})

// gmailRules writes each rule for both of Gmail's selectors.
func gmailRules(rules [][2]string) string {
	var b strings.Builder
	for _, r := range rules {
		var both []string
		for _, s := range strings.Split(r[0], ",") {
			both = append(both, "u+.body "+s, "u+#body "+s)
		}
		b.WriteString(strings.Join(both, ",") + "{" + r[1] + "}")
	}
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

// button is the action as the default button, near-black, with the address
// beneath it.
func button(a Action) string {
	return Button(a, "#111111")
}

// Button is a action as a link styled to look pressable — a table cell, as
// the one shape every mail client draws — in background, with its label in
// white, and the address beneath it as text, for whoever's client shows no
// button or who would rather see where it goes before pressing.
//
// The cell's colour is an image as well as a colour, as White is, so the
// Gmail apps leave it alone; the white label, which they would turn dark on
// it, is kept white by Gmail's blend trick: difference against black, then
// screen onto the button. The button is not to be put inside Ink.
func Button(a Action, background string) string {
	esc := html.EscapeString(a.URL)
	bg := html.EscapeString(background)
	// Outlook.com's restoring rule is for the default button's colour.
	class := ""
	if background == "#111111" {
		class = ` class="mail-btn"`
	}
	return `<table role="presentation" cellspacing="0" cellpadding="0" border="0" style="margin:24px 0">` +
		`<tr><td` + class + ` bgcolor="` + bg + `" style="border-radius:6px;background:` + bg +
		`;background-image:linear-gradient(` + bg + `,` + bg + `)">` +
		`<a` + class + ` href="` + esc + `" style="display:inline-block;padding:12px 20px;` +
		`font-weight:600;color:#ffffff;text-decoration:none;border-radius:6px">` +
		`<span class="gmail-blend-screen" style="display:inline-block">` +
		`<span class="gmail-blend-difference" style="display:inline-block;color:#ffffff">` +
		html.EscapeString(a.Label) + `</span></span></a></td></tr></table>` +
		Ink(`<p class="mail-soft" style="font-size:13px;color:#555555">Or open this address in your browser:<br>`+
			`<a class="mail-soft" href="`+esc+`" style="color:#555555;word-break:break-all">`+esc+`</a></p>`)
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
		b.WriteString(`<a class="mail-link" href="` + esc + `" style="color:#0b57d0">` + esc + `</a>`)
		last = end
	}
	b.WriteString(html.EscapeString(line[last:]))
	return b.String()
}
