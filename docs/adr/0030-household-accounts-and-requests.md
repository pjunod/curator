# ADR 0030 — standalone household accounts and optional identity links

**Status:** accepted for implementation · **Date:** 2026-11-13.
Read the [approved household plan](../HOUSEHOLD_DISCOVERY_PLAN.md) and
[availability boundary](0029-shared-availability.md) first. This implements the
owner's clarification that both applications are standalone and integrations
are first-class optional connections.

## Account and permission ownership

A permanent Curator account owns requests, subscriptions, notes and approval
history. It has a local username/password, display name, owner/member role and
active/suspended status. Existing configured credentials migrate to the first
local owner without copying or synchronizing a Cinema password. Only verified
password logins upgrade the legacy digest. Local sessions and device sessions
persist as token digests with account, credential generation, method and expiry.
Integration keys remain machine credentials rather than household identities.

The existing open, single-user installation remains usable until household
access is established. Creating an invitation requires a configured local owner,
turns on the durable household authentication boundary and cannot expose an
anonymous owner alongside members. Public routes are enumerated; /auth/* is not
a blanket bypass. Members reach only safe household DTOs and their own account
and requests. Owner administration, paths, credentials, imports, compatibility,
metrics, pairing and events stay outside member capabilities. A request's role
is checked against current local account state on every protected operation.

## Optional Cinema connection

Cinema retains its own account directory and authentication. A Curator account
can explicitly link a verified Cinema issuer UUID and permanent user wire UUID.
Require recent local credential proof and Cinema consent in the same account-
and session-bound state/S256 transaction. Names and emails never link accounts.
The (issuer, subject) binding is unique; collisions reveal no other account.
An unlinked Cinema sign-in follows Curator's ordinary invite/local sign-in flow.

Cinema sign-in intersects local permissions with the household-request grant;
it does not elevate a person to owner. Sensitive account/owner management needs
local reauthentication. Cinema grants stay server-side. Derived web and native
sessions remember the link generation. Unlinking revokes those sessions and
pending handoffs locally before remote revocation, even during a Cinema outage.
Local credentials, local sessions and household history survive. Cinema outages
fail closed for Cinema-derived sessions while local sign-in remains usable.
Curator suspension revokes every method. Account changes atomically preserve the
last active owner and usable local recovery credential.

The delegated contract uses exact registered HTTPS callbacks, confidential
client proof, state, S256, short-lived single-use codes and source-session-bound
opaque grants. Cinema authoritatively checks source revocation and idle expiry
without refreshing login activity. Both standalone SQLite and replicated
Hiqlite implement the same contract. Native uses the system browser and a second
one-use Curator handoff bound to an app-generated S256 verifier and a fresh
completion code returned to the consenting device through `monarr://cinema-complete`.
Polling with the original verifier alone never returns a session. Closing the
app and losing that verifier requires a fresh sign-in. Confirm the displayed
local account before saving the device session.

## Request and acquisition ownership

A request is a durable record with exact kind/provider identity, primary copy,
frozen known-aired episode scope where applicable, public status and independent
subscriptions. Joining shares work without sharing account-private notes. Each
subscriber may withdraw their own pending interest. Owner decline and approval
record local actor identity and visible status.

Approval performs a targeted current filesystem/scope check, freezes the
reviewed destination/profile/policy and atomically claims overlapping targets.
A season and an episode cannot independently authorize the same work. An
already-present scope fulfills without searches. Unknown evidence or changed
identity/policy requires another review. Offloaded episodes are reacquired only
when explicitly included in that approval. Opening discovery or submitting a
request never admits work. Offload intent stays set until verified primary
placement; exact pending notifications are narrowed cumulatively for primary
paths only. Additional-copy monitoring remains suppressed while those flags
remain. The immutable origin freezes full known provider IDs, quality policy,
root identity, leaf coordinates and the approved offload epoch through final
publication. Household-origin failures cannot launch ordinary replacements or
season plans; uncertain submissions retain the existing same-token retry.

Approved work is a durable intent, retried after restart using stable admission
identity. Per-target due order/backoff survives restart and prevents a long
request from starving later leaves. After approval, withdrawal removes interest
and preserves authorized work and its claims; re-review releases claims for
leaves now present. Existing acquisition/download/import services execute it. A request
worker must not call broad search-on-add or enable future/unrequested seasons.
No new downloader, import queue or recovery console is introduced. Withdrawal,
approval, file arrival and admission races must have one committed outcome.

## Alternatives and consequences

Cinema-only identity is rejected because it makes Curator dependent on another
application and splits history when a link is removed. Silent identity merging
is rejected because a display name is not proof. Creating another downloader
is rejected because it duplicates the existing durable acquisition authority.

Local accounts and safe requests can ship before Cinema deployment. The cost is
maintaining an explicit human-session boundary and account recovery rules in
Curator. First rollout targets one household server; revisit tenant boundaries
and federated account discovery only if requirements expand. Child policy,
trusted auto-approval, setup and migration journeys remain later work.
