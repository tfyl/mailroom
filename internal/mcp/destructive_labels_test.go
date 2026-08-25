package mcp

import (
	"strings"
	"testing"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// The guard is a backstop for the tool that does not ask.
//
// handleModify asks first, because only a handler can turn a destructive change into a queued
// action. What these two check is the other half: a label change that reaches a provider
// without anybody having asked is refused rather than performed, so the next tool written
// against labelManager cannot reopen this by forgetting.
func TestTheLabelManagerRefusesADestructiveApplyNobodyAuthorized(t *testing.T) {
	work := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work})

	ctx := grantOver(mail.CapRead, mail.CapLabels)
	labels, err := tools.labelManager(ctx, workMailbox)
	if err != nil {
		t.Fatalf("building a label manager: %v", err)
	}

	ids := []mail.ScopedID{{Account: workMailbox.ID, Native: "m1"}}
	err = labels.ApplyLabels(ctx, ids, []mail.LabelID{"TRASH"}, nil)
	if err == nil {
		t.Fatal("a grant without `destructive` reached the provider with a trashing label")
	}
	var scope *mail.ScopeError
	if !asScope(err, &scope) || scope.Capability != mail.CapDestructive {
		t.Fatalf("the refusal did not name `destructive`: %v", err)
	}
	if len(work.applied) != 0 {
		t.Fatalf("the provider was called anyway: %v", work.applied)
	}

	// Ordinary filing goes straight through, which is what keeps `labels` an ordinary
	// capability rather than a privileged one.
	if err := labels.ApplyLabels(ctx, ids, []mail.LabelID{"Receipts"}, nil); err != nil {
		t.Fatalf("ordinary filing was refused: %v", err)
	}
	if len(work.applied) != 1 {
		t.Fatalf("ordinary filing did not reach the provider: %v", work.applied)
	}
}

// Under `hold` the capability is held and the guard still refuses, because reaching a provider
// at all means the handler did not queue the call. A mode with teeth cannot have a path that
// performs the action when the arrangement to hold it was skipped.
func TestTheLabelManagerRefusesADestructiveApplyUnderHold(t *testing.T) {
	work := &stubLabels{}
	tools := fanoutTools(byAccount{workMailbox.ID: work})

	ctx := grantOver(mail.CapRead, mail.CapLabels, mail.CapDestructive)
	g := grantFrom(ctx)
	g.Mode = grant.ModeHold

	labels, err := tools.labelManager(ctx, workMailbox)
	if err != nil {
		t.Fatalf("building a label manager: %v", err)
	}

	ids := []mail.ScopedID{{Account: workMailbox.ID, Native: "m1"}}
	err = labels.ApplyLabels(ctx, ids, []mail.LabelID{"TRASH"}, nil)
	if err == nil {
		t.Fatal("a grant in `hold` reached the provider with a trashing label")
	}
	if !strings.Contains(err.Error(), "nothing was done") {
		t.Errorf("the refusal does not say plainly that nothing happened: %v", err)
	}
	if len(work.applied) != 0 {
		t.Fatalf("the provider was called anyway: %v", work.applied)
	}
}
