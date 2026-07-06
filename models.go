package aether

// Metadata is structured document metadata. Values must be strings, numbers,
// or booleans; timestamp metadata should be supplied as RFC 3339 strings.
type Metadata map[string]any

// MetadataFilter is a structured metadata filter. Keys may be "metadata.<key>"
// or bare metadata keys; values are equality shorthand or operator objects
// using eq/ne/gt/lt/gte/lte/in.
type MetadataFilter map[string]any

// DocumentRecord represents an Aether document's metadata.
type DocumentRecord struct {
	// DocID is the unique document identifier assigned by the server.
	DocID string `json:"doc_id"`
	// CID is the content-addressed identifier (hash) for the document.
	CID string `json:"cid"`
	// Title is the optional human-readable document title.
	Title *string `json:"title,omitempty"`
	// ContentType is the MIME type of the document (e.g. "application/pdf").
	ContentType string `json:"content_type"`
	// SizeBytes is the raw size of the document in bytes.
	SizeBytes int64 `json:"size_bytes"`
	// Chunks is the number of text chunks the document was split into.
	Chunks int `json:"chunks"`
	// Vectors is the number of embedding vectors generated for the document.
	Vectors int `json:"vectors"`
	// Version is the document revision number, incremented on each update.
	Version int `json:"version"`
	// EntityID is the optional identifier of the entity (e.g. a user or
	// customer) the document belongs to.
	EntityID *string `json:"entity_id,omitempty"`
	// Tags is the list of metadata tags attached to the document.
	Tags []string `json:"tags,omitempty"`
	// Source is the optional origin label for the document (e.g. "slack",
	// "upload", "crawler"). Nil when the document has no source.
	Source *string `json:"source,omitempty"`
	// Partition is the partition the document lives in. Nil when the document
	// lives in the default partition — mirrors the EntityID/Source convention.
	Partition *string `json:"partition,omitempty"`
	// Metadata is the structured metadata attached to the document.
	Metadata Metadata `json:"metadata,omitempty"`
	// CreatedAt is the RFC 3339 timestamp when the document was first inserted.
	CreatedAt *string `json:"created_at,omitempty"`
	// UpdatedAt is the RFC 3339 timestamp when the document was last modified.
	UpdatedAt *string `json:"updated_at,omitempty"`
}

// AuditProof is the cryptographic provenance attached to an AuditRecord. It
// carries the signed lineage of a ledger event so a caller can independently
// verify that the recorded action was committed by the claimed node.
type AuditProof struct {
	// ContentID is the content-addressed identifier (e.g. "blake3:...") of the
	// document state at the time of the event. Nil when the event has no
	// associated content (for example a tombstone), in which case the key is
	// omitted from the response entirely.
	ContentID *string `json:"content_id,omitempty"`
	// Lamport is the logical (Lamport) clock value of the event, giving a total
	// order over a node's committed operations.
	Lamport uint64 `json:"lamport"`
	// NodeID is the identifier (64-hex) of the node that committed the event.
	NodeID string `json:"node_id"`
	// PublicKey is the hex-encoded public key the signature was produced with.
	PublicKey string `json:"public_key"`
	// Signature is the hex-encoded cryptographic signature over the event.
	Signature string `json:"signature"`
	// Verified reports whether the server successfully verified Signature
	// against PublicKey for this event.
	Verified bool `json:"verified"`
}

// AuditRecord is a single entry in a document's provenance/lineage trail, as
// returned by Client.Lineage. Each record describes one committed action on a
// resource together with its signed AuditProof.
type AuditRecord struct {
	// At is the RFC 3339 timestamp when the action was recorded.
	At string `json:"at"`
	// Actor is the identity that performed the action (e.g. "node:<hex>").
	Actor string `json:"actor"`
	// Action is the action that was performed (e.g. "document.inserted").
	Action string `json:"action"`
	// Resource is the resource the action targeted (e.g. "document:<uuid>").
	Resource string `json:"resource"`
	// Outcome is the result of the action (e.g. "committed").
	Outcome string `json:"outcome"`
	// Source is the origin of the record (e.g. "ledger").
	Source string `json:"source"`
	// Proof is the cryptographic provenance for this record.
	Proof AuditProof `json:"proof"`
}

