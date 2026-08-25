package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfyl/mailroom/internal/grant"
	"github.com/tfyl/mailroom/internal/held"
	"github.com/tfyl/mailroom/internal/mail"
)

// The administrative tail: filters and settings. Both take an action parameter rather than
// splitting into a tool each, because they are touched rarely and a dozen more tool
// definitions would cost every client context on every call for the sake of a quarterly
// operation.

// --- mail_filters ---

type filtersArgs struct {
	Account string `json:"account,omitempty" jsonschema:"Which mailbox. Required when the grant covers more than one."`
	Action  string `json:"action,omitempty" jsonschema:"list, create or delete. Defaults to list."`
	ID      string `json:"id,omitempty" jsonschema:"Filter id, required to delete."`

	From          string   `json:"from,omitempty" jsonschema:"Match mail from this sender"`
	To            string   `json:"to,omitempty"`
	Subject       string   `json:"subject,omitempty"`
	Query         string   `json:"query,omitempty" jsonschema:"Provider search syntax the filter matches on"`
	NegatedQuery  string   `json:"negated_query,omitempty" jsonschema:"Mail matching this is excluded"`
	HasAttachment bool     `json:"has_attachment,omitempty"`
	AddLabels     []string `json:"add_labels,omitempty" jsonschema:"Labels to apply. Adding TRASH deletes; there is no separate delete action."`
	RemoveLabels  []string `json:"remove_labels,omitempty" jsonschema:"Labels to remove. Removing INBOX archives."`
	// Declared so a client that asks for it is refused by name rather than having it
	// silently dropped, which would leave somebody believing their mail was being forwarded.
	Forward string `json:"forward,omitempty" jsonschema:"Not settable from here. Forwarding hands another address a copy of the mail and is a decision for a person at a settings page; a filter that forwards is the same act with a delay on it. Listing filters still reports one that already exists."`
}

