package gmail

import (
	"testing"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Gmail's attachments.get answers with bytes and a size and nothing else, so the filename and
// content type — which go straight onto the download link mailroom hands out — have to come
// from the message. They cannot be looked up by attachment id: Gmail answers two
// messages.get calls with different ids for the same parts, measured against a live mailbox.
// Size is what survives.
func TestUniqueRefBySize(t *testing.T) {
	refs := []mmail.AttachmentRef{
		{ID: "a", Filename: "invoice.pdf", MimeType: "application/pdf", Size: 33122},
		{ID: "b", Filename: "logo.png", MimeType: "image/png", Size: 4844},
		{ID: "c", Filename: "twin-one.bin", MimeType: "application/octet-stream", Size: 1024},
		{ID: "d", Filename: "twin-two.bin", MimeType: "application/octet-stream", Size: 1024},
	}

	for _, tc := range []struct {
		name string
		size int64
		want string
	}{
		{"the only part of that size", 33122, "invoice.pdf"},
		{"another unique size", 4844, "logo.png"},
		// The safety property. Labelling a download with the wrong attachment's name is
		// worse than leaving it unnamed, so an ambiguous match yields nothing.
		{"two parts share a size", 1024, ""},
		{"no part of that size", 999999, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := uniqueRefBySize(refs, tc.size)
			switch {
			case tc.want == "" && got != nil:
				t.Fatalf("want no match, got %q", got.Filename)
			case tc.want == "":
				return
			case got == nil:
				t.Fatalf("want %q, got no match", tc.want)
			case got.Filename != tc.want:
				t.Fatalf("want %q, got %q", tc.want, got.Filename)
			}
		})
	}
}

// An empty manifest is the ordinary case for a message whose attachment was fetched by an id
// from an earlier read, and must not panic or match.
func TestUniqueRefBySizeOnAnEmptyManifest(t *testing.T) {
	if got := uniqueRefBySize(nil, 100); got != nil {
		t.Fatalf("want no match from an empty manifest, got %+v", got)
	}
}
