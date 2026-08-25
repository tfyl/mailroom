# Providers

Gmail first, then Zoho, then generic IMAP/SMTP, then Microsoft Graph. The seam between
mailroom and a mail service is the main extension point, and the one place where getting the
abstraction wrong is expensive later.

## Capability interfaces, not one big interface

The tempting design is one `Provider` interface with every method on it, and stubs returning
`ErrNotSupported` in half the implementations. That produces an interface shaped entirely by
whichever provider was written first, and callers that cannot tell "this failed" from "this
was never possible".

Instead: a small identity interface, plus narrow capability interfaces a provider implements
only when it genuinely supports them.

The signatures below are the interfaces in `internal/mail/provider.go` as they stand, with
the doc comments and the plain data types they exchange — `Filter`, `SendAs`, `Vacation`,
`Delegate`, `Forwarding`, `IMAPSettings` — left in the file. Read it before implementing
against it: this copy exists to show the shape, and a copy is a thing that can go stale.

```go
type Provider interface {
    ID() ProviderID
    Capabilities() Set
    Quirks() []Quirk
}

type MessageReader interface {
    Search(ctx context.Context, q Query, cursor string) (Page[Message], error)
    Get(ctx context.Context, id ScopedID) (Message, error)
}

type ThreadReader     interface { GetThread(ctx context.Context, id ScopedID) (Thread, error) }
type MessageWriter    interface { Send(ctx context.Context, out Outgoing) (ScopedID, error) }

type AttachmentReader interface {
    GetAttachment(ctx context.Context, msg ScopedID, attachmentID string) (Attachment, error)
}

type DraftManager interface {
    CreateDraft(ctx context.Context, out Outgoing) (ScopedID, error)
    UpdateDraft(ctx context.Context, id ScopedID, out Outgoing) error
    SendDraft(ctx context.Context, id ScopedID) (ScopedID, error)
    DeleteDraft(ctx context.Context, id ScopedID) error
    ListDrafts(ctx context.Context, cursor string) (Page[Message], error)
}

// LabelManager covers labels, folders, and the read/star flags that ride alongside them.
type LabelManager interface {
    ListLabels(ctx context.Context) ([]Label, error)
    CreateLabel(ctx context.Context, name string, exclusive bool) (Label, error)
    DeleteLabel(ctx context.Context, id LabelID) error
    ApplyLabels(ctx context.Context, ids []ScopedID, add, remove []LabelID) error
    SetFlags(ctx context.Context, ids []ScopedID, update FlagUpdate) error
}

// FlagUpdate is a delta: a nil field is left exactly as it was. See below for why it cannot
// be an absolute Flags.
type FlagUpdate struct {
    Read    *bool
    Starred *bool
}

type Destroyer interface {
    Trash(ctx context.Context, ids []ScopedID) error
    Untrash(ctx context.Context, ids []ScopedID) error
    Delete(ctx context.Context, ids []ScopedID) error
}

type FilterManager interface {
    ListFilters(ctx context.Context) ([]Filter, error)
    CreateFilter(ctx context.Context, f Filter) (Filter, error)
    DeleteFilter(ctx context.Context, id string) error
}

// Settings are four interfaces rather than one — see below for why.
type SettingsManager interface {
    ListSendAs(ctx context.Context) ([]SendAs, error)
    GetVacation(ctx context.Context) (Vacation, error)
    SetVacation(ctx context.Context, v Vacation) error
}

type DelegateManager    interface { ListDelegates(ctx context.Context) ([]Delegate, error) }
type ForwardingReader   interface { GetForwarding(ctx context.Context) (Forwarding, error) }
type IMAPSettingsReader interface { GetIMAPSettings(ctx context.Context) (IMAPSettings, error) }

// Streamer is optional, for attachment readers that can avoid buffering the whole payload.
type Streamer interface {
    StreamAttachment(ctx context.Context, msg ScopedID, attachmentID string) (io.ReadCloser, error)
}
```

