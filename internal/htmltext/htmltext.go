// Package htmltext reads an HTML message body the way an agent needs it: as words it can
// quote, and as the links those words point at.
//
// Mail is mostly HTML now, and the plain-text alternative a sender attaches is often an
// afterthought that keeps the anchor text and drops the address — "[Change of Ownership
// form]" with nowhere to go. The link is frequently the whole point of the message, so it has
// to survive the trip into a model's context even when the text part lost it.
package htmltext

import (
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Link is one anchor: what it said, and where it went. URL is the href exactly as the sender
// wrote it, after entity decoding and nothing else.
type Link struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

// Rendering is what one parse of an HTML body yields.
type Rendering struct {
	// Text is the body as readable text, with each link's target written after its words as
	// "text (url)" so that the two cannot drift apart in the reader's mind.
	Text string
	// Links lists every distinct target in document order, each with the first non-empty
	// text that pointed at it.
	Links []Link
}

// Render parses an HTML body once and returns both readings of it.
//
// A tracking redirect is returned as the redirect, not resolved. Following one is a request
// to a third party chosen by the sender, and for the links that matter most here — an
// unsubscribe, a confirmation, a one-time form — following it is the action itself. Reading
// a message must never do that on the reader's behalf.
func Render(src string) Rendering {
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		// html.Parse only fails on a reader error, which a strings.Reader never returns. Kept
		// rather than ignored so a change of input type cannot turn it into a silent panic.
		return Rendering{}
	}
	r := &renderer{seen: map[string]int{}}
	r.walk(doc)
	return Rendering{Text: tidy(r.out.String()), Links: r.links}
}

type renderer struct {
	out   strings.Builder
	links []Link
	seen  map[string]int // url -> index into links
	// pre counts enclosing <pre> elements, inside which whitespace is the content.
	pre int
	// breaks is the line breaks owed before the next words. Blocks ask for them rather than
	// write them, so that a row closing and the next one opening make one line break and not
	// two, however much nesting and whitespace sits between them.
	breaks int
}

// skipped elements carry no words a reader would see. Their text is code, metadata, or a
// template, and rendering it would put a stylesheet in the middle of a sentence.
var skipped = map[atom.Atom]bool{
	atom.Head: true, atom.Script: true, atom.Style: true, atom.Noscript: true,
	atom.Template: true, atom.Title: true, atom.Svg: true, atom.Object: true,
	atom.Iframe: true,
}

// breaksAround is how many line breaks an element puts before and after itself: one for an
// element that is a line, two for one that is a paragraph. Anything absent is inline and runs
// on, which is what keeps "<b>Total</b>: £12" on one line.
//
// A div is a line rather than a paragraph because that is how web clients compose: every
// line typed into Gmail is its own div, and spacing them as paragraphs double-spaces the
// whole message.
var breaksAround = map[atom.Atom]int{
	atom.Div: 1, atom.Tr: 1, atom.Li: 1, atom.Dt: 1, atom.Dd: 1, atom.Center: 1,
	atom.Figcaption: 1, atom.Form: 1, atom.Fieldset: 1,

	atom.P: 2, atom.Table: 2, atom.Ul: 2, atom.Ol: 2, atom.Dl: 2, atom.Blockquote: 2,
	atom.Pre: 2, atom.Hr: 2, atom.H1: 2, atom.H2: 2, atom.H3: 2, atom.H4: 2, atom.H5: 2,
	atom.H6: 2, atom.Address: 2, atom.Article: 2, atom.Aside: 2, atom.Figure: 2,
	atom.Footer: 2, atom.Header: 2, atom.Main: 2, atom.Nav: 2, atom.Section: 2,
}

func (r *renderer) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		r.text(n.Data)
		return
	case html.ElementNode:
		if skipped[n.DataAtom] {
			return
		}
		switch n.DataAtom {
		case atom.Br:
			r.write("")
			r.out.WriteString("\n")
			return
		case atom.Img:
			// An image carries no words of its own. Its alt text stands in for it only inside
			// a link, where an image-only button would otherwise leave the link nameless.
			return
		case atom.A:
			r.anchor(n)
			return
		case atom.Pre:
			r.pre++
			defer func() { r.pre-- }()
		}
	}

	breaks := 0
	if n.Type == html.ElementNode {
		breaks = breaksAround[n.DataAtom]
	}
	r.lineBreak(breaks)
	switch n.DataAtom {
	case atom.Li:
		r.write("- ")
	case atom.Td, atom.Th:
		r.write(" ")
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.walk(c)
	}
	r.lineBreak(breaks)
}