// IngestResult is the outcome of ingesting a single file via IngestFiles or
// IngestDirectory. There is exactly one result per input path, returned in
// the same order as the inputs.
//
// Status is one of:
//
//   - "ingested" — stored and indexed; DocID is set.
//   - "skipped"  — the engine could not ingest this file (an unsupported or
//     binary type, one that needs the server-side document parser when it is
//     not configured, or a file over the size limit). Error explains why. This
//     is the graceful path: the batch continues and the function's error stays
//     nil.
//   - "error"    — an unexpected failure (e.g. the file could not be read, or a
//     transient API/network error). Error carries the detail.
type IngestResult struct {
	// Path is the input file path this result corresponds to.
	Path string `json:"path"`
	// Status is one of "ingested", "skipped", or "error".
	Status string `json:"status"`
	// DocID is the server-assigned document id. Empty unless Status is
	// "ingested".
	DocID string `json:"doc_id,omitempty"`
	// ContentType is the MIME type the file was ingested as, resolved from its
	// extension.
	ContentType string `json:"content_type,omitempty"`
	// Error explains a "skipped" or "error" outcome; empty for "ingested".
	Error string `json:"error,omitempty"`
}

// SearchResult represents a single similarity search hit.
type SearchResult struct {
	// DocID is the identifier of the matched document.
	DocID string `json:"doc_id"`
	// Score is the calibrated relevance of the match, an integer 0–100 where
	// higher is better (~100 for a near-exact match). Results are ordered by
	// descending Score.
	Score int `json:"score"`
	// Title is the optional document title.
	Title *string `json:"title,omitempty"`
	// EntityID is the optional owning entity the matched document is scoped to.
	// Nil when the document is unscoped.
	EntityID *string `json:"entity_id,omitempty"`
	// ContentType is the MIME type of the matched document.
	ContentType string `json:"content_type"`
	// Content is the full document content, populated when include_content is
	// requested (see WithIncludeContent). Nil otherwise.
	Content *string `json:"content,omitempty"`
	// Passage is the specific text chunk that matched the query.
	Passage *string `json:"passage,omitempty"`
	// Tags is the list of metadata tags attached to the matched document.
	Tags []string `json:"tags,omitempty"`
	// Source is the optional origin label of the matched document. Nil when the
	// document has no source.
	Source *string `json:"source,omitempty"`
	// Partition is the partition the matched document lives in. Nil when the
	// document lives in the default partition — mirrors the EntityID/Source
	// convention.
	Partition *string `json:"partition,omitempty"`
	// Metadata is the structured metadata attached to the matched document.
	Metadata Metadata `json:"metadata,omitempty"`
	// CreatedAt is the RFC 3339 timestamp when the matched document was first
	// inserted. Nil when not reported. Kept as a raw string to match the rest of
	// the SDK's timestamp handling.
	CreatedAt *string `json:"created_at,omitempty"`
	// UpdatedAt is the RFC 3339 timestamp when the matched document was last
	// modified. Nil when not reported. Kept as a raw string to match the rest of
	// the SDK's timestamp handling.
	UpdatedAt *string `json:"updated_at,omitempty"`
	// QueryID is the feedback handle for the search that returned this hit.
	// Present only when usage-feedback capture is enabled for your tenant (nil
	// otherwise); pass it to Client.SendSearchFeedback together with DocID.
	QueryID *string `json:"query_id,omitempty"`
}

