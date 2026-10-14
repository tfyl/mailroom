package htmltext

import (
	"reflect"
	"strings"
	"testing"
)

// The case this package exists for: a button whose words survive into the text part and
// whose address does not. The link has to come out with its own name on it.
func TestAButtonKeepsItsAddress(t *testing.T) {
	r := Render(`<html><head><style>.btn{color:red}</style><title>Hi</title></head><body>
		<p>Please complete the
		<a class="btn" href="https://forms.example.com/change?id=1&amp;s=2">Change of Ownership form</a>
		by Friday.</p></body></html>`)

	want := []Link{{Text: "Change of Ownership form", URL: "https://forms.example.com/change?id=1&s=2"}}
	if !reflect.DeepEqual(r.Links, want) {
		t.Errorf("links = %#v, want %#v", r.Links, want)
	}
	wantText := "Please complete the Change of Ownership form (https://forms.example.com/change?id=1&s=2) by Friday."
	if r.Text != wantText {
		t.Errorf("text = %q, want %q", r.Text, wantText)
	}
	if strings.Contains(r.Text, "color") || strings.Contains(r.Text, "Hi") {
		t.Errorf("a stylesheet or title leaked into the text: %q", r.Text)
	}
}

// Templates repeat a link per section; the list names each target once, under the first
// words that gave it a name.
func TestLinksAreDedupedByTarget(t *testing.T) {
	r := Render(`<a href="https://example.com/a"><img src="cid:logo"></a>
		<a href="https://example.com/a">Open</a>
		<a href="https://example.com/b">Other</a>
		<a href="https://example.com/a">Open again</a>`)

	want := []Link{
		{Text: "Open", URL: "https://example.com/a"},
		{Text: "Other", URL: "https://example.com/b"},
	}
	if !reflect.DeepEqual(r.Links, want) {
		t.Errorf("links = %#v, want %#v", r.Links, want)
	}
}

// An image is not text, so it adds nothing to the body — except inside a link, where its alt
// is the only name an image-only button has.
func TestImagesAreIgnoredExceptAsALinksName(t *testing.T) {
	r := Render(`<p><img src="cid:banner" alt="Big banner">Hello</p>
		<a href="https://example.com/app"><img src="https://example.com/b.png" alt="Get the app"></a>`)

	if strings.Contains(r.Text, "Big banner") || strings.Contains(r.Text, "cid:") {
		t.Errorf("an inline image leaked into the text: %q", r.Text)
	}
	want := []Link{{Text: "Get the app", URL: "https://example.com/app"}}
	if !reflect.DeepEqual(r.Links, want) {
		t.Errorf("links = %#v, want %#v", r.Links, want)
	}
}

func TestOnlyFollowableTargetsAreListed(t *testing.T) {
	r := Render(`<a href="#top">Top</a> <a href="javascript:void(0)">Script</a>
		<a href="cid:part1">Part</a> <a href="data:text/plain,x">Data</a> <a>No href</a>
		<a href="mailto:help@example.com">help@example.com</a>`)

	want := []Link{{Text: "help@example.com", URL: "mailto:help@example.com"}}
	if !reflect.DeepEqual(r.Links, want) {
		t.Errorf("links = %#v, want %#v", r.Links, want)
	}
	// Words survive even when their target does not, and a bare address is not repeated.
	for _, word := range []string{"Top", "Script", "No href"} {
		if !strings.Contains(r.Text, word) {
			t.Errorf("text lost %q: %q", word, r.Text)
		}
	}
	if strings.Contains(r.Text, "(mailto:") {
		t.Errorf("an address written as its own text was repeated: %q", r.Text)
	}
}

func TestLayoutBecomesLines(t *testing.T) {
	r := Render(`<table><tr><td>Item</td><td>£12.00</td></tr><tr><td>Total</td><td>£12.00</td></tr></table>
		<ul><li>one</li><li>two</li></ul><p>a<br>b</p><script>alert(1)</script>`)

	want := "Item £12.00\nTotal £12.00\n\n- one\n- two\n\na\nb"
	if r.Text != want {
		t.Errorf("text = %q, want %q", r.Text, want)
	}
}

// Without a link there is nothing to list, and the list is empty rather than absent.
func TestNoLinks(t *testing.T) {
	if r := Render("<p>plain</p>"); len(r.Links) != 0 || r.Text != "plain" {
		t.Errorf("got %#v", r)
	}
}

func TestLooksLikeHTML(t *testing.T) {
	for in, want := range map[string]bool{
		"<html><body>x</body></html>":        true,
		"<div dir=\"ltr\">Hi<br></div>":      true,
		"<P>Shouted markup</P>":              true,
		"Hello,\n\nsee you at 5 <tomorrow>.": false,
		"a < b and b > c":                    false,
	} {
		if got := LooksLikeHTML(in); got != want {
			t.Errorf("LooksLikeHTML(%q) = %v, want %v", in, got, want)
		}
	}
}
