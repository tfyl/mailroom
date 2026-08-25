# Grants

A grant is the unit of access. When an MCP client connects it does not get "your mail" — it
gets a named, scoped, revocable grant that you approved on a consent screen.

Every grant belongs to the user who approved it, and can only name mailboxes that user owns.
On a shared instance this is the boundary that matters: a token issued to one person's agent
cannot reach another person's mail, whatever account id it presents.

One grant per project is the intended pattern. A research agent gets read on one mailbox; an
inbox-triage agent gets read and labels on two; nothing gets `send` until you decide it does.

## The record

```go
type Grant struct {
    ID         ID
    OwnerID    user.ID          // the user who approved it
    ClientID   string           // from dynamic client registration
    Label      string           // "Claude — work triage"
    Accounts   []mail.AccountID // immutable IDs, not aliases. An explicit allowlist, never "all"
    Caps       mail.Set
    Mode       Mode             // how much it may do on its own: unattended, confirm, hold
    CreatedAt  time.Time
    ExpiresAt  *time.Time
    LastUsedAt *time.Time
    RevokedAt  *time.Time
}
```

The issued access token carries an opaque `grant_id` and nothing else. That keeps tokens
small, makes revocation instant (delete the row, every token referencing it dies), and gives
every audit entry something to join on.

`Accounts` is an explicit allowlist. There is deliberately no "all accounts" value — a grant
written when you had two mailboxes should not silently widen when you link a third.

## Capabilities

Capabilities split where *trust* changes, not where the API does.

| Capability | Grants | Withholds |
|---|---|---|
| `read` | Search, get message, get thread, list labels | Any state change; attachment bodies |
| `attachments` | A signed URL to download an attachment's contents | Uploading anything |
| `draft` | Create and update drafts; compose replies; upload a file to attach | Sending them; deleting them |
| `discard` | Delete a draft | Everything else about a draft, and any message that is not one |
| `send` | Send, reply, forward, send an existing draft; upload a file to attach | — |
| `labels` | Apply and remove labels, archive, mark read, star, batch modify, create labels, delete a label that is only a label | Trashing or deleting messages, *including by applying a label that bins* — and *including deleting a label that is really a folder*, which takes the mail inside it |
| `filters` | Create, list, delete filters and rules | Other settings |
| `settings` | Reading aliases, vacation responder, forwarding, delegates and IMAP settings; setting the vacation responder | Changing anything but the vacation responder |
| `destructive` | Trash, permanent delete, batch delete; applying a label whose effect is any of those | — |

**`draft` without `send` is the important one.** It is the setting you want for most agent
work — the agent does everything up to the irreversible step, and a human presses the button.
No other mail MCP server can express it, because none of them separate the two.

`destructive` is separate from `labels` for the same reason. Archiving is recoverable;
permanent delete is not, and they should not travel together.

That separation is on the *effect*, not on the tool, and it has to be: applying a label is how
every provider here bins a message. Gmail's `BatchModify` moves the mail when `TRASH` is added,
and on IMAP, Zoho and Graph applying an exclusive label *is* a move, so naming the bin folder
is the same request `mail_trash` sends. A `labels` grant used to be able to do all of that, and
`hold` did not queue any of it. Now a label operation whose effect is destruction needs
`destructive` as well as `labels`, and is held on the same terms as `mail_trash`.

**This narrows grants that already exist.** Nothing back-fills `destructive`, so a grant
holding `labels` alone loses an ability it had yesterday: it can still file, archive, star and
mark read, and a call that bins is now refused naming `destructive` as what is missing. That is
the same call `discard` faced when it was split out of `draft`, and it is the same answer —
the narrower grant is the one the operator thought they were approving, and the refusal says
plainly what to widen if they did not.

**`discard` is the same cut, one level down.** `draft` used to mean create, edit *and*
delete, which made composing and destroying one decision: an agent trusted to write a reply
was thereby trusted to remove a draft a person had written, and the consent screen said so in
as many words. They are now two boxes. `draft` writes and revises; `discard` throws away.

Deleting a *draft* is `discard` rather than `destructive` because the two destroy different
things. A draft is unsent text in a mailbox this grant already composes into — most often
text the agent wrote itself — and tidying up after itself is ordinary work for a drafting
agent. A received message is not the agent's to lose. Keeping them apart also keeps
`destructive` meaning one thing; folding draft deletion into it would have widened every
grant that already holds it, silently, without anybody approving the wider version.

**Sending a draft is not discarding it.** Every provider removes a draft as part of
delivering it, and that removal belongs to `send`. `mail_send` with a `draft_id` needs `send`
and nothing else, which is what stops this split from breaking the sending grant that already
exists.