// RetrievalResult extends SearchResult with document content for RAG workflows.
type RetrievalResult struct {
	// DocID is the identifier of the matched document.
	DocID string `json:"doc_id"`
	// Score is the calibrated relevance of the match, an integer 0–100 where
	// higher is better (~100 for a near-exact match).
	Score int `json:"score"`
	// Content is the full document text, always populated for retrieval results.
	Content string `json:"content"`
	// Title is the optional document title.
	Title *string `json:"title,omitempty"`
	// EntityID is the optional owning entity the matched document is scoped to.
	// Nil when the document is unscoped.
	EntityID *string `json:"entity_id,omitempty"`
	// ContentType is the MIME type of the matched document.
	ContentType string `json:"content_type"`
	// Passage is the specific text chunk that matched the query.
	Passage *string `json:"passage,omitempty"`
	// Tags is the list of metadata tags attached to the matched document.
	Tags []string `json:"tags,omitempty"`
	// Source is the optional origin label of the matched document. Nil when the
	// document has no source.
	Source *string `json:"source,omitempty"`
	// Partition is the partition the matched document lives in. Nil when the
	// document lives in the default partition — mirrors the EntityID/Source
	// convention.
	Partition *string `json:"partition,omitempty"`
	// Metadata is the structured metadata attached to the matched document.
	Metadata Metadata `json:"metadata,omitempty"`
	// CreatedAt is the RFC 3339 timestamp when the matched document was first
	// inserted. Nil when not reported.
	CreatedAt *string `json:"created_at,omitempty"`
	// UpdatedAt is the RFC 3339 timestamp when the matched document was last
	// modified. Nil when not reported.
	UpdatedAt *string `json:"updated_at,omitempty"`
}

// InsertWithEmbeddingsOptions configures a bring-your-own-embeddings (BYOE) insert.
type InsertWithEmbeddingsOptions struct {
	// Passages is a list of text passages with their precomputed embedding vectors.
	Passages []EmbedPassage `json:"passages,omitempty"`
	// Embedding is a single precomputed embedding vector for the entire document.
	Embedding []float32 `json:"embedding,omitempty"`
	// Filename is the document filename used for content-type detection.
	Filename string `json:"filename,omitempty"`
	// ContentType overrides the MIME type (e.g. "text/plain"). Guessed from Filename if empty.
	ContentType string `json:"content_type,omitempty"`
	// Tags is a list of metadata tags for filtering in search.
	Tags []string `json:"tags,omitempty"`
	// EntityID associates the document with an entity (e.g. a user or
	// customer id) for later filtering in list and search. Empty means no
	// entity association.
	EntityID string `json:"entity_id,omitempty"`
	// Source labels the document's origin (e.g. "slack", "upload") for later
	// filtering in list and search. Empty means no source.
	Source string `json:"source,omitempty"`
	// Metadata is structured metadata for filtering in list/search.
	Metadata Metadata `json:"metadata,omitempty"`
}

// EmbedPassage is a text passage with its precomputed embedding vector.
type EmbedPassage struct {
	// Text is the raw passage text.
	Text string `json:"text"`
	// Embedding is the precomputed vector for this passage.
	Embedding []float32 `json:"embedding"`
}

