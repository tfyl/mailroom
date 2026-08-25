package mcp

import "github.com/modelcontextprotocol/go-sdk/mcp"

// The machine-readable half of what this server says about its tools.
//
// A Description is prose a model reads and weighs. An annotation is a flag a client acts on
// without reading anything: which tools to auto-approve, which to put a confirmation in front
// of, which are safe to retry after a timeout, and which may be run while nobody is watching.
// The two are written together here because they answer to different readers, and getting the
// annotation wrong is worse than leaving it off — an absent hint falls back to the spec's
// cautious default, while a wrong one is a claim a client believes.
//
// Three rules decide every cell below.
//
// A tool that multiplexes operations is annotated as the riskiest thing it can do. mail_draft
// writes a draft and also deletes one; mail_labels lists labels and also deletes one. A hint
// describes the tool, not the argument, so the value has to be the one that stays true
// whichever way the tool is called.
//
// An annotation must hold in every mode. Register runs per connection and the grant's mode is
// in hand, so these could be varied by it — the comment above tool() says why they are not.
// The consequence here is that any tool a `hold` grant queues rather than performs is not
// idempotent: calling it twice queues two actions for the mailbox's owner to approve, which is
// an additional effect even though the mailbox never moved.
//
// `false` on a non-read-only tool is a positive claim, not an absence. The spec's default for
// destructiveHint is true, so writing false says "this only ever adds". It is written only
// where that is exactly so.
type annotation struct {
	// title is a display name, and the only field here aimed at a person rather than a
	// client's policy engine.
	title string

	readOnly    bool
	destructive bool
	idempotent  bool
	openWorld   bool
}

// toolAnnotations is the table, one row per registered tool.
//
// The reads are unremarkable and are grouped first. Everything after them had a decision in
// it, and the ones worth arguing carry the argument.
var toolAnnotations = map[string]annotation{
	// Reads. Each reports what a mailbox holds right now, so the same call tomorrow answers
	// differently — that is what openWorld is for, and it is not a caveat about safety.
	"mail_accounts": {
		title:      "List reachable mailboxes",
		readOnly:   true,
		idempotent: true,
		openWorld:  true,
	},
	"mail_search": {
		title:      "Search mail",
		readOnly:   true,
		idempotent: true,
		openWorld:  true,
	},
	"mail_get_message": {
		title:      "Read one message",
		readOnly:   true,
		idempotent: true,
		openWorld:  true,
	},
	"mail_get_thread": {
		title:      "Read a conversation",
		readOnly:   true,
		idempotent: true,
		openWorld:  true,
	},

	// mail_get_attachment is not read-only, and that is the row most likely to look wrong.
	//
	// It takes nothing out of the mailbox, so "reading" is the obvious reading. But
	// readOnlyHint is about the tool's environment rather than about the mail, and this call
	// writes to it three times over: it copies the attachment onto this server's disk, charges
	// those bytes against the owner's storage allowance, and mints a signed URL that serves
	// the file to anyone holding it with no token at all. A client that treats read-only as
	// "call this as often as you like" would fill the allowance and leave a trail of live
	// credentials behind it. Not idempotent for the same reason: two calls are two copies and
	// two URLs, not one answer given twice.
	//
	// The `inline` path genuinely is a read, and does none of that. It is one argument on a
	// tool whose annotation has to describe every way the tool can be called.
	"mail_get_attachment": {
		title:     "Download an attachment",
		openWorld: true,
	},

	// mail_upload_url is the only tool here whose world is closed. It reaches no provider and
	// no mailbox: it reserves space in this server's own store and signs a URL for it, and the
	// answer depends on nothing outside this process but the clock and the quota. Purely
	// additive, so destructive is false; a second call is a second URL and a second
	// reservation, so idempotent is not.
	"mail_upload_url": {
		title: "Stage a file to attach",
	},

	// Writes.
	"mail_labels": {
		title:       "List, create or delete labels",
		destructive: true,
		openWorld:   true,
	},
	"mail_draft": {
		// Drafting is reversible and the steering says so, but this tool also carries the
		// `discard` action, which removes a draft somebody may have written by hand.
		title:       "Write, update or delete a draft",
		destructive: true,
		openWorld:   true,
	},
	"mail_send": {
		// Destructive is a stretch of the word and the right answer anyway. Sending adds a
		// message rather than removing one, so a literal reading argues for false — but false
		// is the claim "this only ever adds, so it is safe to take back", and this is the one
		// call on this server whose result cannot be taken back. The spec's default for a
		// non-read-only tool is destructive, and mail_send is the last tool that should be
		// opted out of it.
		title:       "Send mail",
		destructive: true,
		openWorld:   true,
	},
	"mail_modify": {
		// This row used to read idempotent and not destructive, on the reasoning that every
		// change it makes is undoable by another call to itself — a label added can be
		// removed, an archived message can have INBOX put back — and it said in as many words
		// that if the tool ever gained a way to move mail to the trash, both would stop being
		// true. It always had one. Applying Gmail's TRASH bins the message, and on the
		// providers with folders applying an exclusive label is a move, so naming the bin is
		// the request mail_trash sends. That now needs `destructive` and is held, and these
		// hints have to say so.
		//
		// Destructive, then, for the same reason mail_trash is: not every call bins mail, but
		// some can, and a hint a client acts on without judgement fails closed. Not idempotent
		// for the same reason mail_trash is not — under `hold` a destructive change becomes a
		// queued action rather than a change, so a client retrying on the strength of this
		// hint would put a second binning in front of the mailbox's owner to approve.
		title:       "Label, archive, star, mark read or bin",
		destructive: true,
		openWorld:   true,
	},
	"mail_trash": {
		// Destructive, obviously — `delete` cannot be undone. Not idempotent, less obviously:
		// trashing an already-trashed message leaves the mailbox where it was, so on its own
		// this would qualify. Under `hold` it does not. A held grant turns each call into a
		// queued action rather than a change, and a client that retried on the strength of
		// this hint would put a second deletion in front of the mailbox's owner to approve.
		title:       "Trash, restore or delete permanently",
		destructive: true,
		openWorld:   true,
	},
	"mail_filters": {
		title:       "List, create or delete filters",
		destructive: true,
		openWorld:   true,
	},
	"mail_settings": {
		// `set_vacation` replaces whatever responder is already configured rather than adding
		// to it, which is a destructive update to a setting somebody else may have written.
		title:       "Read settings; set the vacation responder",
		destructive: true,
		openWorld:   true,
	},
}

