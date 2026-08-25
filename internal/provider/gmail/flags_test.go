package gmail

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	mmail "github.com/tfyl/mailroom/internal/mail"
)

// Gmail keeps read state and the star as labels, so a flag update here is a batchModify. Only
// the labels the update names may appear in it.
//
// Gmail is the one provider where writing both flags every time would be expressible, and
// doing it anyway would still be wrong: a caller marking twenty messages read has not asked
// for the stars on them to be cleared, and would not be told they had been.
func TestAFlagUpdateModifiesOnlyTheLabelsItNames(t *testing.T) {
	var body struct {
		Ids            []string `json:"ids"`
		AddLabelIds    []string `json:"addLabelIds"`
		RemoveLabelIds []string `json:"removeLabelIds"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the batchModify: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	svc, err := gmail.NewService(ctx, option.WithHTTPClient(srv.Client()), option.WithEndpoint(srv.URL))
	if err != nil {
		t.Fatalf("building a stub gmail service: %v", err)
	}
	p := &Provider{svc: svc, account: mmail.Account{ID: "acct_1", Alias: "work"}}

	read := true
	if err := p.SetFlags(ctx, []mmail.ScopedID{{Account: "acct_1", Native: "m1"}},
		mmail.FlagUpdate{Read: &read}); err != nil {
		t.Fatalf("marking read: %v", err)
	}

	if len(body.RemoveLabelIds) != 1 || body.RemoveLabelIds[0] != "UNREAD" {
		t.Errorf("marking read is removing UNREAD, got remove=%v", body.RemoveLabelIds)
	}
	for _, l := range append(body.AddLabelIds, body.RemoveLabelIds...) {
		if l == "STARRED" {
			t.Errorf("the request touched the star on a call that said nothing about it: "+
				"add=%v remove=%v", body.AddLabelIds, body.RemoveLabelIds)
		}
	}
}