type insertWithEmbeddingsRequest struct {
	Content     string         `json:"content"`
	Passages    []EmbedPassage `json:"passages,omitempty"`
	Embedding   []float32      `json:"embedding,omitempty"`
	Filename    string         `json:"filename,omitempty"`
	ContentType string         `json:"content_type,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	EntityID    *string        `json:"entity_id,omitempty"`
	Source      *string        `json:"source,omitempty"`
	Metadata    Metadata       `json:"metadata,omitempty"`
	// Partition scopes this write to a partition when the client is scoped.
	// Empty (unscoped) is omitted from the wire.
	Partition string `json:"partition,omitempty"`
}

type vectorSearchRequest struct {
	Embedding      []float32 `json:"embedding"`
	K              int       `json:"k"`
	IncludeContent bool      `json:"include_content,omitempty"`
	Tags           []string  `json:"tags,omitempty"`
	AnyTags        []string  `json:"any_tags,omitempty"`
	ContentType    []string  `json:"content_type,omitempty"`
	Source         []string  `json:"source,omitempty"`
	EntityID       string    `json:"entity_id,omitempty"`
	Since          string    `json:"since,omitempty"`
	Until          string    `json:"until,omitempty"`
	LastNDays      int       `json:"last_n_days,omitempty"`
	MaxDistance    float32   `json:"max_distance,omitempty"`
	RecencyWeight  float32   `json:"recency_weight,omitempty"`
	HalfLifeDays   float32   `json:"half_life_days,omitempty"`

	FreshnessWeight       float32 `json:"freshness_weight,omitempty"`
	FreshnessHalfLifeDays float32 `json:"freshness_half_life_days,omitempty"`

	Filter MetadataFilter `json:"filter,omitempty"`
	// Partition scopes this search to a partition when the client is scoped.
	// Empty (unscoped) is omitted from the wire.
	Partition string `json:"partition,omitempty"`
}

// ArchivePrice is the live cost-per-GiB for permanent archive uploads via
// Arweave/Irys, returned by Client.ArchivePrice. Mirrors the gateway's
// 5-minute cached upstream price — values older than CacheTTLSeconds
// trigger an upstream refresh on the next request.
type ArchivePrice struct {
	// Provider names the upstream archive network ("arweave", "irys").
	Provider string `json:"provider"`
	// UnitPriceCentsPerGiB is the upload cost per GiB in US-cents at the
	// moment FetchedAt was set. Used both for Portal display and for the
	// at-time price stamped onto archive_events when bytes are uploaded.
	UnitPriceCentsPerGiB int64 `json:"unit_price_cents_per_gib"`
	// FetchedAt is the RFC-3339 timestamp at which the gateway refreshed
	// this value from upstream.
	FetchedAt string `json:"fetched_at"`
	// CacheTTLSeconds is the lifetime of the gateway's in-memory cache
	// for this row. Pin to the same value if the caller wants its own
	// secondary cache.
	CacheTTLSeconds uint64 `json:"cache_ttl_seconds"`
}

// EntityBackfillReport summarizes a BackfillEntityFromTags run, counting how
// the tenant's active documents were classified against the tag convention.
type EntityBackfillReport struct {
	// Scanned is the number of active documents examined.
	Scanned int `json:"scanned"`
	// Updated is the number of documents whose entity_id was set from a tag.
	Updated int `json:"updated"`
	// SkippedExisting is the number of documents left unchanged because they
	// already had an entity_id (and overwrite was false).
	SkippedExisting int `json:"skipped_existing"`
	// SkippedNoMatch is the number of documents with no tag matching the prefix.
	SkippedNoMatch int `json:"skipped_no_match"`
	// SkippedAmbiguous is the number of documents with 2+ tags matching the
	// prefix, which cannot be disambiguated.
	SkippedAmbiguous int `json:"skipped_ambiguous"`
	// SkippedInvalid is the number of documents whose matched tag suffix was
	// not a valid entity_id.
	SkippedInvalid int `json:"skipped_invalid"`
}

// NodeStatus represents the health and resource usage of a single Aether node.
type NodeStatus struct {
	// NodeID is the unique numeric identifier for this node in the cluster.
	NodeID int `json:"node_id"`
	// Documents is the number of active documents stored on this node.
	Documents int `json:"documents"`
	// Vectors is the total number of embedding vectors indexed on this node.
	Vectors int `json:"vectors"`
	// Version is the Aether server version running on this node.
	Version string `json:"version,omitempty"`
}

// AsyncJobResult holds the response from an asynchronous document insertion.
type AsyncJobResult struct {
	// JobID is the unique identifier for the background processing job.
	JobID string `json:"job_id"`
	// Status is the initial job state (typically "pending").
	Status string `json:"status"`
	// PollURL is the relative URL to poll for job completion.
	PollURL string `json:"poll_url"`
}

// JobStatus holds the current state of a background processing job.
type JobStatus struct {
	// Status is the job state: "pending", "processing", "completed", or "failed".
	Status string `json:"status"`
	// DocID is the resulting document identifier, set when the job completes successfully.
	DocID *string `json:"doc_id,omitempty"`
	// Error is the failure reason, set when the job status is "failed".
	Error *string `json:"error,omitempty"`
}

// BatchInsertItem represents a single document in a batch insert request.
type BatchInsertItem struct {
	// Filename is the document filename, used for content-type detection.
	Filename string `json:"filename"`
	// Content is the raw document text to be indexed.
	Content string `json:"content"`
	// Tags is an optional list of metadata tags for filtering in search.
	Tags []string `json:"tags,omitempty"`
	// EntityID associates the document with an entity (e.g. a user or
	// customer id) for later filtering in list and search. Empty means no
	// entity association.
	EntityID string `json:"entity_id,omitempty"`
	// Source labels the document's origin (e.g. "slack", "upload") for later
	// filtering in list and search. Empty means no source.
	Source string `json:"source,omitempty"`
	// Metadata is structured metadata for filtering in list/search.
	Metadata Metadata `json:"metadata,omitempty"`
}

// BatchSearchQuery represents a single query in a batch search request.
type BatchSearchQuery struct {
	// Q is the natural-language search query.
	Q string `json:"q"`
	// K is the maximum number of results to return for this query.
	K int `json:"k,omitempty"`
	// Tags filters results to documents matching ALL of these metadata tags (AND).
	Tags []string `json:"tags,omitempty"`
	// AnyTags filters results to documents matching AT LEAST ONE of these
	// metadata tags (OR).
	AnyTags []string `json:"any_tags,omitempty"`
	// ContentTypes filters results to documents whose content type is any one of
	// these values (OR).
	ContentTypes []string `json:"content_type,omitempty"`
	// Sources filters results to documents whose source is any one of these
	// values (OR).
	Sources []string `json:"source,omitempty"`
	// Filter is a structured metadata filter with equality or operator predicates.
	Filter MetadataFilter `json:"filter,omitempty"`
	// IncludeContent requests full document content inline in results.
	IncludeContent bool `json:"include_content,omitempty"`
	// EntityID filters results to documents associated with the given
	// entity id. Empty means no entity filter.
	EntityID string `json:"entity_id,omitempty"`
	// Since filters results to documents created at or after this RFC 3339
	// timestamp (inclusive). Empty means no lower bound.
	Since string `json:"since,omitempty"`
	// Until filters results to documents created at or before this RFC 3339
	// timestamp (inclusive). Empty means no upper bound.
	Until string `json:"until,omitempty"`
	// LastNDays filters results to documents created within the last N days
	// (server clock, UTC). It cannot be combined with Since but may be
	// combined with Until. Zero means no recency filter.
	LastNDays int `json:"last_n_days,omitempty"`
	// MaxDistance is an optional relevance-distance ceiling; results whose
	// distance exceeds it are dropped server-side. Zero means no cutoff.
	MaxDistance float32 `json:"max_distance,omitempty"`
	// RecencyWeight blends server-side recency into this query's ranking
	// (recency_weight, 0–1). Zero means pure relevance.
	RecencyWeight float32 `json:"recency_weight,omitempty"`
	// HalfLifeDays is the recency decay half-life in days for this query
	// (half_life_days). Zero leaves the server default (30 days).
	HalfLifeDays float32 `json:"half_life_days,omitempty"`
	// FreshnessWeight blends server-side freshness into this query's ranking
	// (freshness_weight, 0–1), boosting recently updated documents (updated_at,
	// falling back to created_at). Zero means pure relevance. Composes with
	// RecencyWeight; the server rejects a combined weight above 1. May require
	// a Scale plan or higher.
	FreshnessWeight float32 `json:"freshness_weight,omitempty"`
	// FreshnessHalfLifeDays is the freshness decay half-life in days for this
	// query (freshness_half_life_days). Zero leaves the server default (14 days).
	FreshnessHalfLifeDays float32 `json:"freshness_half_life_days,omitempty"`
}

// BatchSearchResponse contains the results for a single query within a batch search.
type BatchSearchResponse struct {
	// Query is the original search query text.
	Query string `json:"query"`
	// Results is the list of similarity search hits for this query.
	Results []SearchResult `json:"results"`
}

// internal response wrappers

type searchResponse struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
	// QueryID is the response-level usage-feedback handle; the SDK stamps it
	// onto every hit. Nil unless feedback capture is enabled for the tenant.
	QueryID *string `json:"query_id"`
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// batchInsertItemWire is the on-the-wire shape of a batch-insert document. It
// mirrors the public BatchInsertItem and adds the handle's partition (omitted
// when unscoped). Keeping partition off the public type means there is no
// per-call partition argument — scoping flows only from the client handle.
//
// Tags is sent as a comma-separated string, which is how the server
// deserializes the per-item tags field.
type batchInsertItemWire struct {
	Filename  string   `json:"filename"`
	Content   string   `json:"content"`
	Tags      string   `json:"tags,omitempty"`
	EntityID  string   `json:"entity_id,omitempty"`
	Source    string   `json:"source,omitempty"`
	Metadata  Metadata `json:"metadata,omitempty"`
	Partition string   `json:"partition,omitempty"`
}

type batchInsertRequest struct {
	Documents []batchInsertItemWire `json:"documents"`
	ChunkSize *int                  `json:"chunk_size,omitempty"`
	Overlap   *int                  `json:"overlap,omitempty"`
}

type batchInsertResponse struct {
	Results []DocumentRecord `json:"results"`
}

// batchSearchQueryWire is the on-the-wire shape of a batch-search query. It
// mirrors the public BatchSearchQuery and adds the handle's partition (omitted
// when unscoped), keeping partition off the public type.
// All metadata-facet fields (tags, any_tags, content_type, source) are sent as
// comma-separated strings, which is how the server deserializes them per query.
type batchSearchQueryWire struct {
	Q              string         `json:"q"`
	K              int            `json:"k,omitempty"`
	Tags           string         `json:"tags,omitempty"`
	AnyTags        string         `json:"any_tags,omitempty"`
	ContentType    string         `json:"content_type,omitempty"`
	Source         string         `json:"source,omitempty"`
	Filter         MetadataFilter `json:"filter,omitempty"`
	IncludeContent bool           `json:"include_content,omitempty"`
	EntityID       string         `json:"entity_id,omitempty"`
	Since          string         `json:"since,omitempty"`
	Until          string         `json:"until,omitempty"`
	LastNDays      int            `json:"last_n_days,omitempty"`
	MaxDistance    float32        `json:"max_distance,omitempty"`
	RecencyWeight  float32        `json:"recency_weight,omitempty"`
	HalfLifeDays   float32        `json:"half_life_days,omitempty"`

	FreshnessWeight       float32 `json:"freshness_weight,omitempty"`
	FreshnessHalfLifeDays float32 `json:"freshness_half_life_days,omitempty"`

	Partition string `json:"partition,omitempty"`
}

type batchSearchRequest struct {
	Queries []batchSearchQueryWire `json:"queries"`
}

// batchSearchResponseItemWire is the on-the-wire shape of one query's results
// in a batch search: the public BatchSearchResponse envelope plus the optional
// per-query usage-feedback query_id, which the SDK stamps onto each hit.
type batchSearchResponseItemWire struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
	QueryID *string        `json:"query_id"`
}

type batchSearchResponseWrapper struct {
	Results []batchSearchResponseItemWire `json:"results"`
}

// searchFeedbackRequest is the wire body of POST /search/feedback.
type searchFeedbackRequest struct {
	QueryID string `json:"query_id"`
	DocID   string `json:"doc_id"`
	Signal  string `json:"signal"`
}

type backfillEntityRequest struct {
	TagPrefix string `json:"tag_prefix"`
	Overwrite bool   `json:"overwrite"`
}

// moveDocumentRequest is the wire body of POST /documents/{id}/move. Both
// fields are always present on the wire — an explicit null names the default
// partition and an omitted field is a 400 — so neither carries omitempty.
type moveDocumentRequest struct {
	ToPartition     *string `json:"to_partition"`
	ExpectPartition *string `json:"expect_partition"`
}

// ── Partition lifecycle ───────────────────────────────────────────

// PartitionInfo names a partition and its active (non-tombstoned) document count.
type PartitionInfo struct {
	// ID is the partition identifier.
	ID string `json:"id"`
	// DocumentCount is the number of active documents in the partition.
	DocumentCount int `json:"document_count"`
}

// PartitionWarning is an advisory flag about a likely-mistyped or ghost
// partition. It is informational only and never blocks any operation.
//
// Kind is "single_document" (a partition holding one document — often a typo
// or abandoned ghost) or "near_duplicate" (two ids that differ only
// cosmetically — likely the same end-client under two keys). Partitions lists
// the ids the warning applies to; Detail is a human-readable explanation.
type PartitionWarning struct {
	// Kind is the warning category ("single_document" or "near_duplicate").
	Kind string `json:"kind"`
	// Partitions are the partition ids the warning applies to.
	Partitions []string `json:"partitions"`
	// Detail is a human-readable explanation of the warning.
	Detail string `json:"detail"`
}

// PartitionList is the result of Client.ListPartitions: the tenant's
// partitions plus any advisory warnings. The default (unkeyed) partition is
// not enumerated.
type PartitionList struct {
	// Partitions are the tenant's partitions with their document counts.
	Partitions []PartitionInfo `json:"partitions"`
	// Warnings are advisory flags about likely-mistyped or ghost partitions.
	Warnings []PartitionWarning `json:"warnings"`
}

// ── Provable isolation ────────────────────────────────────────────

// SearchTrace is evidence of which partition(s) a search actually touched,
// computed from the records the query returned.
//
// For a scoped query, PartitionsTouched is always empty or exactly
// [ScopedTo], and CandidatesInScope is the partition's own size (proof the
// scope bounded the search as a hard ceiling, not a post-filter). Boundary is
// "partition" (scoped) or "tenant" (unscoped).
type SearchTrace struct {
	// ScopedTo is the partition the search was scoped to, or nil if unscoped.
	ScopedTo *string `json:"scoped_to,omitempty"`
	// PartitionsTouched are the partitions of the records actually returned.
	PartitionsTouched []string `json:"partitions_touched"`
	// DefaultPartitionTouched is true when the default partition was reached.
	DefaultPartitionTouched bool `json:"default_partition_touched"`
	// Results is the number of records returned.
	Results int `json:"results"`
	// CandidatesInScope is the number of candidate records the scope bounded
	// the search to, or nil when not reported.
	CandidatesInScope *int `json:"candidates_in_scope,omitempty"`
	// Boundary is "partition" (scoped) or "tenant" (unscoped).
	Boundary string `json:"boundary"`
}

// TracedSearch holds search results together with the SearchTrace that
// produced them.
type TracedSearch struct {
	// Results is the list of search hits.
	Results []SearchResult
	// Trace is the isolation evidence for the search.
	Trace SearchTrace
}

// IsolationCheck is the outcome of Client.VerifyIsolation on a partition
// handle. OK is true iff no returned record left the handle's partition; only
// meaningful for a query that returns results — a 0-result query passes
// vacuously (Results == 0).
type IsolationCheck struct {
	// OK is true iff no returned record left the handle's partition.
	OK bool
	// ScopedTo is the partition the check was run under.
	ScopedTo *string
	// PartitionsTouched are the partitions of the records actually returned.
	PartitionsTouched []string
	// Results is the number of records returned.
	Results int
	// CandidatesInScope is the number of candidate records the scope bounded
	// the search to, or nil when not reported.
	CandidatesInScope *int
	// Leaked lists any partitions touched other than the handle's own.
	Leaked []string
}

// tracedSearchResponse is the on-the-wire shape of a traced search: the normal
// search envelope plus a trace object.
type tracedSearchResponse struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
	QueryID *string        `json:"query_id"`
	Trace   SearchTrace    `json:"trace"`
}

// partitionDeleteResponse is the on-the-wire shape of a partition delete.
type partitionDeleteResponse struct {
	DocumentsDeleted int `json:"documents_deleted"`
}