func (r *renderer) lineBreak(n int) { r.breaks = max(r.breaks, n) }

// write puts words on the page, paying any line breaks owed first.
func (r *renderer) write(s string) {
	if r.breaks > 0 {
		r.out.WriteString(strings.Repeat("\n", r.breaks))
		r.breaks = 0
	}
	r.out.WriteString(s)
}

func (r *renderer) text(s string) {
	if r.pre > 0 {
		r.write(s)
		return
	}
	s = collapse(s)
	if r.breaks > 0 && strings.TrimSpace(s) == "" {
		// Indentation between two blocks. Writing it would pay the breaks owed to the first
		// block and then owe the second its own, which is a blank line nobody wrote.
		return
	}
	r.write(s)
}

// anchor writes a link's words followed by its target, and records the pair.
func (r *renderer) anchor(n *html.Node) {
	href := strings.TrimSpace(attr(n, "href"))
	label := strings.TrimSpace(collapse(words(n)))

	if !followable(href) {
		// A fragment or a script is not somewhere a reader can go, so it is rendered as the
		// words alone and not listed.
		r.write(label)
		return
	}

	r.record(label, href)
	switch {
	case label == "":
		r.write(href)
	case sameTarget(label, href):
		r.write(label)
	default:
		r.write(label + " (" + href + ")")
	}
}

func (r *renderer) record(label, href string) {
	if i, ok := r.seen[href]; ok {
		if r.links[i].Text == "" {
			r.links[i].Text = label
		}
		return
	}
	r.seen[href] = len(r.links)
	r.links = append(r.links, Link{Text: label, URL: href})
}

// words is the visible text inside an element, with an image's alt standing in for the
// image. It is what an anchor is called.
func words(n *html.Node) string {
	var b strings.Builder
	var visit func(*html.Node)
	visit = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
		case n.Type == html.ElementNode && skipped[n.DataAtom]:
			return
		case n.Type == html.ElementNode && n.DataAtom == atom.Img:
			if alt := strings.TrimSpace(attr(n, "alt")); alt != "" {
				b.WriteString(" " + alt + " ")
			}
		case n.Type == html.ElementNode && n.DataAtom == atom.Br:
			b.WriteString(" ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			visit(c)
		}
	}
	visit(n)
	return b.String()
}

// followable reports whether an href names somewhere outside this document. Relative links
// are kept: mail has no base URL to resolve them against, but the path is still what the
// sender wrote and a reader may know where it belongs.
func followable(href string) bool {
	if href == "" || strings.HasPrefix(href, "#") {
		return false
	}
	u, err := url.Parse(href)
	if err != nil {
		return true
	}
	switch strings.ToLower(u.Scheme) {
	case "javascript", "data", "vbscript", "cid":
		return false
	}
	return true
}

// sameTarget is true when an anchor's words are already its address, as in a bare pasted
// link, so that rendering does not repeat it in brackets.
func sameTarget(label, href string) bool {
	trim := func(s string) string {
		s = strings.TrimSuffix(strings.ToLower(s), "/")
		for _, p := range []string{"mailto:", "https://", "http://", "www."} {
			s = strings.TrimPrefix(s, p)
		}
		return s
	}
	return trim(label) == trim(href)
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// collapse folds runs of whitespace to one space, as a browser does outside <pre>.
// Non-breaking spaces are folded too: mail templates use them as padding by the dozen.
func collapse(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\u00a0' {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// tidy trims every line and allows at most one blank line in a row. Layout tables nest
// blocks many deep, and each level contributes a newline that means nothing to a reader.
// Zero-width non-joiners go too: templates pad the hidden preview line with hundreds of them
// so that a mail client's preview stops before the body begins.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := true // drops leading blank lines
	for _, line := range lines {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\u200c", ""))
		if line == "" {
			if !blank {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// markup is the set of opening tags that mark a body as HTML when the provider does not say
// which it is. A whole document starts with <html>, but a fragment — which is what some APIs
// hand back for a message composed in a web client — starts wherever its first element does.
var markup = []string{"<html", "<body", "<div", "<p>", "<p ", "<br", "<table", "<a href", "<span", "<!doctype html"}

// LooksLikeHTML reports whether a body of unlabelled type is HTML.
func LooksLikeHTML(s string) bool {
	if len(s) > 4096 {
		s = s[:4096]
	}
	s = strings.ToLower(s)
	for _, tag := range markup {
		if strings.Contains(s, tag) {
			return true
		}
	}
	return false
}