### What this did to the grants that already existed

Nothing re-grants `discard`. A grant is stored as a comma-separated capability list, so every
grant approved before this change reads back as exactly the words in its row, and none of
them is `discard`.

**An agent that could delete a draft yesterday is refused today.** That is deliberate and it
is the safe direction: the operator approved a box that said "create, edit and delete", and
the honest reading of a split is that the half nobody has since ticked was not granted.
Quietly adding `discard` to every grant holding `draft` would preserve behaviour by granting
a permission no one approved, which is the thing this product exists to make impossible.

The refusal is legible rather than mysterious — `scope_denied` names `discard` — and the
remedy is one edit: tick `discard` on the grants page. That goes through the widening
confirmation, and the button names it for the same reason it names `send` and `destructive`,
because a discarded draft is not coming back.

`attachments` is separate from `read` because reading a message body and pulling down a
contract or a spreadsheet of customer records are different risks. A message listing still
shows attachment names and sizes under `read`; fetching the bytes needs the extra tick.

Uploading answers to `draft` or `send` rather than to `attachments`, which reads mail out.
Minting an upload URL writes bytes to this server's disk, and the only thing those bytes can
ever be used for is being attached to a message — so the permission that reaches them is the
one that could attach them. A grant holding `attachments` alone can download files and cannot
put one anywhere.

## Modes

Capabilities answer whether an action is within a client's reach. A **mode** answers what has
to happen before it is taken. The same grant that suits a nightly digest job — read, labels,
`send`, nobody watching — is the wrong shape for an agent improvising in an inbox, and the
difference between them is not which permissions they hold. It is identical. The difference is
whether a human is in the loop.

There are three, set per grant on the consent screen and changeable from the grants page.

| Mode | The client is told | The server enforces |
|---|---|---|
| `unattended` | Finish the job without checking back, sending included | nothing |
| `confirm` *(default)* | Put sending, deleting, filters and the vacation responder to your human and wait for an answer | nothing |
| `hold` | Those four are queued for the mailbox's owner and not carried out | **those four do not happen** |

**Two of the three are wording and nothing else, and the UI says so on every screen that
offers them.** The wording is the text of each tool's `Description` — the only thing on this
server that talks to the model rather than to the code. It is real, it works on a well-behaved
client, and it stops nothing: a model that reads "wait for an answer" and sends anyway is not
prevented by anything. `internal/mcp/steering.go` holds it, and is written as instructions
about *when to act* rather than as caution, because "be careful with sending" tells a model to
be careful and leaves it to decide what that means.

