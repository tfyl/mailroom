package mcp

import "github.com/tfyl/mailroom/internal/grant"

// The steering half of a grant's mode: the wording a client reads in each tool's description.
//
// Be exact about what this is. A tool's Description is the only thing on this server that
// talks to the model rather than to the code — it is read once, at tools/list, and everything
// the agent decides after that is decided by the agent. It is real and it works, and it stops
// nothing. Two of the three modes are made entirely of it.
//
// So the wording is written as instructions about when to act rather than as caution. "Be
// careful with sending" tells a model to be careful and leaves it to decide what that means.
// "Show the recipients, the subject and the body to your human and wait for an answer" names
// the act, the audience and the ordering, and a model either did it or did not.
//
// The `hold` wording carries a second job, and it is the one that would be a defect to get
// wrong. Under that mode a send does not happen, and a client that reports "sent" to its user
// because the tool call returned successfully has told them something false about their mail.
// Every held tool says, in its description and again in its result, that the call queues
// rather than performs.

// steering is one tool's description: the part that never changes, and the sentence appended
// for each mode. An empty clause means the mode makes no difference to this tool — most of
// them, because reads and reversible changes are not what a mode is about.
type steering struct {
	base       string
	unattended string
	confirm    string
	hold       string
}

// text is the description a client sees for this tool under this mode.
func (s steering) text(m grant.Mode) string {
	var clause string
	switch m.Resolved() {
	case grant.ModeUnattended:
		clause = s.unattended
	case grant.ModeHold:
		clause = s.hold
	default:
		clause = s.confirm
	}
	if clause == "" {
		return s.base
	}
	return s.base + " " + clause
}

