package microsoft

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

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

// The conversation listing selected no body, so every message in a thread came back empty and
// mail_get_thread had no text and no links for any of them.
func TestAThreadAsksForEveryMessagesBody(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// Field by field: bodyPreview is in every listing, and a substring match on it is a
		// check that cannot fail.
		if !slices.Contains(strings.Split(r.URL.Query().Get("$select"), ","), "body") {
			t.Errorf("thread $select = %q, want it to include body", r.URL.Query().Get("$select"))
		}
		writeJSON(t, w, map[string]any{"value": []map[string]any{
			{"id": "m1", "conversationId": "c1", "body": map[string]any{"contentType": "html", "content": `<a href="https://example.com/1">One</a>`}},
			{"id": "m2", "conversationId": "c1", "body": map[string]any{"contentType": "html", "content": `<a href="https://example.com/2">Two</a>`}},
		}})
	})
	thread, err := p.GetThread(context.Background(), mmail.ScopedID{Account: "acct_1", Native: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 2 {
		t.Fatalf("thread has %d messages", len(thread.Messages))
	}
	for _, m := range thread.Messages {
		if m.Body.HTML == "" {
			t.Errorf("%s came back with no body", m.ID)
		}
	}
}
