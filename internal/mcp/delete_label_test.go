package mcp

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// On three of the four providers a label is a folder, and deleting one takes the mail filed
// under it — permanently on IMAP, which has no bin. So deleting is gated on the effect, the
// same way applying is, rather than on the tool.
//
// The assertion that matters is what reached the provider. A refusal that still called
// DeleteLabel would read as a refusal and have destroyed the folder anyway.
func TestDeletingAFolderNeedsDestructiveAsWellAsLabels(t *testing.T) {
	work := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: &stubLabels{}})

	res, _, err := tools.handleLabels(grantOver(mail.CapLabels), nil, labelsArgs{
		Action: "delete", Account: "work", ID: "folder:Archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("deleting a folder on `labels` alone must be refused: %v", payload(t, res))
	}
	if len(work.deleted) != 0 {
		t.Fatalf("the folder was deleted despite the refusal: %v", work.deleted)
	}

	message := refusalMessage(t, res)
	if !strings.Contains(message, string(mail.CapDestructive)) {
		t.Errorf("the refusal should name `destructive` as what is missing: %s", message)
	}
	if !strings.Contains(message, "the mail filed under it") {
		t.Errorf("the refusal should say what is actually lost: %s", message)
	}
}

// The regression risk of the fix: Gmail's labels are tags, so deleting one must stay an
// ordinary `labels` operation. Gating on the tool rather than the effect would have broken it.
func TestDeletingATagStillNeedsOnlyLabels(t *testing.T) {
	work := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: &stubLabels{}})

	res, _, err := tools.handleLabels(grantOver(mail.CapLabels), nil, labelsArgs{
		Action: "delete", Account: "work", ID: "Label_17",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("deleting a tag destroys no mail and needs only `labels`: %v", payload(t, res))
	}
	if len(work.deleted) != 1 || work.deleted[0] != "Label_17" {
		t.Errorf("the tag should have been deleted, got %v", work.deleted)
	}
}

func TestDeletingAFolderWorksWithDestructive(t *testing.T) {
	work := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: &stubLabels{}})

	res, _, err := tools.handleLabels(grantOver(mail.CapLabels, mail.CapDestructive), nil, labelsArgs{
		Action: "delete", Account: "work", ID: "folder:Archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("a grant holding both should be allowed: %v", payload(t, res))
	}
	if len(work.deleted) != 1 {
		t.Errorf("the folder should have been deleted, got %v", work.deleted)
	}
}

// Under hold nothing is destroyed, which is the point: a connection whose destructive actions
// wait for a person must not delete a folder because the tool was named after labels.
func TestDeletingAFolderIsWithheldUnderHold(t *testing.T) {
	work := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work, archiveMailbox.ID: &stubLabels{}})

	ctx := grantOver(mail.CapLabels, mail.CapDestructive)
	g := grantFrom(ctx)
	g.Mode = grant.ModeHold

	res, _, err := tools.handleLabels(ctx, nil, labelsArgs{
		Action: "delete", Account: "work", ID: "folder:Archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("a held connection must not delete a folder: %v", payload(t, res))
	}
	if len(work.deleted) != 0 {
		t.Fatalf("the folder was deleted under hold: %v", work.deleted)
	}
	if message := refusalMessage(t, res); !strings.Contains(message, "holds destructive actions") {
		t.Errorf("the caller should be told it was held rather than refused outright: %s", message)
	}
}

// The backstop, which is the part that survives the next tool somebody writes: a DeleteLabel
// reaching a provider without the handler having asked is refused at the boundary. Promoting
// this method from the embedded manager is exactly what let a folder deletion through on
// nothing but `labels`.
func TestTheProviderBoundaryRefusesAnUnaskedFolderDelete(t *testing.T) {
	work := &stubLabels{}
	guarded := guardedLabels{LabelManager: work, acct: workMailbox}

	err := guarded.DeleteLabel(grantOver(mail.CapLabels), "folder:Archive")
	if err == nil {
		t.Fatal("the boundary should refuse a folder delete on `labels` alone")
	}
	if len(work.deleted) != 0 {
		t.Fatalf("the folder was deleted at the boundary: %v", work.deleted)
	}

	// And it must not stand in the way of the ordinary case.
	if err := guarded.DeleteLabel(grantOver(mail.CapLabels), "Label_17"); err != nil {
		t.Fatalf("deleting a tag should pass the boundary: %v", err)
	}
	if len(work.deleted) != 1 {
		t.Errorf("the tag should have reached the provider, got %v", work.deleted)
	}
}

// refusalMessage pulls the per-mailbox message out of a fanned-out result, which is where a
// refusal for one account lands rather than at the top level.
func refusalMessage(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	body := payload(t, res)
	accounts := accountsBlock(t, body)
	if m, ok := entry(t, accounts, "work")["message"].(string); ok {
		return m
	}
	top, _ := body["message"].(string)
	return top
}
