package htmltext

import (
	"html"
	"regexp"
	"strings"
)

// bareURL matches an http(s) address written into plain text. It stops at whitespace and at
// the characters html.EscapeString introduces, so it runs safely over escaped text.
var bareURL = regexp.MustCompile(`https?://[^\s<>"']+`)

// FromText renders a plain-text body as the HTML alternative sent alongside it.
//
// A message with no HTML part is not neutral in a webmail client. Gmail opens a text-only
// draft in its plain-text compose mode, and a draft sent from that mode is hard-wrapped at
// about 70 columns, so every paragraph arrives chopped mid-sentence. Sending an HTML
// alternative keeps the client in its normal mode, and the reader's window decides where
// lines break.
//
// The rendering adds nothing the text did not say: every line break stays a line break,
// every character is escaped, and bare URLs become links because a text/plain reader would
// have linked them anyway.
func FromText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimRight(text, "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = linkify(html.EscapeString(line))
	}
	return `<div dir="ltr">` + strings.Join(lines, "<br>\n") + "</div>\n"
}

func linkify(escaped string) string {
	return bareURL.ReplaceAllStringFunc(escaped, func(u string) string {
		// Sentence punctuation after an address belongs to the sentence, not the address.
		trimmed := strings.TrimRight(u, ".,;:!?)")
		return `<a href="` + trimmed + `">` + trimmed + `</a>` + u[len(trimmed):]
	})
}
