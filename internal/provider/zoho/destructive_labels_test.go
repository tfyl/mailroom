package zoho

import (
	"context"
	"net/http"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Moving a message into the Trash folder bins it exactly as Trash does, so this classification
// is the gate on that route. Getting it wrong does not merely duplicate the destructive
// capability, it removes it for anything reached through a label change.
func TestZohoClassifiesItsBinFolderByName(t *testing.T) {
	var listings int
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/folders") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		listings++
		writeEnvelope(t, w, []map[string]any{
			{"folderId": "1", "folderName": "Inbox", "isSystemFolder": true},
			{"folderId": "2", "folderName": "Trash", "isSystemFolder": true},
			{"folderId": "3", "folderName": "Spam", "isSystemFolder": true},
			{"folderId": "4", "folderName": "Receipts"},
		})
	})

	ctx := context.Background()
	for _, tc := range []struct {
		label mmail.LabelID
		want  mmail.LabelEffect
	}{
		{"folder:2", mmail.EffectTrash},
		{"folder:3", mmail.EffectSpam},
		{"folder:1", mmail.EffectFile},
		{"folder:4", mmail.EffectFile},
		// A label is a sticker: applying one adds to a message and moves nothing, so no label
		// is destructive whatever somebody has named it.
		{"label:99", mmail.EffectFile},
		{"malformed", mmail.EffectFile},
	} {
		got, err := p.EffectOfApplying(ctx, tc.label)
		if err != nil {
			t.Fatalf("classifying %q: %v", tc.label, err)
		}
		if got != tc.want {
			t.Errorf("applying %q classified as %q, want %q", tc.label, got, tc.want)
		}
	}

	// The folder listing is read once. A modify naming two folders should not fetch the same
	// list twice, and a classification on the hot path of every label change is worth keeping
	// to one request.
	if listings != 1 {
		t.Errorf("the folder listing was fetched %d times", listings)
	}
}

// A folder listing that cannot be read refuses the classification rather than answering
// "ordinary". A check that could not be made has not passed.
func TestZohoRefusesToClassifyWhenTheFolderListingFails(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := p.EffectOfApplying(context.Background(), "folder:2"); err == nil {
		t.Fatal("an unreadable folder listing classified a folder as ordinary")
	}
}
