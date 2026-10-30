package htmltext

import "testing"

func TestFromTextKeepsLineBreaksAndEscapes(t *testing.T) {
	got := FromText("Hi <Sam> & co,\r\n\r\nSee you soon.\n")
	want := "<div dir=\"ltr\">Hi &lt;Sam&gt; &amp; co,<br>\n<br>\nSee you soon.</div>\n"
	if got != want {
		t.Errorf("FromText = %q, want %q", got, want)
	}
}

func TestFromTextLinksBareURLs(t *testing.T) {
	got := FromText("Key: https://share.example.com/s/abc?x=1&y=2#frag. Thanks")
	want := "<div dir=\"ltr\">Key: <a href=\"https://share.example.com/s/abc?x=1&amp;y=2#frag\">" +
		"https://share.example.com/s/abc?x=1&amp;y=2#frag</a>. Thanks</div>\n"
	if got != want {
		t.Errorf("FromText = %q, want %q", got, want)
	}
}

// The rendering must round-trip through the reader: what an agent reads back from a draft it
// wrote should be the text it wrote.
func TestFromTextRoundTripsThroughRender(t *testing.T) {
	text := "Hi,\n\nLine one\nline two https://example.com/a\n\nBye"
	if got := Render(FromText(text)).Text; got != "Hi,\n\nLine one\nline two https://example.com/a\n\nBye" {
		t.Errorf("round trip = %q", got)
	}
}
