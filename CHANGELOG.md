# Changelog

All notable changes to the Aether Go SDK are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.6.0]

Additive release — no breaking changes. Existing code continues to work
unchanged; every new method, option, and type is opt-in.

### Added

- **Durable conversation threads.** `Client.AppendThread(ctx, threadID, ThreadAppendInput)`
  and `Client.GetThread(ctx, threadID, opts...)` store and replay an ordered
  message history (`ConversationThread`) for an agent or chat session; the
  server assigns each turn's index atomically. Read options
  `WithLastNThreadTurns` / `WithRecentThreadTurns` bound the replay, and the
  `WithSearchThreadID` search option restricts semantic retrieval to one
  conversation. `Memory.Thread(threadID)` (or `NewThread`) is the ergonomic
  facade, with `Append`, `Context`, and `ThreadID`.
- **Thread lifecycle.** Whole-thread operations — `Client.ThreadRestore`,
  `Client.ThreadSetACL(ctx, id, readers *[]string)`,
  `Client.ThreadMove(ctx, id, expectPartition, toPartition *string)`, and
  `Client.ThreadDelete(ctx, id, hard bool)` — each returning a uniform
  `*ThreadLifecycleResult` (`Status`, `ThreadID`, `Turns`), with the same
  operations on the `Thread` facade as `Restore`, `SetACL`, `Move`, and
  `Delete`. Every operation sends an `Idempotency-Key`. Turn text is never
  rewritten — an edit appends a correction turn — and deletes are soft by
  default; `hard = true` is an irreversible erasure.
- **Shared grounding provenance receipts.** `Client.CreateGroundingReceipt` and
  `Client.RevokeGroundingReceipt(ctx, receiptID)` bind a generated answer to its
  declared sources with signed evidence. New `GroundingReceipt`,
  `GroundingBinding`, `GroundingSource`, `GroundingTrustSignal`,
  `GroundingSetAttestation`, `ReceiptAttestation`, and `ShareableReceipt` types.
- **Multimodal memory.** `Memory.RememberImage(ctx, source, opts...)` and
  `Memory.RememberAudio(ctx, source, opts...)` remember image and audio content
  (bytes, a path, or a URL) so it is recalled alongside text. `MemoryMediaOption`
  helpers: `WithMediaCaption`, `WithMediaTranscript`,
  `WithMediaAutoTranscription`, `WithMediaContentType`, `WithMediaFilename`,
  `WithMediaMetadata`, `WithMediaSource`. Media results surface as
  `MediaMemoryRecord`.
- **Connections API + connect sessions.** Attach an end user's external account
  (Dropbox today) to their partition from your own backend, without the portal:
  - `Client.CreateConnectSession(ctx, CreateConnectSessionOptions{...})` (provider,
    external user id, return URL, optional target partition) mints a hosted
    OAuth entry point and returns a `*ConnectSession` (`SessionToken`,
    `ConnectURL`, a one-time `ClientSecret`, `ExpiresAt`).
  - `VerifyConnectRedirectSignature(clientSecret, session, status, connectionID, sig) bool`
    verifies the signed redirect back to your return URL entirely offline
    (HMAC-SHA256 over `session|status|connection_id`, keyed by
    `SHA-256(client_secret)`). Standard-library crypto only; no new
    dependencies.
  - `Client.ListConnections(ctx, ListConnectionsOptions{...})`,
    `Client.GetConnection`, `Client.ResyncConnection`,
    `Client.BrowseConnection(ctx, id, path, cursor)`, and
    `Client.UpdateSelection(ctx, id, selectedPaths)` manage a connection and its
    sync scope. `Client.DeleteConnection` purges the synced content and returns
    a `*DisconnectResult`; the signed purge receipt is fetchable with
    `Client.GetPurgeReceipt(ctx, receiptID)` (`*ConnectionPurgeReceipt`).
  - New types: `ConnectSession`, `CreateConnectSessionOptions`, `Connection`,
    `ListConnectionsOptions`, `ConnectionBrowseEntry`, `ConnectionBrowsePage`,
    `DisconnectResult`, `PurgeSummary`, `ConnectionPurgeReceipt`.
- **Typed connect-session errors.** The sentinels `ErrSessionInvalid` (HTTP 400,
  `session_invalid` — the session token is unknown, already used, or expired;
  mint a new session instead of retrying) and `ErrPartitionMismatch` (HTTP 400,
  `partition_mismatch` — the handle's partition disagrees with where the session
  would resolve) match with `errors.Is`; the codes are exported as
  `CodeSessionInvalid` / `CodePartitionMismatch`.

### Changed

- **User-Agent reports the real SDK version.** The `Version` constant sent in
  the `User-Agent` header had been stuck at `0.3.3`; it now tracks the release.

## [0.4.0]

### Added

- **Move a document between partitions.** `Client.MoveDocument(ctx, docID string, from, to *string)`
  performs a metadata-only partition move via `POST /v1/documents/{id}/move`,
  returning the updated `DocumentRecord`. `from` asserts the document's current
  partition so a stale or wrong assertion fails loudly instead of moving the
  wrong record; pass `nil` for the unpartitioned space. Unlike id-addressed
  reads and writes, a move is never auto-scoped by a partition-scoped client —
  it names both endpoints explicitly.
- **Analytical query facade.** `Client.Query(ctx, QueryRequest)` runs a
  structured query and returns a `QueryResponse` that carries either row results
  or an aggregate; use `QueryResponse.IsAggregate()` to tell them apart.
- **Typed field schema.** Declare and manage the typed fields backing structured
  queries with `Client.DeclareFields`, `Client.ListFields`, and
  `Client.DeleteField`, each returning the tenant's current `[]FieldSchema`
  (including live coverage per field).
- **Partition echoed on responses.** `DocumentRecord`, search results, and
  insert responses now expose an optional `Partition` field so you can see which
  partition a record lives in without a second lookup.

### Changed

- **Partition-required errors are now typed.** A `400` from a partition-scoped
  key that omitted the required partition is surfaced as the sentinel
  `ErrPartitionRequired`, so it can be matched with `errors.Is` instead of
  string-matching the message.
- **Scoped-handle partition guard now covers id-addressed operations.** A
  partition-scoped client consistently applies its partition to operations
  addressed by document id, closing a gap where some id-addressed calls
  previously escaped the scope.