func (t *Tools) handleFilters(ctx context.Context, _ *mcp.CallToolRequest, args filtersArgs) (*mcp.CallToolResult, any, error) {
	g, err := requireGrant(ctx)
	if err != nil {
		return nil, nil, err
	}
	if args.Action == "" {
		args.Action = "list"
	}

	acct, err := t.oneAccount(ctx, g, "mail.filters", args.Account, mail.CapFilters, args.Action)
	if err != nil {
		return toolError(err), nil, nil
	}
	manager, err := t.filterManager(ctx, acct)
	if err != nil {
		return toolError(err), nil, nil
	}

	switch args.Action {
	case "list":
		filters, err := manager.ListFilters(ctx)
		if err := t.auditRead(ctx, g, grant.Audit{
			AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
			Affected: counted(len(filters)), Detail: grant.Detail{Action: "list"},
		}, err); err != nil {
			return toolError(err), nil, nil
		}
		rendered := make([]map[string]any, len(filters))
		for i, f := range filters {
			rendered[i] = renderFilter(f)
		}
		return result(map[string]any{
			"account": acct.Alias, "account_address": acct.Address, "filters": rendered,
		})

	case "create":
		// Refused for the same reason handleSettings refuses to write forwarding: it hands
		// somebody else access to the mail itself, which is a decision for a person at a
		// settings page rather than something an agent arranges on their behalf.
		//
		// A filter that forwards is that act with a delay and a repeat on it — nobody watches
		// a rule run, it applies to mail that has not arrived yet, and on Graph the
		// destination is not verified — so refusing in one tool and accepting in the other
		// left the documented threat reachable through the door marked "rule management". The
		// audit comment further down already called a forwarding rule "the exfiltration this
		// whole product is trying not to enable", and then recorded it rather than stopping
		// it.
		if args.Forward != "" {
			refusal := fmt.Errorf(
				"nothing was created: a filter that forwards mail to %s would hand that "+
					"address a copy of everything it matches, including mail that has not "+
					"arrived yet. Forwarding is set by a person at the mailbox's own settings "+
					"page — mail_settings will not write it either. Create the filter without "+
					"`forward`, or ask the mailbox's owner to set forwarding themselves.",
				args.Forward)
			_ = t.gate.Record(ctx, g, grant.Audit{
				AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
				Outcome: grant.RefusedAs(refusal), Reason: refusal.Error(),
				Detail: grant.Detail{
					Action: "create", To: nonEmpty(args.Forward), Subject: describeFilter(mail.Filter{
						From: args.From, To: args.To, Subject: args.Subject, Query: args.Query,
					}),
				},
			})
			return toolError(refusal), nil, nil
		}

		filter := mail.Filter{
			From: args.From, To: args.To, Subject: args.Subject,
			Query: args.Query, NegatedQuery: args.NegatedQuery, HasAttachment: args.HasAttachment,
			AddLabels: toLabelIDs(args.AddLabels), RemoveLabels: toLabelIDs(args.RemoveLabels),
		}
		// A filter that files mail in the bin is the same class of thing mail_modify was
		// letting through, with a delay and a repeat on it: nobody watches a rule run, and it
		// applies to mail that has not arrived yet. So it needs `destructive` on the same
		// terms, and for the stronger reason.
		//
		// The `hold` half is already right below — creating a filter is held whatever it does
		// — which is why this only decides the capability.
		if len(filter.AddLabels) > 0 {
			labels, err := t.labelManager(ctx, acct)
			if err != nil {
				return toolError(err), nil, nil
			}
			applies, err := mail.DestructiveApplies(ctx, labels, filter.AddLabels)
			if err != nil {
				return toolError(err), nil, nil
			}
			if len(applies) > 0 && !g.Caps.Has(mail.CapDestructive) {
				refusal := destructiveRefusal(g, acct, applies)
				_ = t.gate.Record(ctx, g, grant.Audit{
					AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapDestructive,
					Outcome: grant.RefusedAs(refusal), Reason: refusal.Error(),
					Detail: grant.Detail{
						Action: "create", To: nonEmpty(args.Forward), Subject: describeFilter(filter),
					},
				})
				return toolError(refusal), nil, nil
			}
		}

		// Held before the provider is reached, so nothing about the mailbox's rules changes
		// until its owner says so. What the queue holds is the filter the client asked for;
		// the audit row it writes says the same thing the created row below would.
		if g.Mode.Holds() {
			return t.heldResult(ctx, g, acct, grant.Audit{
				AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
				Detail: grant.Detail{
					Action: "create", To: nonEmpty(args.Forward), Subject: describeFilter(filter),
				},
			}, held.KindFilterAdd,
				"add a filter to "+acct.Alias+": "+filterInWords(filter),
				held.FilterPayload{Filter: filter})
		}
		created, err := manager.CreateFilter(ctx, filter)
		// A forwarding address is recorded beside the recipients of a send, because it is the
		// same act with a delay on it: a rule that mails everything matching a query to a
		// stranger is the exfiltration this whole product is trying not to enable, and a row
		// that said only "a filter was created" would be the one place it did not show up.
		entry := grant.Audit{
			AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
			Detail: grant.Detail{
				Action: "create", Name: created.ID,
				To: nonEmpty(args.Forward), Subject: describeFilter(created),
			},
		}
		note := t.auditChange(ctx, g, entry, err)
		if err != nil {
			return toolError(err), nil, nil
		}
		return result(noted(map[string]any{
			"account": acct.Alias, "account_address": acct.Address,
			"created": renderFilter(created),
		}, note))

	case "delete":
		if args.ID == "" {
			return t.invalid(ctx, g, grant.Audit{
				AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
				Detail: grant.Detail{Action: "delete"},
			}, fmt.Errorf("id is required to delete a filter")), nil, nil
		}
		if g.Mode.Holds() {
			return t.heldResult(ctx, g, acct, grant.Audit{
				AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
				Detail: grant.Detail{Action: "delete", Name: args.ID},
			}, held.KindFilterDrop,
				"delete the filter "+args.ID+" from "+acct.Alias,
				held.FilterPayload{FilterID: args.ID})
		}
		err := manager.DeleteFilter(ctx, args.ID)
		note := t.auditChange(ctx, g, grant.Audit{
			AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
			Detail: grant.Detail{Action: "delete", Name: args.ID},
		}, err)
		if err != nil {
			return toolError(err), nil, nil
		}
		return result(noted(map[string]any{
			"account": acct.Alias, "account_address": acct.Address, "deleted": args.ID,
		}, note))

	default:
		return t.invalid(ctx, g, grant.Audit{
			AccountID: acct.ID, Tool: "mail.filters", Capability: mail.CapFilters,
		}, fmt.Errorf("action must be list, create or delete; got %q", args.Action)), nil, nil
	}
}

