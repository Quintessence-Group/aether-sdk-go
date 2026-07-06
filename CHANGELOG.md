# Changelog

All notable changes to the Aether Go SDK are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
