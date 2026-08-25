package mcp

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/mail"
)

// listTools reads the tool list over the real protocol, keyed by name, so every assertion
// below is about what a client receives rather than about a map in this package.
func listTools(t *testing.T, g *grant.Grant) map[string]*mcp.Tool {
	t.Helper()
	session, err := connect(t, g)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	out := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		out[tool.Name] = tool
	}
	return out
}

func fullGrant(mode grant.Mode) *grant.Grant {
	g := readGrant(mail.AllCapabilities...)
	g.Mode = mode
	return g
}

// TestEveryToolIsAnnotated asserts the hints one tool at a time, because that is the only way
// this can be asserted at all.
//
// A blanket "every tool has annotations" check passes on a table that says every tool is
// read-only, which is the failure worth catching: a wrong hint is a claim a client acts on
// without reading anything. The expectations here are written out rather than derived from
// toolAnnotations, so changing a hint means changing this table too — which is the point.
func TestEveryToolIsAnnotated(t *testing.T) {
	want := map[string]struct {
		title       string
		readOnly    bool
		destructive bool
		idempotent  bool
		openWorld   bool
	}{
		"mail_accounts":       {"List reachable mailboxes", true, false, true, true},
		"mail_search":         {"Search mail", true, false, true, true},
		"mail_get_message":    {"Read one message", true, false, true, true},
		"mail_get_thread":     {"Read a conversation", true, false, true, true},
		"mail_get_attachment": {"Download an attachment", false, false, false, true},
		"mail_upload_url":     {"Stage a file to attach", false, false, false, false},
		"mail_labels":         {"List, create or delete labels", false, true, false, true},
		"mail_draft":          {"Write, update or delete a draft", false, true, false, true},
		"mail_send":           {"Send mail", false, true, false, true},
		"mail_modify":         {"Label, archive, star, mark read or bin", false, true, false, true},
		"mail_trash":          {"Trash, restore or delete permanently", false, true, false, true},
		"mail_filters":        {"List, create or delete filters", false, true, false, true},
		"mail_settings":       {"Read settings; set the vacation responder", false, true, false, true},
	}

	tools := listTools(t, fullGrant(grant.ModeConfirm))
	if len(tools) != len(want) {
		t.Fatalf("a grant holding every capability was offered %d tools; this table covers %d: %v",
			len(tools), len(want), sortedNames(tools))
	}

	for name, tool := range tools {
		expect, ok := want[name]
		if !ok {
			t.Errorf("%s is registered and this test says nothing about what it does", name)
			continue
		}
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s reached a client with no annotations at all", name)
			continue
		}
		if a.Title != expect.title {
			t.Errorf("%s: title is %q, want %q", name, a.Title, expect.title)
		}
		if a.ReadOnlyHint != expect.readOnly {
			t.Errorf("%s: readOnlyHint is %v, want %v", name, a.ReadOnlyHint, expect.readOnly)
		}
		if a.DestructiveHint == nil || *a.DestructiveHint != expect.destructive {
			t.Errorf("%s: destructiveHint is %v, want %v", name, a.DestructiveHint, expect.destructive)
		}
		if a.IdempotentHint != expect.idempotent {
			t.Errorf("%s: idempotentHint is %v, want %v", name, a.IdempotentHint, expect.idempotent)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint != expect.openWorld {
			t.Errorf("%s: openWorldHint is %v, want %v", name, a.OpenWorldHint, expect.openWorld)
		}
	}
}

// TestNoToolFallsBackToTheCautiousDefault. annotationsFor answers for an unknown name so that
// a tool registered without a row is never shipped with a confident wrong hint — but the
// fallback is a safety net rather than a place for a tool to live, and a tool that reaches it
// is one nobody decided about.
func TestNoToolFallsBackToTheCautiousDefault(t *testing.T) {
	for name := range listTools(t, fullGrant(grant.ModeConfirm)) {
		if _, ok := toolAnnotations[name]; !ok {
			t.Errorf("%s is registered with no row in toolAnnotations, so it ships as "+
				"destructive-and-open-world by default rather than by decision", name)
		}
	}
}

