-- Storage for mailroom. Three tables carry the product: users, accounts and grants.
--
-- Every mailbox and every grant belongs to exactly one user, which is what lets several
-- people share a deployment without sharing a mailbox. Ownership is enforced in the queries
-- rather than only in the handlers, so a missed check in the UI cannot reach another user's
-- mail.
--
-- Credentials are sealed before they arrive here, so a copy of this database without the
-- encryption key is inert.

CREATE TABLE IF NOT EXISTS users (
    id           TEXT PRIMARY KEY,
    -- Identity is keyed on (issuer, subject), not subject alone: subjects are only unique
    -- within an issuer, so two issuers may legitimately use the same one.
    issuer       TEXT NOT NULL,
    subject      TEXT NOT NULL,
    email        TEXT NOT NULL DEFAULT '',
    name         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX IF NOT EXISTS users_identity ON users(issuer, subject);

CREATE TABLE IF NOT EXISTS accounts (
    id          TEXT PRIMARY KEY,
    -- Nullable only so that a database predating multi-user support can be opened. The first
    -- user to sign in adopts any unowned rows; after that every insert sets it.
    owner_id    TEXT REFERENCES users(id),
    -- Not UNIQUE at the column level: that would cover soft-deleted rows too, and unlinking
    -- a mailbox would take its name out of circulation permanently. The partial index below
    -- is the constraint that is actually wanted.
    alias       TEXT NOT NULL,
    address     TEXT NOT NULL,
    provider    TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'linked',
    -- Sealed provider refresh token. AES-256-GCM, bound to the account id as additional
    -- authenticated data so a row cannot be moved between accounts.
    credential  TEXT NOT NULL,
    scopes      TEXT NOT NULL DEFAULT '',
    linked_at   INTEGER NOT NULL,
    synced_at   INTEGER NOT NULL DEFAULT 0,
    deleted_at  INTEGER
);

-- Indexes over owner_id are created by the migration step rather than here: this file runs
-- against tables that may predate the column, and indexing a column that does not exist yet
-- fails the whole startup.

-- An alias names one live mailbox at a time, and is free again once that mailbox is unlinked.
--
-- It used to be reserved forever, on the reasoning that a freed alias pointing at a different
-- mailbox silently changes what older references mean. That reasoning is sound and the cost
-- was too high: re-linking a mailbox under the name it already had — after a provider scope
-- changes, or a token is revoked — is the ordinary case, and it was refused.
--
-- What the reservation was protecting is narrower than it looks. Grants store immutable
-- account ids, so no grant can inherit a reused alias. The one thing that does travel by name
-- is a grant's default_scope selector, which holds bare aliases; point an old alias at a new
-- mailbox and that selector follows it. Tool results name the address alongside the alias for
-- this reason, so a client that is watching can tell.
CREATE UNIQUE INDEX IF NOT EXISTS accounts_alias_live ON accounts(alias) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS clients (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    redirect_uris TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS grants (
    id           TEXT PRIMARY KEY,
    -- The user who approved this grant on the consent screen. A grant can only ever name
    -- mailboxes its owner owns, so this is also the boundary the MCP endpoint enforces.
    owner_id     TEXT REFERENCES users(id),
    client_id    TEXT NOT NULL REFERENCES clients(id),
    label        TEXT NOT NULL,
    -- Immutable account ids, comma separated. Never aliases: renaming a mailbox must not
    -- change who can reach it, and a reused alias must never inherit an older grant.
    accounts     TEXT NOT NULL,
    capabilities TEXT NOT NULL,
    -- How much this client may do on its own: unattended, confirm or hold. Empty on a grant
    -- approved before modes existed, and read as `confirm` — see internal/grant/mode.go. The
    -- column is added by the migration step as well, for a database that predates it.
    mode         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER,
    last_used_at INTEGER,
    revoked_at   INTEGER,
    -- Set when the operator takes a revoked grant off their grants page. The row stays,
    -- because audit_log.grant_id has no foreign key and the audit page resolves a row's
    -- label by joining onto this table: deleting the row would keep every historical row
    -- and blank the name on all of them, which is the audit log surviving and becoming
    -- unreadable. Only a revoked grant can reach this, and nothing loads a removed one.
    deleted_at   INTEGER
);

CREATE INDEX IF NOT EXISTS grants_client ON grants(client_id);

CREATE TABLE IF NOT EXISTS tokens (
    -- SHA-256 of the bearer token. The token itself is never stored, so a database leak does
    -- not hand over live credentials.
    token_hash TEXT PRIMARY KEY,
    grant_id   TEXT NOT NULL REFERENCES grants(id),
    issued_at  INTEGER NOT NULL,
    expires_at INTEGER
);

CREATE INDEX IF NOT EXISTS tokens_grant ON tokens(grant_id);

-- An audit row says what a grant did to a mailbox and how it ended. It may name what a call
-- affected and what that call sent out of a mailbox; it may never hold what was in one. No
-- message body, no snippet, no attachment content, and no subject or sender of a message that
-- was read — an audit log that is also a copy of the mailbox is a liability rather than a
-- control. docs/security.md carries the rule and the reasoning for where it falls.
CREATE TABLE IF NOT EXISTS audit_log (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    -- Denormalised from the grant so the audit page can be scoped without a join, and so a
    -- row survives legibly if its grant is ever removed.
    owner_id   TEXT,
    grant_id   TEXT,
    account_id TEXT,
    tool       TEXT NOT NULL,
    outcome    TEXT NOT NULL,
    at         INTEGER NOT NULL,

    -- The four below arrived after the first release, and all four are nullable with no
    -- default for one reason: a row written before them holds NULL, and NULL is the one value
    -- the writer never produces. That is what lets the page tell "this call recorded nothing
    -- here" from "this row predates the log recording anything at all", rather than drawing
    -- both as a blank where a reader expects a fact.
    --
    -- Capability, reason and affected are columns rather than keys inside `detail` because
    -- they mean the same thing for every tool and are what anybody would filter or aggregate
    -- on: "what has this grant actually used" is a query over capability, and the grant index
    -- below already carries the other half of it. Everything one tool knows and its
    -- neighbours do not is JSON in `detail`, because a column per tool does not survive the
    -- next tool.
    capability TEXT,
    reason     TEXT,
    affected   INTEGER,
    detail     TEXT
);

CREATE INDEX IF NOT EXISTS audit_at ON audit_log(at DESC);
CREATE INDEX IF NOT EXISTS audit_grant ON audit_log(grant_id, at DESC);


-- Invites exist so an operator can hand somebody an account without running an identity
-- provider and without opening the instance to everyone the issuer authenticates.
CREATE TABLE IF NOT EXISTS invites (
    id          TEXT PRIMARY KEY,
    -- SHA-256 of the code. The code is a credential: it is shown once, at creation, and a
    -- copy of this database hands over no usable invites.
    code_hash   TEXT NOT NULL UNIQUE,
    note        TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    expires_at  INTEGER,
    -- An invite binds to whoever redeems it rather than to an address named in advance. An
    -- issuer that permits unverified addresses would otherwise let one person claim an
    -- invite intended for another simply by asserting their email.
    redeemed_by TEXT,
    redeemed_at INTEGER,
    revoked_at  INTEGER,
    -- Set when this invite moves an existing user onto a new login rather than creating a
    -- new account. Redeeming it rewrites that row's identity, so the mailboxes, grants and
    -- audit history it already owns come along.
    adopts_user_id TEXT
);

CREATE INDEX IF NOT EXISTS invites_created ON invites(created_at DESC);


-- Attachment bytes are held on disk, not here; this table is everything that decides who may
-- reach them. Rows are short-lived by design — a blob is a copy of mail that already exists
-- in a mailbox, or a file staged for one send — so the sweeper deletes both halves as soon as
-- expires_at passes, and nothing here is meant to survive a day.
CREATE TABLE IF NOT EXISTS blobs (
    id         TEXT PRIMARY KEY,
    owner_id   TEXT NOT NULL,
    -- The grant that minted this. Every fetch re-reads it, so revoking a grant kills the
    -- links it handed out rather than leaving them live until their own expiry.
    grant_id   TEXT NOT NULL,
    -- 'mail' for a copy of something already in a mailbox, 'upload' for bytes a client sent.
    -- It decides which capability reaches the blob, so it is stored rather than guessed.
    kind       TEXT NOT NULL,
    -- 'pending' -> 'uploading' -> 'ready'. An upload URL may be used once, which is enforced
    -- by the move out of 'pending' being a conditional UPDATE.
    state      TEXT NOT NULL,
    -- The mailbox a copy came from, so a fetch can check the grant still covers it. Empty
    -- for an upload, which came from no mailbox.
    account_id TEXT NOT NULL DEFAULT '',
    filename   TEXT NOT NULL DEFAULT '',
    mime_type  TEXT NOT NULL DEFAULT '',
    size       INTEGER NOT NULL DEFAULT 0,
    -- Disk a pending upload is charged for before its bytes arrive. Without it, minting a
    -- hundred upload URLs would pass every quota check because the store is still empty.
    reserved   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS blobs_expiry ON blobs(expires_at);
CREATE INDEX IF NOT EXISTS blobs_owner ON blobs(owner_id);


-- Privileged calls a client in `hold` mode made, which this server declined to perform until
-- their owner answers them. Unlike the audit log, a row here carries content — the composed
-- message, its attachment bytes, the filter to create — because it is an instruction that has
-- not been carried out yet and cannot be carried out later without being kept. Answering one
-- clears the payload in the same statement that resolves it, so a dealt-with row holds only
-- its one-line summary.
--
-- An action nobody answers is reclaimed the same way once it is past its TTL, resolved as
-- `expired`: this is the only table here that stores message bodies, so it is the only one
-- whose contents are bounded by a clock rather than by whoever gets round to the page.
CREATE TABLE IF NOT EXISTS held_actions (
    id          TEXT PRIMARY KEY,
    owner_id    TEXT NOT NULL,
    -- The grant whose client asked. Kept so the page can name it, and so the per-grant cap on
    -- unanswered actions can be counted without a join.
    grant_id    TEXT NOT NULL,
    account_id  TEXT NOT NULL,
    -- The audit name of the call that was held, so every row this action writes over its life
    -- names the same tool the client called.
    tool        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    summary     TEXT NOT NULL,
    payload     TEXT NOT NULL,
    created_at  INTEGER NOT NULL,
    -- NULL while it is waiting. The conditional UPDATE onto this column is the whole of the
    -- double-approval defence: two tabs pressing Approve race on it and one of them loses.
    resolved_at INTEGER,
    resolution  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS held_owner ON held_actions(owner_id, created_at DESC);
CREATE INDEX IF NOT EXISTS held_grant ON held_actions(grant_id, resolved_at);

-- The reclaimer's index: instance-wide, oldest unanswered first. Partial, so it holds only
-- the rows that are actually waiting and shrinks as they are answered — on an install where
-- nothing is queued the sweep touches an empty index rather than scanning the history.
--
-- Both columns have been on this table since it shipped, which is why this can live here
-- rather than in migrate(). An index in this file over a column a migration adds is the bug
-- that broke every populated database while every fresh one looked fine; see users.go.
CREATE INDEX IF NOT EXISTS held_unanswered ON held_actions(created_at) WHERE resolved_at IS NULL;