// One decision this table does not make, because the opposite one is tempting and wrong.
//
// Register is called per connection with the grant in hand, so a tool's annotations could be
// varied by its mode. Under `hold` a send reaches no provider, destroys nothing and touches no
// mailbox, and it would be easy to describe that connection's mail_send as a closed-world,
// non-destructive tool. Three reasons not to.
//
// The direction of the change is unsafe. Mode-dependent steering makes a description more
// cautious under `hold`; mode-dependent annotations would make the flags less cautious, and
// they are the half a client acts on without judgement. A tools/list cached at connect time,
// or a grant an operator moves off `hold` afterwards, would leave a client holding hints that
// say a send is harmless.
//
// The queue is not the end of the story. A held send is mail that goes out the moment somebody
// presses Approve, which is usually minutes later. Annotating it as harmless would encourage
// exactly the behaviour that fills the queue with sends nobody meant, and approval fatigue is
// how those get delivered.
//
// And `hold` does not cover a whole tool in any case. mail_trash performs `untrash`
// immediately, mail_filters lists immediately, mail_settings reads immediately, and mail_draft
// is never held at all. A per-mode downgrade would be wrong for the half of each tool that
// still runs.
//
// So annotations describe the action a tool exists to take, in every mode. The mode lives in
// the description, where a reader weighs it, and in the result of the call, which says `held`.
// tool builds the Tool a client sees: the name and description the caller supplies, and the
// hints for that name.
//
// A name with no row is a programming error — a tool registered without anyone deciding what
// it does to a mailbox. It fails closed, to the most cautious answer the spec has, and
// annotations_test.go turns it into a build failure rather than leaving it to be noticed.
func tool(name, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: annotationsFor(name),
	}
}

func annotationsFor(name string) *mcp.ToolAnnotations {
	a, ok := toolAnnotations[name]
	if !ok {
		a = annotation{title: name, destructive: true, openWorld: true}
	}
	return &mcp.ToolAnnotations{
		Title:           a.title,
		ReadOnlyHint:    a.readOnly,
		DestructiveHint: &a.destructive,
		IdempotentHint:  a.idempotent,
		OpenWorldHint:   &a.openWorld,
	}
}