// describeFilter renders what a filter matches on, so a row says which mail the rule reaches
// rather than only that a rule now exists. These are the criteria a client supplied, not mail
// that was read: no message this filter will ever match has been looked at yet.
func describeFilter(f mail.Filter) string {
	var parts []string
	for _, c := range []struct{ label, value string }{
		{"from", f.From}, {"to", f.To}, {"subject", f.Subject},
		{"query", f.Query}, {"not", f.NegatedQuery},
	} {
		if c.value != "" {
			parts = append(parts, c.label+":"+c.value)
		}
	}
	if f.HasAttachment {
		parts = append(parts, "has:attachment")
	}
	for _, l := range f.AddLabels {
		parts = append(parts, "+"+string(l))
	}
	for _, l := range f.RemoveLabels {
		parts = append(parts, "-"+string(l))
	}
	return strings.Join(parts, " ")
}

// nonEmpty makes a one-item list, or none, so an absent value stays absent rather than
// becoming a list holding an empty string.
func nonEmpty(v string) []string {
	if v == "" {
		return nil
	}
	return []string{v}
}

func renderFilter(f mail.Filter) map[string]any {
	out := map[string]any{"id": f.ID}
	for k, v := range map[string]string{
		"from": f.From, "to": f.To, "subject": f.Subject,
		"query": f.Query, "negated_query": f.NegatedQuery, "forward": f.Forward,
	} {
		if v != "" {
			out[k] = v
		}
	}
	if f.HasAttachment {
		out["has_attachment"] = true
	}
	if len(f.AddLabels) > 0 {
		out["add_labels"] = labelStrings(f.AddLabels)
	}
	if len(f.RemoveLabels) > 0 {
		out["remove_labels"] = labelStrings(f.RemoveLabels)
	}
	return out
}

// --- mail_settings ---

type settingsArgs struct {
	Account string `json:"account,omitempty"`
	Action  string `json:"action,omitempty" jsonschema:"aliases, vacation, forwarding, delegates, imap, or set_vacation. Defaults to aliases."`

	// set_vacation only.
	Enabled            bool   `json:"enabled,omitempty"`
	Subject            string `json:"subject,omitempty"`
	Body               string `json:"body,omitempty"`
	RestrictToContacts bool   `json:"restrict_to_contacts,omitempty" jsonschema:"Only reply to people in your contacts"`
	RestrictToDomain   bool   `json:"restrict_to_domain,omitempty" jsonschema:"Only reply to people in your organisation"`
}