There is no `WatchProvider`. Push was dropped along with the interface nothing implemented,
and the reasoning is in [roadmap.md](roadmap.md#push-was-dropped-deliberately).

`Capabilities()` returns a `Set`, and implementations build it with `DerivedCapabilities`,
which computes the set from the interfaces the type actually satisfies. A hand-maintained
list is a list that drifts, and this one is load-bearing.

The MCP layer type-asserts and returns a structured `unsupported_by_provider` error naming
the provider and the capability. `mail_accounts` reports each account's capability set, so a
model can see in advance that filters are not a thing on plain IMAP.

`Capabilities()` must never claim more than the implemented interfaces support. It may claim
*less*: a provider whose sending depends on configuration implements `MessageWriter` to
satisfy the interface, then withholds `send` on an account with no SMTP host, so the
capability set describes what this account can actually do rather than what the type can do
in principle. Overstating is the failure that matters — callers trust the set to decide what
to attempt.

## Labels and folders

This is where naive abstractions break.

- **Gmail** — non-exclusive labels. A message can carry many.
- **Zoho** — exclusive folders *and* non-exclusive labels, both at once.
- **IMAP** — folders only. A message lives in exactly one.
- **Microsoft** — exclusive folders *and* non-exclusive categories, like Zoho. A category has
  no id of its own: Graph identifies one by its display name, on a message and in the master
  list alike, so the label id carries the name.

One `Label` type with an `Exclusive bool` covers all three. Applying an exclusive label moves
the message; applying a non-exclusive one adds to it. `mail_modify` needs no provider
branches, and no provider has to pretend to be something it isn't.

Do not model "archive" as its own operation. It is Gmail-specific vocabulary for removing the
inbox label, and it does not survive contact with IMAP.

### Some labels are not filing

`Exclusive` says a label *moves* the message. It does not say where, and one destination is not
filing at all: the bin. Adding Gmail's `TRASH`, moving into the IMAP mailbox called `Trash`,
into Zoho's Trash folder or Graph's `deleteditems` are four spellings of one act, and it is the
act `destructive` exists to gate. Zoho is where that mattered most for longest: the folder move
was the only route to its bin until a `Destroyer` was written for it, and now that there is one,
`EffectOfApplying` is what keeps the older route gated alongside it.

So the model carries `LabelEffect`, and `LabelManager.EffectOfApplying` is how a provider says
which of its own ids is the bin or the junk folder. It is on the interface rather than in a
table upstream because a table upstream is a table somebody has to remember to extend: the next
provider's bin would be an ordinary folder until it bit someone. Answer it from whatever the
provider actually knows — Gmail's system label ids are their own names, Graph's well-known
folder names resolve to ids in any locale, and IMAP has only the mailbox name it was given.

## Read state and stars are not labels

Gmail keeps both as labels called `UNREAD` and `STARRED`. Nobody else does: Zoho has an
integer for read state and a separate follow-up flag, Exchange has `isRead` and
`flag/flagStatus`, IMAP has `\Seen` and `\Flagged`. So they travel as their own operation,
`SetFlags`, rather than as label ids.

They did not, and the consequences were not symmetric. `mail_modify` translated its `read` and
`starred` arguments into Gmail's two label ids and sent them to `ApplyLabels` on whichever
provider owned the mailbox. Zoho and Microsoft refused them as malformed label ids — confusing
but loud. IMAP, which cannot express a label removal at all, returned success having done
nothing, so "mark these read" and "archive these" reported as done on every IMAP mailbox and
changed none of them. Every provider implemented `SetFlags`; nothing called it.

The update is a **delta**, `FlagUpdate{Read, Starred *bool}`, not a whole `Flags`. An absolute
value cannot say "mark this read and leave the star alone" — it says the message is read *and*
unstarred — so a caller marking twenty messages read would silently clear the stars on them
and be told it worked.

`archive` stays a label change, because that is what it is: removing `INBOX`. A provider with
folders refuses it by name rather than being handed a flag it has no field for.

## Threads

Gmail and Microsoft return a thread id of their own, so their grouping is authoritative —
Exchange assigns a `conversationId` and every message in the conversation carries it. IMAP
does not, and must derive threads from `References` and `In-Reply-To` headers, falling back to
normalized-subject grouping. Zoho is a third case and sits with IMAP rather than with Gmail.

**Zoho threading is derived, and this document has now been wrong about it twice.** It first
said derived, then corrected itself to native on the strength of the published API, and the
correction was the error: running the suite against a real Zoho mailbox showed that mailroom
cannot learn which thread a listed message belongs to.

Zoho does thread mail. `GET /messages/view?threadId=<thread>` returns a conversation's members
and they all carry the thread id. What is missing is the step before it. Neither
`/messages/view` nor `/messages/search` reports a `threadId` on the messages it returns; Zoho
reports one only under `threadedMails=true`, which its own documentation describes as
retrieving "emails that are a part of conversations" — a filter, not an annotation. Measured on
a live mailbox, it cut one folder's first 200 messages to 4 and the inbox's to 137. A listing
that hides most of the mailbox cannot be the listing search pages through, and
`/messages/search` rejects the parameter outright with `EXTRA_PARAM_FOUND`.

So mailroom treats a message's own id as its thread id. Zoho accepts that and answers for it
when the message started the thread; asked for a reply's own id it returns an empty array.
`GetThread` therefore anchors the thread on the message it was reached from, fetched directly,
and merges in whatever the guess turns up — so the answer always contains at least the message
asked about, and `derived_threads` says the grouping was inferred. It used to return the empty
array as the thread, which reads as "there is no conversation here" rather than "this could not
be looked up", and is the worst of the three available answers.

Derived threading is genuinely worse and the model should know: `Thread.Derived bool` travels
with the result. An agent told to "reply to the last message in this thread" should be able
to tell whether "thread" was authoritative or inferred.

## Coverage

| Capability | Gmail | Zoho Mail | IMAP/SMTP | Microsoft Graph |
|---|---|---|---|---|
| Search with provider query syntax | full | full | **none — see below** | full, but `$search` and `$filter` do not compose |
| Threads / conversations | native | **derived — no thread id on any listing** | derived | native |
| Labels | non-exclusive | both | folders only | both |
| Drafts | full | **save, list and discard; no edit, no send — see below** | not implemented | full |
| Send | full | **attachments are uploaded first, unrun — see below** | SMTP | full, up to 3 MB of attachments |
| Attachments | full | in from the message, out through Zoho's upload store | full | file attachments only |
| Batch modify | native | one request per change | one command per mailbox | one request per message |
| Read state and stars | labels | two update modes | `\Seen` / `\Flagged` | `isRead` / `flag` |
| Filter on unread, starred or attachments *alongside* free text | full | refused by name | attachments refused | unread and starred refused by name |
| Trash and permanent delete | native | **move to Trash; `expunge=true` destroys — see below** | one command per mailbox | full |
| Filters / rules | full | **no API — see below** | none | work/school accounts only |
| Aliases, vacation | full | **real send-as list; vacation reads and switches off, cannot switch on — see below** | none | addresses, not send-as; vacation has no subject |
| Delegates | scope not requested | not implemented | none | no Graph v1.0 API |
| Forwarding, IMAP settings | read-only | not implemented, though the account record carries both | none | no Graph v1.0 API |
| Incremental sync | not implemented | not implemented | not implemented | not implemented |

Several of those entries need more than a cell.

**IMAP has no query syntax, and `query` means four different things.** `mail_search`'s
`query` argument is described as provider search syntax, and each provider does something
different with it. Gmail parses it as Gmail search syntax; Graph parses it as KQL over
from, subject and body; Zoho puts it in a `searchKey`; IMAP sends it as `TEXT`, which
RFC 3501 defines as a substring of the raw message with no operator parsing anywhere in the
grammar. So `from:alice is:unread` sent to an IMAP mailbox searches for that literal
seventeen-character string, finds nothing, and reports success. Nothing here fixes that; it is
a property of the argument being one field aimed at four grammars, and it is the reason a
fan-out across mixed providers should use the structured fields rather than `query`.

**IMAP search returned nothing at all, for every query, until this was fixed.** The provider
sent `SEARCH` and read the answer with `AllUIDs`, which is empty for a sequence-number search
— `UID SEARCH` is the one that answers in UIDs, and UIDs are what everything downstream here
addresses by. Every search against every IMAP mailbox came back with no messages and no error.
It is written up here rather than quietly corrected because of how it survived: the
conformance suite's first behavioural assertion *skipped* when a search matched nothing, so a
provider that could never find anything skipped that check and every check that depended on
it, and reported a pass. The suite now fails there instead.

**IMAP cannot filter on attachments, and now says so.** RFC 3501 6.4.4 lists every SEARCH key
and none of them concerns MIME structure; the section goes the other way and permits a server
to exclude non-text body parts from matching entirely. RFC 9051 adds none, and no registered
extension covers it — Gmail's `X-GM-RAW` is the only widely deployed way to ask, and it is
proprietary and capability-gated. The filter was previously dropped in silence, so the whole
mailbox came back with every message in it presented as one carrying an attachment.

**IMAP attachments are addressed by section, not by name.** The manifest keys each attachment
by its IMAP section number — `2`, or `1.2` inside a nested multipart — because that is what a
`FETCH` can name a part by, and because two files of the same name are otherwise
indistinguishable. `GetAttachment` fetches that one section rather than the whole message, so a
small attachment does not cost a download of the large one beside it, and peeks rather than
reads, so downloading a file does not mark the message read. Only the transfer encoding the
part declares is undone; an encoding this does not recognise is an error rather than bytes
passed through as though they were the file.

**Zoho sends attachments by uploading them first, and none of that has ever been run against a
mailbox.** Zoho carries no attachment bytes in the message: files go to
`POST /accounts/{id}/messages/attachments?uploadType=multipart` as a `multipart/form-data` body
of repeated `attach` parts, and the compose call references what came back by the `storeName`,
`attachmentPath` and `attachmentName` triple the [send-with-attachments
page](https://www.zoho.com/mail/help/api/post-send-email-attachment.html) names outright. The
[upload page](https://www.zoho.com/mail/help/api/post-upload-attachments.html) is the other
half. `Send` and `CreateDraft` both go through it, and neither refuses attachments any more.

Three properties, each of them a failure this was built to avoid rather than a feature:

- *Nothing is sent until everything is stored.* The upload precedes the compose call, and an
  upload that fails stops the message — a bad HTTP status, a failing envelope under an HTTP
  200, an answer accounting for fewer files than went up, or an entry missing any of the three
  fields a compose call references it by. A send that quietly dropped a file is exactly what
  the old refusal existed to prevent, and it must not come back by the back door.
- *Refusal is at the lower of two limits, and it says which one.* mailroom caps a message's
  attachments at `mail.MaxAttachmentBytes`, 18 MiB, and Zoho publishes 20 MB for a whole
  message on a personal plan (more on the paid ones). mailroom's is the lower, so mailroom's is
  what refuses and the refusal says so — a caller sent to their Zoho plan would be shrinking
  files against a ceiling that is not in the way. Zoho does not say whether its number counts
  raw bytes or the base64 they become inside a MIME message, so something under mailroom's cap
  can still be refused by Zoho; that refusal is loud and is Zoho's.
- *Everything is attached and nothing is embedded.* `isInline` is a property of the upload
  request rather than of a file inside it, and mailroom never rewrites a body to reference an
  embedded part — a file stored inline would be one the message never mentions. An image that
  was inline on the message it was forwarded from therefore arrives as an ordinary attachment.

What is unverified is the whole of it. Every field name above comes from Zoho's documentation
rather than from watching a mailbox answer, which is the reverse of everything else in this
provider and is the first thing to distrust if a send with attachments misbehaves. Two parts
are weaker still even by that standard, and both are named in the code: whether Zoho accepts
`attachments` alongside `mode=draft` — its save-draft page documents no attachment parameter at
all, and that endpoint has already been seen refusing a send-only key with
`EXTRA_KEY_FOUND_IN_JSON` — and whether it accepts them alongside the
`action`/`referencedMessageId` pair that makes a send a reply. Either would fail as an error
rather than as a quietly incomplete message, which is the direction this has to fail in, but
neither has been watched.

**Zoho drafts are three-fifths of an interface, and the missing two are Zoho's, not
mailroom's.** Saving is `POST /messages` with `mode=draft`, listing is the ordinary folder
listing pointed at Drafts, and discarding is `DELETE /folders/{f}/messages/{m}`. Editing a
stored draft and sending one are not in Zoho's API at all: the published index of message
operations runs to twenty-five calls — send, reply, save draft, two listings, the reads,
eleven modes of `updatemessage`, delete — and none of them rewrites a stored message or puts
one on the wire. Mail360 has `POST /accounts/{key}/drafts/{draftId}`, but that is a different
product on a different host with its own account key and scopes; `mail.zoho.com` has no
`/drafts` route.

Both gaps are refused by name, with the capability still claimed — the same footing `send` was
on while it refused attachments. Each refusal is a workaround declined rather than a method
nobody wrote:

- *Editing* would mean saving a second draft and deleting the first. `UpdateDraft` reports no
  id, so the caller would keep one addressing a deleted message while its replacement sat in
  Drafts under an id nobody has — an edit that reports success and cannot be found afterwards.
- *Sending* would mean reading the draft back and posting it as a fresh send. Zoho's metadata
  endpoint reports `toAddress` and `ccAddress` and carries no bcc field of any spelling, so a
  draft written to a blind copy would go out to fewer people than it was addressed to, with
  nothing in the result saying so, and its attachments would be dropped the same way. Quietly
  changing who receives a message is the one thing a send cannot take back.

Discarding deliberately does not pass `expunge`. It defaults to false, so the draft goes to
Trash and can be recovered: destroying mail outright is what `destructive` gates and `Delete`
is the method that spends it — `expunge=true` here would be that capability arriving through the
`discard` door.

**Deleting a vacation reply that is not there is a 500, not a no-op.** Measured against the
live mailbox:

```
PUT /accounts/{id}  →  500 {"status":{"code":500,"description":"Internal Error"},…}
```

Most mailboxes have no auto-reply most of the time, so the only write this provider supports
failed almost every time it was called, with an error that read like Zoho being broken rather
than like nothing needing to be done. `SetVacation` reads first and reports a request already
satisfied as satisfied. A read that fails is deliberately not treated as "already off": that
would report success for a responder still running.

The other half is still unverified. Switching off a responder that is genuinely on has not
been run, because turning one on to test it needs the dates `SetVacation` refuses to invent,
and a failed switch-off would leave a real mailbox auto-replying.

What the same run did settle: `ListSendAs` reports a real send-as list — the mailbox's own
address came back primary, default and verified, and a second address on Zoho's own domain came
back unverified, which is the distinction the `validated` handling exists to draw.
`GetVacation` reads cleanly, and switching on refuses as designed.

**Zoho trashes and destroys mail through two calls it already publishes.** `Trash` and
`Untrash` are the `moveMessage` mode of `updatemessage`, aimed at the system Trash folder and at
the system Inbox; `Delete` is the same `DELETE` the discard above uses, with `expunge=true`,
which Zoho documents as deleting the message "permanently without moving it to the trash
folder". Both are covered by `ZohoMail.messages.ALL`, which mailroom already requests, so no
linked mailbox has to be consented again for `destructive` to start working.

Three things about it are worth knowing before calling it:

- **A move spends the id the caller is holding.** A Zoho id is `<folder>/<message>` and a move
  rewrites the folder half without telling anybody, so reading a message after trashing it
  addresses a path it has left — a `400 messageId is invalid`, which mailroom reports as
  `not_found`. Find the message again; the id it was trashed by is done.
- **`Untrash` restores to the inbox, not to where the message came from.** Zoho records that a
  message is in Trash and not what folder it was in beforehand, so there is nothing to restore
  to. Graph makes the same guess here for the same reason.
- **`Untrash` does not check that what it restores was in the bin.** The only evidence it could
  check is the folder half of an id that trashing has already invalidated, so the check would
  refuse exactly the round trip it would exist to protect. `moveMessage` addresses messages by
  id alone, which is what makes ignoring that half safe; `Delete` cannot ignore it, because its
  folder travels in the path — which is why destroying a message through an id from before it
  was trashed fails as `not_found` rather than destroying something else.

`Delete` is a request per message, because Zoho publishes no bulk delete, and it parses every id
in a batch before it sends the first one: a batch carrying one unreadable id destroys nothing,
rather than destroying its way up to the bad one and then reporting a parse error.

**Zoho's two endpoints take different parameters, and mailroom used to mix them.** The listing
endpoint, `/messages/view`, documents `status`, `flagid`, `labelid` and `attachedMails` among
sixteen parameters. The search endpoint, `/messages/search`, documents `searchKey`,
`receivedTime`, `start`, `limit` and `includeto`, and no more. A filter parameter sent to the
search endpoint is not an error — it is a parameter that does nothing, and the answer is the
whole mailbox with the search terms applied and the filter dropped in silence. Three of the
four were being sent there. Each is now either served on the listing endpoint or refused by
name. Two of them, `has:attachment` and `label:` by display name, have documented `searchKey`
equivalents and are worth wiring in once the search-expression syntax is settled.

**Zoho's flag mode is `setFlag`, and it names the flag.** It was `changeFlag` with a numeric
id. Zoho documents eleven modes for `updatemessage` and `changeFlag` is not among them, so
starring a message asked for nothing at all. The number is right in the other direction — the
listing endpoint's `flagid` parameter is an integer, 0 flag_not_set through 3 followup — and
Zoho's own response samples use both shapes on adjacent pages, so a flag is written by name,
filtered on by number, and read as whichever arrived.

**Zoho paging can return the same message twice.** It pages by offset — `start` is
1-indexed and advances by exactly the page size, and that arithmetic is right — but the list
underneath is not ordered stably, so the offset does not address what it addressed a moment
ago. Measured over an eight-page walk of ten: one message arrived at page 3 position 8 and
again at page 4 position 0, in every run. Not a delivery race — the head of the list was
identical before and after the walk — and pinning `sortorder` changed nothing.

A repeat implies the other half: a walk that returns one message twice can step over another,
which loses mail rather than merely duplicating it. Zoho therefore declares
`unstable_paging`, and a caller that must see every message exactly once has to deduplicate by
id across pages. Gmail and Microsoft page by opaque cursor and do not have this problem;
neither repeated a message over the same walk.

**A Gmail attachment id is not stable, and cannot be compared.** Two `messages.get` calls
against the same message answer with different attachment ids for the same parts — same
filenames, same bytes, different ids. Measured on a live mailbox: one attachment came back as
`ANGjdJ_Nwd5qs…` and then `ANGjdJ-va_CK-…`. Ids already handed out keep working, so this is a
comparison problem rather than an expiry one, but it means any lookup that matches an
attachment by id silently matches nothing. `GetAttachment` matches on size instead, and where
two parts are the same length it reports no filename rather than risk the other one's.

**Microsoft ids are immutable ids, and every request asks for them.** An ordinary Graph
message id encodes the folder the message is in, so archiving a message or sending a draft
changes its id and an id an agent is holding stops resolving. `Prefer: IdType="ImmutableId"`
is set centrally on every request rather than per call, because an id minted in one mode is
not valid in the other — a single request that forgot the header would hand back an id that
fails everywhere else. It is also what lets `Send` report an id at all: sending is done by
creating a draft and sending that, and the draft's id survives the move into Sent Items.

Two caveats Microsoft documents and one it does not. An immutable id survives a move between
folders but *not* a move into an archive mailbox or an export-and-reimport, and ids are
case-sensitive. What is not documented anywhere is whether personal Microsoft accounts honour
the header at all — the page describing it makes no mention of account types.

**Several of Graph's query shapes here rest on nothing Microsoft has written down.** There is
no filterable-property list for `message` in the documentation at all. The only published
`$filter` examples for messages are on `from`, `receivedDateTime`, `isRead`, `subject` and
`importance` — there is not one official example of filtering on `conversationId`, which is how
threading works, nor on `flag/flagStatus`, which is how starred works, nor on `categories`,
which is how a non-exclusive label is matched. All three are common practice with nothing
behind them. Meanwhile Graph's known-issues page says an unsupported combination of query
parameters "might fail silently", so an ignored filter comes back as a full unfiltered page
that looks exactly like an answer.

`GetThread` has always checked the `conversationId` of what came back rather than trusting the
filter. `Search` now does the same for starred, categories and attachments — the three
predicates with no documented example — on the first page and on every page followed from a
`nextLink`, since a nextLink carries the original query parameters and a filter Graph ignored
once it ignores throughout. Nothing is re-checked that Microsoft publishes an example for.
`Search` still refuses to combine `$search` with `$filter` rather than send a combination
whose behaviour is unstated.

**Ordering is asked for only where Exchange will serve it.** Its documented rule is positional:
every property in `$orderby` must also be in `$filter`, and must appear there before any
property that is not being sorted on. Break it and the whole search fails with
`InefficientFilter`. So the date clauses are written first, and `$orderby=receivedDateTime desc`
is sent only when the filter is empty or led by the date — a query filtered on unread alone
comes back in Exchange's own order rather than failing.

**Message rules and automatic replies were documented here as unavailable on a personal
Microsoft account. Against a real one they read fine.** A live `live.com` mailbox on
22 August 2026 answered `ListFilters` with a rule and `GetVacation` with a populated setting,
neither of them an error. The previous claim — a 403 on both, no scope that fixes it — was
written from Microsoft's documentation and never tested, and it is not what Exchange does.

What remains untested is writing: nothing here has created a message rule or set an automatic
reply on a consumer mailbox, because the live suite is read-mostly by design and somebody's
real auto-reply is not a test fixture. Graph may still refuse those. The refusal path is
implemented and reports `unsupported_by_provider` naming the operation rather than the
capability, so if Exchange does refuse a write, that is what a caller sees.

**Microsoft aliases are addresses, not send-as entries.** Graph v1.0 has no send-as API, so
`mail_settings aliases` reports the mailbox's own address plus the proxy addresses Exchange
routes to it. Everything but the primary is marked unverified, because nothing has established
that Exchange will let the mailbox send from it.

**An Outlook automatic reply has no subject.** `SetVacation` refuses one rather than dropping
it: the reply answers with the original subject and there is no field for anything else, so
accepting a subject would mean quietly not doing what was asked, on a setting nobody looks at
again until somebody outside mentions the reply they got.

**Nothing here does incremental sync**, and none of the three has a sync cursor. mailroom
proxies to the provider on every call rather than holding an index, so there is nothing local
to bring up to date. The row survives because a Gmail history id or an IMAP `UIDNEXT` is one
of the first things somebody looks for, and finding no answer is slower than finding "no".

## Adding a provider

1. Implement `Provider` plus whichever capability interfaces you genuinely support.
2. Register it with the outbound auth flow — OAuth for hosted services, credentials for IMAP.
3. Pass the conformance suite.

### The conformance suite

`internal/provider/conformance` is the contract. It exists because a provider that compiles
is not a provider that works.

It comes in two halves, which matters for how early a provider can be held to it:

**`Static(t, provider)`** needs no credentials and runs in CI on every change. It checks the
claims a provider makes about itself — above all that `Capabilities()` agrees with the
interfaces actually implemented, in both directions. A set that overstates is worse than one
that is merely narrow, because callers trust it to decide what to attempt.

**`QueryTranslation(t, emit, expects)`** needs no credentials either, and checks the half
`Static` and `Live` between them both miss: what the provider *sends*.

This exists because of a bug that passed every other check. Zoho's search expression was built
as `field:contains:value` joined by `&&`, with free text emitted as a bare word carrying no
field at all; Zoho's syntax is `field:value` joined by `::`, with `entire:` for a whole-message
search. Zoho parsed none of it and answered success with zero results, so every plain-language
search against a Zoho mailbox silently found nothing — and the suite passed throughout, because
it tested mailroom's expectations against stubs that answered whatever they were told to. A
stub cannot reject syntax the real service rejects. Nothing in a round trip through one is
evidence about the service at all.

So there is a table of canonical queries in the conformance package, and each provider declares
what its request has to contain for every one of them — or that it refuses the term, which is a
correct answer where the provider genuinely cannot express it. Three properties do the work:

- **Every cell carries a citation.** An `Expectation` with an empty `Why` fails, because an
  expectation written from memory is the thing being guarded against.
- **A term with no cell fails.** Adding a query term forces all four providers to answer for
  it, and a fifth provider cannot be written with a silent hole in it.
- **The request is read off the wire, not off the struct.** The three HTTP providers are
  pointed at a stub and the URL is captured; IMAP's connection is recorded and the `UID SEARCH`
  line is read out of the transcript. That is not pedantry: the client library rewrites
  mailroom's `HEADER "From"` criterion into RFC 3501's `FROM` key, which is a different
  question — the envelope's field rather than the header line — and reading the criteria struct
  would never have shown it.

What it cannot do is confirm that a wire form is *right*. That still comes from the
documentation. What it does is make every translation visible, name it beside its source, and
fail when one is missing.

**`Live(t, harness)`** needs a real mailbox and checks behaviour:

- A search that should match returns something. This one fails rather than skipping, and the
  distinction is not academic: skipping is how a provider that found nothing for every query
  passed this suite for months.
- Pagination terminates, and cursors survive a round trip.
- `Capabilities()` matches the interfaces actually implemented.
- Message IDs returned by `Search` are resolvable by `Get`.
- Applying an exclusive label removes the previous one; a non-exclusive one does not.
- Unsupported operations return `unsupported_by_provider`, never a generic failure.
- Rate-limit responses surface as a typed retryable error, not a string.
- Empty results and errors are distinguishable.

The suite was written before the second provider, so Zoho was built against the contract
rather than the contract being reverse-engineered from Gmail. That ordering is the difference
between a contract and a description.

## Settings, and why the interfaces are split

`SettingsManager` carries only what any provider with settings can be expected to do:
send-as aliases and the vacation responder. Delegation, forwarding and IMAP settings each
live in their own small interface, so a provider implements what it supports and no more.
Bundling them would force stubs that fail at call time, which is the shape this package
exists to avoid.

`CapSettings` is gated on `SettingsManager`, so the optional interfaces are only reachable
through a provider that implements the core one — the static suite checks that, because one
implemented without it is dead code that looks like support and behaves like absence.

Two limits on Gmail worth knowing, both found by running against a real mailbox:

- **Delegation needs `gmail.settings.sharing`**, which mailroom does not request. Adding it
  would force every already-linked mailbox to re-consent, so it is a deliberate omission
  rather than an oversight.
- **Delegation is Workspace-only.** A consumer gmail.com account refuses it outright with
  "access restricted to service accounts that have been delegated domain-wide authority",
  whatever scopes are held. Both cases are reported as `unsupported_by_provider`, since
  neither is something a retry or a re-link can fix.

## Status

| Provider | Static conformance | Live conformance | Last run against a real mailbox |
|---|---|---|---|
| Gmail | passing | **passing**, by hand | not recorded |
| Zoho | passing | **passing**, by hand | 24–25 August 2026 |
| IMAP/SMTP | passing | **passing**, in CI on every test run — genuinely, now | n/a, in-process server |
| Microsoft Graph | passing | **passing**, by hand — 18 pass, 0 fail, 1 skip | 22 August 2026 |

The two passes are not equivalent, and the difference is the most important thing on this
page. IMAP's runs in CI on every change, because the go-imap library ships an in-memory server
and the suite needs no credentials — so a regression there is caught by the next commit. That
claim was false until recently and the way it was false is worth remembering: the behavioural
half skipped rather than failed when a search matched nothing, so a provider that returned
nothing for every query skipped past most of the suite and reported a pass. A contract that
stands down when the implementation finds nothing cannot tell an empty mailbox from a broken
provider, which is the same confusion the contract exists to prevent, one layer up.

**The other three are run by hand.** The live suite is gated behind `MAILROOM_LIVE_ACCOUNT`,
so it does not run in CI, cannot run in CI without somebody's credentials, and is invoked by a
person who has decided to invoke it. Read "passing" in those rows as "it passed the last time
somebody tried", with the date beside it saying when that was. Nothing re-runs them, nothing
watches them, and nothing anywhere in this repository will notice the day one of them stops
being true: a provider can break the morning after its last green run and this table will go
on saying passing until the next person thinks to check. The date column exists so that a
reader can judge how much that is worth rather than having to take the word passing at face
value. Gmail's row has no date because none was recorded at the time, which is its own answer.

Running one:

```sh
set -a; . ./.env; set +a
MAILROOM_LIVE_ACCOUNT=work go test ./internal/app/ -run TestLiveConformance -v
```

Beyond the suite, Gmail has been exercised end to end by hand: a message sent and received, an
attachment round-tripped and compared by hash rather than by size, and the size limits shown
to refuse rather than truncate. That is the strongest evidence any provider here has, and it
is still a person running things rather than something CI would notice breaking.

Zoho can now be linked from the mailboxes page, which is what made the provider reachable at
all: it was implemented and configurable but had no route, so no Zoho mailbox could ever be
stored for it to talk to. Wiring the flow up turned two details of the published API into
things mailroom now depends on and nobody has confirmed — Zoho separates OAuth scopes with
commas rather than the spaces the OAuth 2 library emits, and it refuses the `Bearer` scheme
its own token endpoint names in favour of `Zoho-oauthtoken`. Both are documented behaviour,
both are exercised against a stub, and both have now been confirmed against the real thing —
the live run below could not have authenticated otherwise.

Zoho has now been run against a live mailbox, and the run found three things no stub could
have. They are worth listing, because each is a place where the published API and the service
disagree, or where mailroom had believed something the documentation never said:

- **`messageId` is a string on some endpoints and a bare number on others.** `/messages/view`
  and `/details` quote it; `/content`, `/header` and `/messages/search` do not. Zoho's own
  samples show both. Every id search returned was unopenable, because `Get` decoded the content
  endpoint into a struct expecting the quoted form. Identifiers are now decoded by a type that
  takes either and keeps the digits exactly as they arrived — Zoho's ids are 19 digits and a
  float64 carries 15 to 16, so decoding one as a number silently yields a different id.
- **A missing message is a `400 Invalid Input`, not a 404.** Mapped to `ErrNotFound`, but
  narrowly: 400 is also what Zoho answers for a request it could not parse, and this provider
  has shipped two of those. See the mapping's own comment for the four conditions.
- **Threading is derived**, above.

The capability set is honest: Zoho implements reading, attachments, sending, labels, `draft`
and `discard` since drafts were written, `destructive` since a `Destroyer` was, and `settings`
since the send-as list and the vacation reply were. It deliberately does not claim filters,
because that interface is absent rather than stubbed — and absent for a reason the next section
gives.

**Sending an attachment through Zoho works, and a text file does not come back byte for byte.**
Measured on the live mailbox: a 51-byte `text/plain` attachment was sent, arrived, and fetched
back as 53 bytes — exactly the same content with its line endings converted to CRLF. That is
mail doing what mail does to a text part, not Zoho and not this provider, and it will be true
of any provider here. Worth knowing before somebody compares a checksum across a send and
concludes the file was corrupted. A binary attachment has no line endings to convert.

The draft half was the part expected to fail and does not. Saving a draft that carries an
attachment is undocumented — Zoho publishes no attachment parameter for the save-draft
endpoint at all — and the same endpoint has been seen refusing a send-only key with
`EXTRA_KEY_FOUND_IN_JSON`. It was tried against the live mailbox anyway, and the draft saved
with its file and discarded cleanly.

**A Zoho draft cannot answer a message.** The save-draft endpoint refuses the two fields that
record what a reply replies to — the pair `Send` uses, and the only threading Zoho's message
API offers. Posting them alongside `mode=draft` returns

```
404 {"data":{"errorCode":"EXTRA_KEY_FOUND_IN_JSON"},"status":{"code":404,"description":"Invalid Input"}}
```

which this provider maps to `not_found`, so the caller was told its message did not exist when
nothing was wrong with it. Measured on the live mailbox; the hermetic tests had asserted the
opposite, because a stub accepts whatever body it is handed.

Refused by name rather than saved as an ordinary draft: that would sit in Drafts detached from
the conversation it answers, and nothing would have said so. Zoho documents `inReplyTo` and
`refHeader` as the alternative and both take an RFC 5322 Message-ID, which mailroom does not
hold — its ids are Zoho's own — so this needs the header fetched first rather than a different
field name.

**Zoho attachments work inbound, and both halves of that path were wrong until a message with
one turned up.** The outbound half — uploading a file and referencing it on a send — was
written afterwards, from the documentation, and was not part of this run; it is written up
above, in the Coverage notes.

`Get` reported no manifest, so `mail_get_attachment` could not be reached at all
— and because it could not be reached, nobody had discovered that the download was broken
too. `/attachmentinfo` now supplies the manifest, measured rather than guessed:

```json
{"attachments":[{"attachmentSize":697,"attachmentName":"…","attachmentId":"…"}],"messageId":"…"}
```

`attachmentSize`, not `size`, and no content type anywhere in it. The download is the odd one
out among Zoho's routes: it answers with the file rather than the usual envelope, and refuses
`Accept: application/json` with `406 NOT_ACCEPTABLE`, so it is fetched as raw bytes. The
content type comes from the response, and the filename is percent-decoded — Zoho encodes it
inside a plain `filename=` parameter rather than the `filename*` form that declares an
encoding, so reading it literally gives a worse name than the manifest's.

**Zoho has no settings API; it has an account record.** Both halves of `SettingsManager` come
off the object `GET /api/accounts` answers with — the from-addresses are `sendMailDetails` and
the auto-reply is `vacationResponse` — and writing the auto-reply is a `PUT` back to
`/api/accounts/{accountId}` carrying a `mode`, which is how Zoho's whole
[account surface](https://www.zoho.com/mail/help/api/account-api.html) works. Unlike the rest
of this section, none of it has been run against a live mailbox; it is written from the pages
cited below and from nothing else.

The listing endpoint is read rather than `GET /api/accounts/{accountId}`, and that choice is
made on a documentation discrepancy rather than a preference: the
[all-accounts sample](https://www.zoho.com/mail/help/api/get-all-users-accounts.html) carries
`vacationResponse`, and the
[single-account sample](https://www.zoho.com/mail/help/api/get-user-account-details.html)
shows the same mailbox — same address, same account id — with no auto-reply field anywhere in
it. Reading the endpoint documented to carry the field is the option that cannot report an
auto-reply as absent for somebody who has one. Which one the service really answers with is
unverified. The record is then picked out by account id, because a Zoho login holds several
accounts and Zoho's own sample answers with two, each carrying its own auto-reply.

**Aliases are a real send-as list, and verification is asserted in one direction only.** Zoho
publishes `sendMailDetails` — `fromAddress`, `displayName`, `validated` — which is the answer
to "what may this mailbox put in a From header", so mailroom reads that rather than the
`emailAddress` array beside it, which is the receiving side and is Microsoft's situation
rather than Zoho's. `validated` is documented nowhere beyond appearing in a response sample,
and in that sample it is `false` on the mailbox's own address, alongside
`validationRequired: true`. Reading it alone therefore reports the address the account *is* as
one nothing has established it can send from. So the mailbox's own address — matched against
`primaryEmailAddress` from the same record — is verified, and every other address is verified
only where Zoho said `validated`. An alias Zoho is quiet about stays unverified, because an
alias reported as verified when it is not is a send that is accepted here and fails later.

Two fields are left empty on purpose. There is no reply-to: Zoho has one, and a whole
[`updateReplyToStatus` mode](https://www.zoho.com/mail/help/api/put-update-reply-to-address.html)
for writing it, but no published account response carries a field to read it back and guessing
at a name would decode nothing or the wrong thing. And nothing but the mailbox's own address is
reported as the default, because Zoho publishes no field saying which from-address composing
uses — `status` is the nearest-looking candidate and Zoho's own sample has it true on two rows
at once.

**The vacation reply reads as present-or-absent, and two of its five fields cannot be
answered.** The stored response carries no enabled flag of any spelling; what decides whether a
reply goes out is the date window, and the dates cannot be read, because the page that defines
the format as `MM/DD/YYYY HH:MM:SS` gives `"toDate": "19/05/2024"` as its own sample
([add](https://www.zoho.com/mail/help/api/put-add-vacation-reply.html)). With the field order
contradicted by the page defining it, parsing one to decide "is this in effect today" is a coin
toss dressed as a fact, so a stored response reads as enabled and an expired one is
over-reported — the direction that leaves somebody checking rather than the direction that
tells them an auto-reply they still have is switched off. The restrictions are reported as
unset for the same class of reason: Zoho *writes* the audience as a name (`all`, `contacts`,
`noncontacts`, `org`, `nonOrgAll`, `nonOrgContacts`, `nonOrgNonContacts`) and *reads* it back
as a bare integer, and no page documents which number is which name.

**`SetVacation` switches one off and refuses to switch one on.** Off is exact:
[`deleteVacationReply`](https://www.zoho.com/mail/help/api/put-delete-vacation-reply.html)
takes nothing but the mode under user authentication, so no part of the request is invented. It
removes the stored response rather than disabling it — Zoho has no state for an auto-reply that
exists and is quiet — so the wording does not survive being switched off, and turning one back
on means supplying it again.

On is refused by name, because
[add](https://www.zoho.com/mail/help/api/put-add-vacation-reply.html) and
[update](https://www.zoho.com/mail/help/api/put-update-vacation-reply.html) both make
`fromDate`, `toDate` and `sendingInt` mandatory and mailroom's vacation settings carry no dates
at all. Choosing them means deciding on somebody's behalf when their auto-reply stops answering
their mail — either it goes quiet while they are still away or it answers strangers for a year
after they are back — and nothing in the result would say which was picked. The format
contradiction above makes it worse rather than better: a date mailroom wrote could be read as a
different day from the one it meant. This is the same refusal `CreateDraft` makes about replies
and `Send` makes about attachments.

Switching off is the only write, and it needs `ZohoMail.accounts.UPDATE`, which is now in the
scope list. A mailbox linked before that was asked for holds a token without it, so that one
call will be refused until the mailbox is linked again — a re-link for a setting, not for the
mail. Whether Zoho refuses `deleteVacationReply` on a mailbox that has no vacation reply is
unverified; mailroom sends it either way rather than guessing that it errors.

One thing the live run did not settle and nothing since has. The account record carries
`mailForward` — `mailForwardTo`, `type`, `status` — so a `ForwardingReader` is implementable;
it is not implemented, and it would be read-only if it were, for the reason `mail_settings`
gives: forwarding hands somebody else access to the mail itself.

What the live run did not cover is the `Destroyer` written after it, which reaches Zoho's
documented `DELETE /folders/{f}/messages/{m}` with its `expunge` flag — so applying the Trash
folder as an exclusive label is no longer the only route to binning Zoho mail. Trashing,
restoring and permanent delete have still never been run against a mailbox: the two calls
behind them are Zoho's documented ones, and the hermetic tests establish what mailroom sends
rather than what Zoho does with it — which is the exact shape of evidence this run showed to be
worth little. Two details in particular are waiting on a mailbox. `destfolderId` is sent as a
JSON string, which is how `ApplyLabels` has always sent it and how Zoho spells its own ids on
half its endpoints, while the move endpoint documents a long. And whether a message keeps its
numeric id across a move between folders is not stated anywhere in Zoho's documentation; if it
does not, a message has to be found again after trashing for a stronger reason than the folder
half of its id going stale.

Microsoft was in the position Zoho was in until this run, and is no longer. An Azure app has
since been registered, the round trip is proven end to end — consent at
`login.microsoftonline.com`, the code exchange, the first Graph call — and the live suite has
been run against a real `live.com` mailbox: 18 pass, 0 fail, 1 skip. That run is also what
corrected this page about message rules and automatic replies, above, both of which had been
written from Microsoft's documentation and were not what Exchange does.

It was run once, by hand, on 22 August 2026, and it carries the caveat every by-hand row
carries: nothing re-runs it and nothing would notice the day it stopped being true.

What the hermetic tests establish, and go on establishing on every commit, is what mailroom
sends and what it claims: that every Graph request asks for immutable ids, that a 401 becomes
`auth_expired` so the mailbox is marked rather than retried, that a query Graph cannot express
in one request is refused by name instead of half-answered, that applying a category keeps the
ones already on the message, and that the consent URL carries `offline_access` and the four
Graph scopes at the `common` tenant. What Microsoft actually answers to any of it rests on that
one run.

Unlike Zoho, Microsoft claims the whole capability surface — read, attachments, draft,
discard, send, labels, filters, settings and destructive — because Graph genuinely serves all
nine. Drafting and discarding fall together for every provider, because deleting a draft is a
method on the same interface that saves one; they are two capabilities because a grant is
trusted with them separately, not because a provider implements them separately. The
narrowing is per operation rather than per capability, which is why the refusals above name
an `op`.