// The register is mail_accounts': plain sentences, second person, saying what to do rather
// than what to feel. Where a mode changes nothing about a tool, it adds nothing to it — a
// caution repeated on every description is a caution read on none of them.
var toolSteering = map[string]steering{
	"mail_accounts": {
		base: "List the mailboxes this grant can reach, what each provider supports, and " +
			"which capabilities you currently hold. Call this first: it tells you what you " +
			"may do before you attempt it.",
		unattended: "This connection is set to work unattended: carry the task through " +
			"without stopping to clear each step, sending included, because there may be " +
			"nobody at the other end to answer you.",
		confirm: "This connection is set to check with a person first: before you send, " +
			"delete, write a filter or change the vacation responder, tell the human you are " +
			"working with exactly what you are about to do and wait for their answer.",
		hold: "This connection is set to hold privileged actions. Sending, deleting, filters " +
			"and the vacation responder are not carried out when you call for them: each one " +
			"is queued for the mailbox's owner to approve in mailroom's own web interface, " +
			"and the result says so. Everything else runs normally. Say that such an action " +
			"is waiting for approval, never that it is done.",
	},

	"mail_draft": {
		base: "Create, update or delete a draft. Creating and updating need the `draft` " +
			"capability; deleting one needs the separate `discard` capability, so a grant may " +
			"be able to compose a draft and not to remove it. Drafting never sends: sending " +
			"needs mail_send and the separate `send` capability. Attach files with " +
			"`attachments` — reuse one already in a mailbox by naming from_message and " +
			"attachment_id, which never moves the bytes through this conversation.",
		confirm: "Drafting needs nobody's permission, because a draft changes nothing that " +
			"cannot be changed back. Compose the message here and put the finished text in " +
			"front of your human rather than describing what you would write.",
		hold: "Nothing on this tool is held, deleting included: a draft is not mail anybody " +
			"has received, so it is written and removed immediately. When a send is going to " +
			"be queued anyway, drafting first gives the mailbox's owner something they can " +
			"read and edit before they approve it.",
	},

	"mail_send": {
		base: "Send a message, a reply, or an existing draft. This is irreversible and rate " +
			"limited per grant. Attach files with `attachments`, either by reference to one " +
			"already in a mailbox or as inline base64.",
		unattended: "Send when the work calls for it; you do not need to clear each message " +
			"with anyone first.",
		confirm: "Show the recipients, the subject and the body to the human you are working " +
			"with, and wait for their answer before you call this. Do not call it to find out " +
			"whether they would have agreed: this is the one tool whose result cannot be " +
			"taken back.",
		hold: "Calling this does not deliver anything. The message is queued for the " +
			"mailbox's owner to approve or discard in mailroom, and nothing leaves the " +
			"mailbox until they approve it. The result carries `held: true` and the id it was " +
			"queued under. Report the message as waiting for approval — reporting it as sent " +
			"is wrong, and whoever you are working with will act on what you tell them.",
	},

	"mail_modify": {
		base: "Label, archive, star, or mark read across one or many messages. Ids may span " +
			"mailboxes; each is authorized separately. One thing here is not filing: a label " +
			"whose effect is destruction — Gmail's TRASH or SPAM, or a folder that is the bin " +
			"or junk — moves the mail out of the mailbox, and needs the `destructive` " +
			"capability as well as `labels`. Use mail_trash when that is what you mean.",
		unattended: "File, archive, star and mark read as the task requires.",
		confirm: "Filing, archiving, starring and marking read need nobody's permission: they " +
			"take nothing away and they can be undone. Moving mail to the bin or to junk is " +
			"not in that class — name the messages you mean and wait for your human's answer, " +
			"exactly as you would before calling mail_trash.",
		hold: "Filing, archiving, starring and marking read run immediately. A change that " +
			"moves mail to the bin or to junk is not carried out when you call for it: the " +
			"whole call is queued for the mailbox's owner to approve in mailroom, and the " +
			"result says `held`. Say it is waiting for approval, never that it is done.",
	},

	"mail_trash": {
		base: "Move messages to trash, restore them, or delete permanently. `delete` cannot be undone.",
		unattended: "Trash and restore as the task requires. Use `delete` only for what you " +
			"were asked to delete by name.",
		confirm: "Name the messages you mean and get your human's agreement before you trash " +
			"or delete anything. `untrash` needs no permission: it only puts mail back.",
		hold: "`trash` and `delete` are not carried out when you call for them: each call is " +
			"queued for the mailbox's owner to approve in mailroom, and the result says " +
			"`held`. `untrash` runs immediately, because restoring mail takes nothing away.",
	},

	"mail_filters": {
		base: "List, create or delete server-side filters. Actions are expressed as label " +
			"changes: archiving is removing INBOX, trashing is adding TRASH.",
		unattended: "Create and delete filters as the task requires.",
		confirm: "Listing needs no permission. Creating or deleting a filter changes what " +
			"happens to mail that has not arrived yet, which nobody will watch happen — put " +
			"the exact rule to your human, in words, before you write it.",
		hold: "Listing runs immediately. Creating or deleting a filter is queued for the " +
			"mailbox's owner to approve in mailroom and the result says `held`; the mailbox's " +
			"rules are unchanged until they approve it.",
	},

	"mail_settings": {
		base: "Read mailbox settings — send-as aliases, vacation responder, forwarding, " +
			"delegates, IMAP — and set the vacation responder. Not every provider supports " +
			"every section; mail_accounts reports which.",
		unattended: "Set the vacation responder when the task calls for it.",
		confirm: "Reading any section needs no permission. `set_vacation` turns on an " +
			"automatic reply that answers everyone who writes to this mailbox until somebody " +
			"turns it off — show your human the subject and body and wait for an answer " +
			"before you set it.",
		hold: "Reading any section runs immediately. `set_vacation` is queued for the " +
			"mailbox's owner to approve in mailroom and the result says `held`; the responder " +
			"is unchanged until they approve it.",
	},
}

// describe is what Register hands the SDK for a tool whose wording the mode changes. The
// tools not named in the map above keep their descriptions where they are registered, which
// is the right place for a description that is the same in every mode.
//
// A name with no entry would be a tool this file was meant to steer and does not, so it
// answers with something a reader will notice rather than with silence.
func describe(tool string, m grant.Mode) string {
	s, ok := toolSteering[tool]
	if !ok {
		return "(no description: " + tool + " has no steering)"
	}
	return s.text(m)
}
