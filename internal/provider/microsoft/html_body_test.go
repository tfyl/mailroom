package microsoft

import "testing"

// Graph returns one body, and for most mail it is HTML. It has to land in the HTML field so
// the links in it are found, not in the text field where they would be read as markup.
func TestGraphBodyIsFiledByItsContentType(t *testing.T) {
	p := testProvider(t, nil)

	html := p.convert(message{ID: "m1", Body: &itemBody{ContentType: "html", Content: `<a href="https://example.com/f">Form</a>`}})
	if html.Body.HTML == "" || html.Body.Text != "" {
		t.Errorf("an html body was filed as %+v", html.Body)
	}

	text := p.convert(message{ID: "m2", Body: &itemBody{ContentType: "text", Content: "plain"}})
	if text.Body.Text != "plain" || text.Body.HTML != "" {
		t.Errorf("a text body was filed as %+v", text.Body)
	}
}