`hold` is the one with teeth. Under it, `mail_send`, `mail_trash` (trash and delete),
`mail_filters` (create and delete) and `mail_settings` (`set_vacation`) do not reach the
provider at all. The call is authorized as usual, the instruction is recorded complete — the
message with its attachments already resolved, the ids to delete, the filter to create — and
the tool answers `held: true` with the id it was queued under. It waits on `/held`, behind the
operator's own session, until they approve or discard it — or until it expires unanswered,
which by default is three days later. Nothing an MCP client can do reaches that page. See
[What a held action stores, and for how long](#what-a-held-action-stores-and-for-how-long).

`mail_modify` is held too, for the half of it that is not filing: a label whose effect is
destruction — Gmail's `TRASH` or `SPAM`, a folder that is the bin or junk on the providers with
folders — is trashing under another name, so it needs `destructive` as well as `labels` and is
queued exactly as `mail_trash` is. The whole call is held rather than the destructive part of
it, because a change split into a half that happened and a half that is waiting is one nobody
can report or decide about. See [tools.md](tools.md#labels-that-destroy-mail).

Four things are deliberately *not* held. `untrash` only puts mail back. `mail_draft` is what
an agent under `hold` is meant to do instead, so holding it would leave the mode with no way to
make progress — `discard` included, because a draft is not mail anybody has received. Ordinary
filing — labels, archive, star, read — is reversible. And every read runs normally.

### What a held action stores, and for how long

A held send holds the message. The row carries the recipients, the subject, the body and the
attachment bytes, as JSON in a `TEXT` column of the same SQLite database as everything else —
**not encrypted**, unlike a provider credential, and unlike anything the audit log is allowed
to carry. That is deliberate rather than an oversight: this is not a record of mail that
exists, it is mail that does not exist yet and cannot be sent later without being kept whole.
A held trash, filter or vacation change holds far less, because for those the instruction is
the ids or the rule and there is no body involved.

Answering an action drops the payload in the same statement that resolves it. What survives on
the page afterwards is the one-line summary, the grant that asked and what was decided.

#### How long a held action waits

**Three days, then it expires.** `MAILROOM_HELD_TTL` sets it — anything from `5m` to `720h`,
or `off` to keep the original behaviour of waiting indefinitely, which logs a warning at
startup because of what the paragraph above says is sitting in the row.

An action is a question put to a person who is expected to answer it, so the useful life of
one is measured in hours; three days is chosen so a weekend fits inside it. Something nobody
has looked at after that is abandoned rather than pending, and a queue of abandoned messages
is a copy of somebody's outbound mail with no end date on it.

What expiry does, exactly:

- The payload is cleared — the message, its recipients and its attachment bytes are gone from
  the database.
- The row stays, resolved as `expired`, and appears in the closed list under `/held` beside
  the ones that were approved and discarded. The record of *what a client asked for* is the
  cheap half and it is worth keeping; the mail is the expensive half and it goes. Losing the
  row entirely would leave an audit trail that quietly forgets the questions nobody answered,
  which is the wrong half to lose.
- **It cannot be approved afterwards, and cannot be discarded either.** There is nothing left
  to approve. The enforcement is not a check that could be missed: every path to a payload
  goes through one conditional `UPDATE`, and expiry writes the same column that `UPDATE` is
  conditional on — so an expired action loses that race exactly the way a second browser tab
  pressing Approve does, and gets the same answer.
- It stops counting against `MaxPending`, the per-grant cap on unanswered actions, so a grant
  whose queue is fifty abandoned rows can still queue something somebody will read.

Nothing is served past its TTL even between sweeps: the cutoff is part of every query, so an
expired action is neither listed nor answerable from the instant it lapses, whether or not the
sweeper has reached it. The sweeper runs every five minutes and once at startup — a restart
after downtime is exactly when the queue is holding mail that should already be gone — and
opening `/held` reclaims anything expired on the way past.

The client is told. A held result carries `expires_at` alongside the action id, so an agent
that queued something has the one fact about the queue it cannot look up: when "still waiting
for approval" stops being a true thing to tell somebody.

### Why not elicitation

MCP has a capability for asking the human through the client, and it is the obvious thing to
reach for. It does not work here, for two reasons and neither is fixable from this side.

It is negotiated per session, and mailroom's transport is deliberately stateless — every
request carries its own bearer token and no session survives between them, which is what lets
a client reconnect or be load-balanced without re-establishing anything. The Go SDK synthesises
the initialize parameters for a stateless request with no capabilities at all, so
`ServerSession.Elicit` answers "client does not support elicitation" on every request this
server will ever serve. Making it work means giving up stateless sessions for a control that
then only exists while a connection does.

The second reason is why the first is not worth fixing: a client that cannot elicit has to be
either refused everything or allowed to proceed. Proceeding is an opt-out operated by the
party being controlled. Everything mailroom knows about the far end of an MCP connection
arrived from the far end of that connection, which is exactly why the question is asked
somewhere the client has no route to.

### Changing a mode

A mode moves the same way a scope does, and is treated the same way in both directions.
Tightening one takes initiative away and applies the moment you save. **Loosening one hands
the client more autonomy than it had, with nobody at that end asked again** — the same shape as
adding a capability — so it goes through the widen page, which itemises it and names the
destination in the button: "Set it to unattended". Both directions are written to the audit
log as `grant.edit`, `mode confirm → hold`.

Coming off `hold` decides what happens next and releases nothing: everything already queued
still has to be answered one at a time. The widen page says so, because it is the thing an
operator is most likely to assume otherwise.

### A grant with no mode

Every grant approved before modes existed carries nothing in the column, and behaves as
`confirm`. The value is not backfilled — writing `confirm` into those rows would claim
somebody chose it — and `grant.Mode`'s own methods resolve an empty or unrecognised value, so
an unset mode behaves as the default everywhere rather than at the call sites that remembered
to check. Editing something else about such a grant leaves the column alone.

### The mode is not the client's to change

An agent that can widen its own autonomy has no mode at all. The column has exactly two
writers, `CreateGrant` and `EditGrant`, and both are reached only from a browser handler behind
an authenticated session and a CSRF token. Nothing in `internal/mcp` writes a grant, no tool
takes a mode-shaped argument, and the grant is re-read from the store on every request rather
than trusted from the token.

`TestAClientCannotChangeItsOwnMode` drives every tool the server offers, over the real
protocol, with `mode`, `grant_mode`, `autonomy`, `hold` and `held` arguments attached, then
asks the two questions that matter: is the grant still what it was, and is a send afterwards
still held.

## Aliases are labels, not keys

A grant stores immutable account IDs. The alias — `work`, `personal` — is a mutable label
resolved at call time.

This matters because the alternative is quietly dangerous. If grants stored the alias,
renaming a mailbox would break every grant naming it; worse, deleting a mailbox and
nicknaming a *different* one `work` would silently hand every old grant access to a mailbox
nobody approved it for.

Storing the ID means renames are free, and a newly linked mailbox is always a new thing that
no existing grant reaches. The only cost is that an agent whose prompt hardcodes an old alias
must re-resolve it, which is what `mail_accounts` is for.

Aliases are never reused after deletion: unlinking is a soft delete, so the row survives
still holding its name and nothing else can take it.

A rename does release the old name. Rename from the mailboxes page and the previous alias
becomes available again, so a caller still using it may later find a different mailbox rather
than none. That is why every tool result names the address beside the alias — the alias says
what you asked for, the address says what you got. Access is unaffected either way, because
grants store ids.

## Consent

```
client  POST /register                    → client_id
client  open /authorize?client_id&PKCE    → browser
you     authenticate                       (OIDC, or forward-auth)
you     consent form: accounts, capabilities, mode, expiry, label
you     POST approve + CSRF token
client  ← redirect with code
client  POST /token + PKCE verifier       → access token bound to grant_id
```

The consent form is the scoping UI. It shows every linked account and every capability as
explicit checkboxes, defaulting to none ticked. Prefilling `read` would be friendlier and is
the wrong default for something people self-host.

Requesting clients may *suggest* a scope. The form treats that as a suggestion to display,
never as a preselection.

The mode is the one group on that form that arrives with something selected, and it has to be:
a grant has a mode whether or not anybody chooses one, so an empty radiogroup would misdescribe
what Approve does. It arrives on `confirm`.

## Editing a grant

A grant is decided on the consent screen, and it can be changed afterwards from the grants
page: which mailboxes it covers, which capabilities it holds, its mode, and when it expires.
Everything else about it is fixed — its owner, the client that holds it, when it was approved.

The client keeps its token. That is the point: before this existed, the only thing that could
be done to a grant that was slightly wrong was to revoke it, which costs the client its token
and needs whoever runs the client to walk the whole authorization flow again. An agent that
now needs a second mailbox should not require the person who wrote its config to be at their
desk.

Nothing about the token changes, and nothing about the token needs to. A token carries an
opaque grant id and the grant is re-read on every call, so an edit takes effect on the next
call in exactly the way a revocation does.

### Widening is not the same act as narrowing

Taking something away — dropping a mailbox, removing a capability, bringing the expiry
forward — leaves the token with strictly less than it had. It applies as soon as you save,
and the person who did it can put it back from the same page.

Handing something over is different, and the interface says so. It is the one operation in
the product where what an already-issued token may do grows without anyone at the client end
approving anything: the token was issued under the old terms, and the operator is changing
them from this side. So it goes through a page that itemises exactly what is being added,
and applies nothing until that page is submitted.

`send`, `destructive` and `discard` get one thing more, and one thing only: the button names
them. Every other capability can be taken back and leaves nothing behind, but mail that has
been sent stays sent, mail that has been deleted stays deleted and a draft that has been
discarded stays discarded, so taking the capability away afterwards reaches none of it. The
test for this group is whether removing the permission later reaches what was done under it,
which is not the same test as the privileged flag on the consent screen — `discard` is
ordinary there and named here. A second confirmation dialog was considered and rejected — a
question asked twice about the same submission is clicked through within a week, whereas a
button reading "Grant send and destructive" is read every time, because it is the thing being
pressed.

### What it cannot do

- **Attach a mailbox the operator does not own.** The same check `CreateGrant` makes, in the
  same place, against the signed-in user rather than anything the form said. Mailbox ids are
  not secret — they appear in tool results and in the audit log — so posting one straight at
  the endpoint is the obvious thing to try, and it gets "not yours to grant".
- **Reach another user's grant.** The grant is loaded from the owner-scoped list, and the
  update is scoped again in its `WHERE`. A grant that is not yours is reported as missing
  rather than forbidden, so guessing an id learns nothing.
- **Revive a revoked grant.** Revocation is documented above as the thing that cannot be
  undone, and an edit that brought one back would make that untrue.
- **Set an expiry the consent screen would refuse.** Both read it through the same
  `grant.ParseExpiry`, so there is one bound rather than two that can drift.
- **Set a mode this build does not have.** Both read it through the same `grant.ParseMode`,
  which refuses an unrecognised value rather than resolving it — a form naming one has drifted
  from the server, and quietly landing on the default would hide that while leaving the
  operator believing they had set what they picked. An *absent* field leaves the mode alone,
  so a submission from somewhere else cannot reset a grant to the default by omission.

Every edit writes to the audit log under the tool name `grant.edit`: a row per mailbox added
or removed, carrying the account id so the page renders whatever alias it has now, and a row
for the capability change and the new expiry. Reading a grant afterwards shows where it ended
up and says nothing about it ever having moved, which is the gap those rows close.

## Enforcement

Every tool call resolves its grant before touching a provider:

1. Is the grant valid — not expired, not revoked?
2. Does the account belong to the grant's owner? A mailbox owned by anyone else resolves as
   missing, so guessing an id gets a not-found rather than access.
3. Does it name the requested account, or is the call fanning out to a subset it names?
4. Does it hold the capability the tool requires?

A refusal returns a structured `scope_denied` error naming the missing capability and the
account, so the model can tell the user "I would need `send` on `work` for that" rather than
retrying blindly.

`mail_accounts` exists so the model can see its own scope up front and never propose an
action the grant will refuse.

## Content constraints are deliberately not a feature

A tempting addition is a filter narrowing what a grant sees *inside* an account it already
reaches — "this project may read `work`, but only messages labelled `clients`".

mailroom does not do this, and the reason is worth recording so it does not get added later
by someone who has not thought it through.

Such a filter can only be enforced honestly if it can be pushed down into the provider's own
query. When it cannot — and across Gmail, Zoho and IMAP it frequently cannot, since their
search syntaxes agree on almost nothing — the server must fetch the messages and filter them
out in memory. At that point it has already read the mail the grant supposedly forbade. A
boundary that the server crosses in order to enforce it is not a boundary; it is a display
filter wearing a security label.

The honest alternative is available and simpler: **scope by account.** If an agent should see
only client mail, give it a mailbox that only contains client mail. Account-level scoping is
enforceable at the credential, not in application logic.

If this is revisited, the only acceptable form is a small canonical predicate set where every
predicate is natively enforceable by every provider, with a provider that cannot enforce one
refusing the grant rather than falling back to in-process filtering.

## Revocation and audit

The grants page lists every grant, what it reaches, when it was last used, and a revoke
button. Revocation is immediate.

Revoked grants accumulate, so they can be removed: one at a time from the revoked band, or
all at once from the block underneath it. **Only a revoked grant can be removed** — that is
the predicate on the query, not a check in a handler. A live grant is removable only by
revoking it first, which is the step that asks and explains what breaks; an expired grant
does not qualify either, because it is a single edit — a new expiry — from working again, and
removing one would end an authorisation without ever asking that question.

Removing is a **soft delete**, and the audit log is the reason. `audit_log.grant_id` carries
no foreign key, and the audit page resolves each row's name by joining onto `grants`. Deleting
the row would therefore keep every historical row and blank the grant name on all of them: the
history would survive and stop being readable, which is the opposite of what that page is for.
So the row stays, marked `deleted_at` and loaded by nothing — no listing, no lookup by id, no
token — and what it is now is the name its own audit rows are read under. The UI says as much
rather than claiming the record is destroyed, which is the same bargain unlinking a mailbox
makes and for the same reason.

A removal takes with it what the grant left behind: its token rows are deleted (they stopped
resolving at revocation, so this loses nothing), and its attachment blobs are expired so the
sweeper deletes the bytes and then the rows. Attachment links were already dead — every fetch
re-reads the minting grant, and a removed one is not there to read.

Every tool call writes an audit row, and so does every call the gate refuses: grant, account,
tool, outcome, timestamp, the capability it spent, how many things it affected, and — opened
per row on the page — what those things were. For a send that means the recipients and the
subject, because a log recording that mail was sent and not to whom is no use the first time
an agent does something surprising, which is the moment this log exists for.

A call that a grant's mode held rather than performed is recorded with the outcome `held`,
which is neither a success nor a refusal, and carries everything the performed call would have
carried — the recipients, the subject, the ids — because what a client asked for is the fact
worth recording at the moment it asked. The answer to it, `ok` or `declined` or a provider
error, is written under the same tool name when the operator gives one.

It never holds a message body, and never the subject or sender of a message that was read.
[security.md](security.md#the-rule-about-what-may-go-in-it) carries the whole rule and the
argument for where it falls; the short version is that a log which is also a copy of your
mailbox is a liability rather than a control.

The held queue is the one place in the database that does carry a message body, and only
because an instruction that has not been carried out cannot be carried out later without being
kept. Answering an action drops the payload in the same statement that resolves it, so what
survives is the one-line summary and nothing else.