// handleSettings reads mailbox settings, and writes exactly one of them.
//
// Only the vacation responder can be changed. Forwarding and delegation hand somebody else
// access to the mail itself, which is a decision for a person at a settings page, not
// something an agent should arrange on their behalf — so those stay readable and no more.
func (t *Tools) handleSettings(ctx context.Context, _ *mcp.CallToolRequest, args settingsArgs) (*mcp.CallToolResult, any, error) {
	g, err := requireGrant(ctx)
	if err != nil {
		return nil, nil, err
	}
	if args.Action == "" {
		args.Action = "aliases"
	}

	acct, err := t.oneAccount(ctx, g, "mail.settings", args.Account, mail.CapSettings, args.Action)
	if err != nil {
		return toolError(err), nil, nil
	}
	p, err := t.providers.For(ctx, acct)
	if err != nil {
		return toolError(err), nil, nil
	}

	// The code a client matches on is derived from the error's type, so a refusal has to
	// travel as one. Formatted into a message it arrives as a generic `error`, which is the
	// code that means "worth retrying" — the opposite of what this is.
	unsupported := func(section string) (*mcp.CallToolResult, any, error) {
		refusal := &mail.UnsupportedError{
			Provider: p.ID(), Account: acct.Alias, Address: acct.Address,
			Capability: mail.CapSettings,
		}
		if _, hasSettings := p.(mail.SettingsManager); hasSettings {
			// This mailbox has settings and is missing one corner of them. Naming the
			// capability instead would tell a caller to stop trying sections that work.
			refusal.Op = section + " settings"
		}
		return toolError(refusal), nil, nil
	}

	// Which section, on every one of these: reading the delegate list and reading the IMAP
	// toggle are different things to have done to a mailbox, and both used to record as
	// "mail.settings ok".
	section := grant.Audit{
		AccountID: acct.ID, Tool: "mail.settings", Capability: mail.CapSettings,
		Detail: grant.Detail{Action: args.Action},
	}

	switch args.Action {
	case "aliases":
		manager, ok := p.(mail.SettingsManager)
		if !ok {
			return unsupported("alias")
		}
		aliases, err := manager.ListSendAs(ctx)
		read := section
		read.Affected = counted(len(aliases))
		if err := t.auditRead(ctx, g, read, err); err != nil {
			return toolError(err), nil, nil
		}
		rendered := make([]map[string]any, len(aliases))
		for i, a := range aliases {
			rendered[i] = map[string]any{
				"address": a.Address, "display_name": a.DisplayName, "reply_to": a.ReplyTo,
				"default": a.Default, "primary": a.Primary, "verified": a.Verified,
			}
		}
		return result(map[string]any{
			"account": acct.Alias, "account_address": acct.Address, "aliases": rendered,
		})

	case "vacation":
		manager, ok := p.(mail.SettingsManager)
		if !ok {
			return unsupported("vacation")
		}
		v, err := manager.GetVacation(ctx)
		if err := t.auditRead(ctx, g, section, err); err != nil {
			return toolError(err), nil, nil
		}
		return result(map[string]any{
			"account": acct.Alias, "account_address": acct.Address,
			"vacation": map[string]any{
				"enabled": v.Enabled, "subject": v.Subject, "body": v.Body,
				"restrict_to_contacts": v.RestrictToContacts, "restrict_to_domain": v.RestrictToDomain,
			},
		})

	case "set_vacation":
		manager, ok := p.(mail.SettingsManager)
		if !ok {
			return unsupported("vacation")
		}
		vacation := mail.Vacation{
			Enabled: args.Enabled, Subject: args.Subject, Body: args.Body,
			RestrictToContacts: args.RestrictToContacts, RestrictToDomain: args.RestrictToDomain,
		}
		// The subject of the auto-reply, and never its body. An auto-reply is outgoing mail,
		// so its subject sits with the subject of a send; its body is a message body, and the
		// rule about those has no exception for the one this server composed.
		wrote := section
		wrote.Detail.Action = "set_vacation " + onOff(args.Enabled)
		wrote.Detail.Subject = args.Subject
		if g.Mode.Holds() {
			return t.heldResult(ctx, g, acct, wrote, held.KindSetVacation,
				describeVacation(vacation, acct.Alias), held.VacationPayload{Vacation: vacation})
		}
		err := manager.SetVacation(ctx, vacation)
		note := t.auditChange(ctx, g, wrote, err)
		if err != nil {
			return toolError(err), nil, nil
		}
		return result(noted(map[string]any{
			"account": acct.Alias, "account_address": acct.Address,
			"vacation_enabled": args.Enabled,
		}, note))

	case "delegates":
		manager, ok := p.(mail.DelegateManager)
		if !ok {
			return unsupported("delegate")
		}
		delegates, err := manager.ListDelegates(ctx)
		read := section
		read.Affected = counted(len(delegates))
		if err := t.auditRead(ctx, g, read, err); err != nil {
			return toolError(err), nil, nil
		}
		rendered := make([]map[string]any, len(delegates))
		for i, d := range delegates {
			rendered[i] = map[string]any{"address": d.Address, "verified": d.Verified}
		}
		return result(map[string]any{
			"account": acct.Alias, "account_address": acct.Address, "delegates": rendered,
		})

	case "forwarding":
		reader, ok := p.(mail.ForwardingReader)
		if !ok {
			return unsupported("forwarding")
		}
		f, err := reader.GetForwarding(ctx)
		if err := t.auditRead(ctx, g, section, err); err != nil {
			return toolError(err), nil, nil
		}
		return result(map[string]any{
			"account": acct.Alias, "account_address": acct.Address,
			"forwarding": map[string]any{
				"enabled": f.Enabled, "address": f.Address, "disposition": f.Disposition,
			},
		})

	case "imap":
		reader, ok := p.(mail.IMAPSettingsReader)
		if !ok {
			return unsupported("IMAP")
		}
		s, err := reader.GetIMAPSettings(ctx)
		if err := t.auditRead(ctx, g, section, err); err != nil {
			return toolError(err), nil, nil
		}
		return result(map[string]any{
			"account": acct.Alias, "account_address": acct.Address,
			"imap": map[string]any{
				"enabled": s.Enabled, "auto_expunge": s.AutoExpunge, "max_folder_size": s.MaxFolderSize,
			},
		})

	default:
		return t.invalid(ctx, g, section, fmt.Errorf(
			"action must be aliases, vacation, set_vacation, forwarding, delegates or imap; got %q",
			args.Action)), nil, nil
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// --- helpers ---

// oneAccount resolves exactly one mailbox for an administrative call.
//
// These operations are per-mailbox and never fan out: applying a filter or an auto-reply to
// every mailbox a grant happens to cover, because the caller omitted a name, is not a
// mistake worth allowing.
func (t *Tools) oneAccount(ctx context.Context, g *grant.Grant, tool, selector string, c mail.Capability, action string) (mail.Account, error) {
	var sel []string
	if selector != "" {
		sel = []string{selector}
	}
	accounts, err := t.gate.Resolve(ctx, g, tool, sel, c)
	if err != nil {
		return mail.Account{}, err
	}
	if len(accounts) > 1 {
		// Bare aliases, for the same reason mail_send's disambiguation uses them: this list
		// is offered to be chosen from, and whatever it names is what comes back in
		// `account`, so every entry has to be a selector that resolves.
		names := make([]string, len(accounts))
		for i, a := range accounts {
			names[i] = a.Alias
		}
		return mail.Account{}, fmt.Errorf(
			"this grant covers several mailboxes (%v); name one with `account` to %s", names, action)
	}
	return accounts[0], nil
}

func (t *Tools) filterManager(ctx context.Context, acct mail.Account) (mail.FilterManager, error) {
	p, err := t.providers.For(ctx, acct)
	if err != nil {
		return nil, err
	}
	manager, ok := p.(mail.FilterManager)
	if !ok {
		return nil, &mail.UnsupportedError{
			Provider: p.ID(), Account: acct.Alias, Address: acct.Address, Capability: mail.CapFilters,
		}
	}
	return manager, nil
}

func labelStrings(in []mail.LabelID) []string {
	out := make([]string, len(in))
	for i, l := range in {
		out[i] = string(l)
	}
	return out
}

// filterInWords and describeVacation are the one line a held action is listed by.
//
// A filter is a rule about mail that has not arrived, so the summary has to say what it
// matches and what it would do — "everything from x, archived" is a decision somebody can
// take, and "a filter" is not. Separate from describeFilter, which writes the same rule for
// an audit row: that one is scanned in a table column and this one is read as a sentence by
// somebody about to press a button.
func filterInWords(f mail.Filter) string {
	var matches []string
	for label, value := range map[string]string{
		"from": f.From, "to": f.To, "subject": f.Subject,
		"matching": f.Query, "not matching": f.NegatedQuery,
	} {
		if value != "" {
			matches = append(matches, label+" "+value)
		}
	}
	if f.HasAttachment {
		matches = append(matches, "with an attachment")
	}
	sort.Strings(matches)

	var does []string
	for _, l := range f.AddLabels {
		does = append(does, "label "+string(l))
	}
	for _, l := range f.RemoveLabels {
		does = append(does, "remove "+string(l))
	}
	if f.Forward != "" {
		does = append(does, "forward to "+f.Forward)
	}

	if len(matches) == 0 {
		matches = []string{"every message"}
	}
	if len(does) == 0 {
		does = []string{"nothing"}
	}
	return strings.Join(matches, ", ") + " → " + strings.Join(does, ", ")
}

func describeVacation(v mail.Vacation, alias string) string {
	if !v.Enabled {
		return "turn off the vacation responder on " + alias
	}
	subject := v.Subject
	if subject == "" {
		subject = "(no subject)"
	}
	return "turn on the vacation responder on " + alias + ", replying " + subject
}
