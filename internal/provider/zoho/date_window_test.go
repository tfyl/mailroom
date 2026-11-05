package zoho

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// The live failure: "assessment after:2026-11-01" against a Zoho mailbox answered ok with
// nothing, while "assessment" alone answered 25 results including one from 4 November. A
// date bound Zoho does not apply the way it was asked to is reported as an empty success,
// so the bound is now applied here, to mail Zoho returned for the keyword alone.
func TestZohoServesDateBoundsInTheGateway(t *testing.T) {
	day := func(d int) string {
		return strconv.FormatInt(time.Date(2026, 11, d, 12, 0, 0, 0, time.UTC).UnixMilli(), 10)
	}
	var keys []string
	p := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.URL.Query().Get("searchKey"))
		// Newest first, as both endpoints answer, running back past the bound.
		writeEnvelope(t, w, []map[string]any{
			{"messageId": "4", "folderId": "1", "subject": "Self Assessment letter", "receivedTime": day(4)},
			{"messageId": "2", "folderId": "1", "subject": "assessment", "receivedTime": day(2)},
			{"messageId": "0", "folderId": "1", "subject": "assessment", "receivedTime": strconv.FormatInt(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC).UnixMilli(), 10)},
		})
	})

	page, err := p.Search(context.Background(), mmail.Query{
		Raw:    "assessment",
		After:  time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Before: time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC),
		Limit:  3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	if len(keys) != 1 {
		t.Fatalf("the listing ran back past After on the first page, so one request should do; made %d", len(keys))
	}
	if keys[0] != "entire:assessment" {
		t.Errorf("searchKey must carry the keyword and no date: got %q", keys[0])
	}
	if strings.Contains(keys[0], "Date") {
		t.Errorf("a date went to Zoho: %q", keys[0])
	}
	if len(page.Items) != 1 || page.Items[0].Subject != "assessment" {
		t.Fatalf("want only the 2 November message inside [1 Nov, 3 Nov), got %+v", page.Items)
	}
	if page.Cursor != "" {
		t.Errorf("past After on a newest-first listing there is nothing more to find; cursor %q", page.Cursor)
	}
}
