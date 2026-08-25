package microsoft

import (
	"context"
	"net/http"
	"strings"
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Graph's folders are opaque ids, so the well-known bins have to be resolved before a caller's
// folder id can be compared with them. Moving mail into Deleted Items is, request for request,
// what Trash does.
func TestMicrosoftClassifiesItsBinFolders(t *testing.T) {
	var resolutions int
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		resolutions++
		switch {
		case strings.HasSuffix(r.URL.Path, "/mailFolders/"+deletedItems):
			writeJSON(t, w, map[string]any{"id": "AAMkBIN="})
		case strings.HasSuffix(r.URL.Path, "/mailFolders/"+junkEmail):
			writeJSON(t, w, map[string]any{"id": "AAMkJUNK="})
		default:
			// Recoverable Items is not present on every mailbox, and a bin the mailbox does
			// not have must not fail every classification on it.
			w.WriteHeader(http.StatusNotFound)
		}
	})

	ctx := context.Background()
	for _, tc := range []struct {
		label mmail.LabelID
		want  mmail.LabelEffect
	}{
		// The mailbox's own ids, as ListLabels hands them out.
		{folderLabel("AAMkBIN="), mmail.EffectTrash},
		{folderLabel("AAMkJUNK="), mmail.EffectSpam},
		{folderLabel("AAMkPROJECT="), mmail.EffectFile},
		// The well-known names, which move and copy accept in place of an id and which
		// CreateFilter already writes for a rule that deletes.
		{folderLabel(deletedItems), mmail.EffectTrash},
		{folderLabel(junkEmail), mmail.EffectSpam},
		// A category is a sticker: applying one moves nothing.
		{categoryLabel("Deleted Items"), mmail.EffectFile},
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

	// The bins are resolved once per provider: three well-known names, and then nothing.
	if resolutions != len(binFolders) {
		t.Errorf("the well-known folders were resolved in %d requests, want %d",
			resolutions, len(binFolders))
	}
}

// A resolution that fails for any reason other than the folder being absent refuses the
// classification. Answering "ordinary" because Graph was briefly unreachable would let a
// throttle become a way past the capability.
func TestMicrosoftRefusesToClassifyWhenGraphFails(t *testing.T) {
	p := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	if _, err := p.EffectOfApplying(context.Background(), folderLabel("AAMkBIN=")); err == nil {
		t.Fatal("an unreachable Graph classified a folder as ordinary")
	}
}