// TestNoOrphanAnnotations catches the other direction: a row describing a tool that no longer
// exists is a decision about nothing, and reads as coverage this file does not have.
func TestNoOrphanAnnotations(t *testing.T) {
	tools := listTools(t, fullGrant(grant.ModeConfirm))
	for name := range toolAnnotations {
		if _, ok := tools[name]; !ok {
			t.Errorf("toolAnnotations has a row for %s, which no grant is offered", name)
		}
	}
}

// TestAnnotationsDoNotVaryByMode is the assertion behind the decision in annotations.go.
//
// Under `hold` a send reaches no provider, so it would be easy to annotate that connection's
// mail_send as harmless. It must not be: annotations are the half a client acts on without
// judgement, a tool list is cacheable, and a grant's mode can be edited after one is cached —
// so a hint that says "safe" under `hold` is a hint that outlives the mode it was true for.
// The mode belongs in the description, which this test checks does still move.
func TestAnnotationsDoNotVaryByMode(t *testing.T) {
	base := listTools(t, fullGrant(grant.ModeUnattended))

	for _, mode := range []grant.Mode{grant.ModeConfirm, grant.ModeHold} {
		got := listTools(t, fullGrant(mode))
		for name, tool := range got {
			if !reflect.DeepEqual(tool.Annotations, base[name].Annotations) {
				t.Errorf("%s under %s is annotated %+v, and under unattended %+v",
					name, mode, tool.Annotations, base[name].Annotations)
			}
		}
	}

	// The control: the descriptions do change, so an identical-annotations result is a
	// decision rather than a test that compared nothing.
	unattended := base["mail_send"].Description
	held := listTools(t, fullGrant(grant.ModeHold))["mail_send"].Description
	if unattended == held {
		t.Fatal("mail_send reads identically under unattended and hold, so this test is " +
			"comparing two copies of the same registration")
	}
}

// TestReadToolsAreAnnotatedReadOnly states the property separately from the table, because it
// is the one a client is most likely to act on: these are the tools it may run unattended.
func TestReadToolsAreAnnotatedReadOnly(t *testing.T) {
	tools := listTools(t, fullGrant(grant.ModeConfirm))
	reads := []string{"mail_accounts", "mail_search", "mail_get_message", "mail_get_thread"}
	for _, name := range reads {
		if !tools[name].Annotations.ReadOnlyHint {
			t.Errorf("%s is not annotated read-only", name)
		}
	}

	// And the ones that read mail but are not reads. mail_get_attachment copies bytes onto
	// this server and mints a token-free URL for them; mail_upload_url reserves space and
	// hands out a write credential. A client that ran either freely would be spending the
	// owner's storage allowance on the strength of a wrong hint.
	for _, name := range []string{"mail_get_attachment", "mail_upload_url"} {
		if tools[name].Annotations.ReadOnlyHint {
			t.Errorf("%s is annotated read-only; it writes to this server's store", name)
		}
		if tools[name].Annotations.IdempotentHint {
			t.Errorf("%s is annotated idempotent; each call stages another copy", name)
		}
	}
}

// TestUploadURLIsTheOnlyClosedWorldTool. Everything else here answers with, or acts on, what a
// mail provider holds at that moment. Minting an upload URL reaches nothing outside this
// process, and saying so is what tells a client the answer does not depend on a mailbox.
func TestUploadURLIsTheOnlyClosedWorldTool(t *testing.T) {
	for name, tool := range listTools(t, fullGrant(grant.ModeConfirm)) {
		open := tool.Annotations.OpenWorldHint != nil && *tool.Annotations.OpenWorldHint
		if name == "mail_upload_url" && open {
			t.Error("mail_upload_url is annotated open-world; it touches no provider")
		}
		if name != "mail_upload_url" && !open {
			t.Errorf("%s is annotated closed-world", name)
		}
	}
}

func sortedNames(m map[string]*mcp.Tool) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
