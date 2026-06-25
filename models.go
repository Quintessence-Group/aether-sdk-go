package aether

// DocumentRecord represents an Aether document's metadata.
type DocumentRecord struct {
	// DocID is the unique document identifier assigned by the server.
	DocID string `json:"doc_id"`
	// CID is the content-addressed identifier (hash) for the document.
	CID string `json:"cid"`
	// Title is the optional human-readable document title.
	Title *string `json:"title,omitempty"`
	// EntityID is the optional owning entity (user, agent, tenant, …) the
	// document is scoped to. Set at insert/update time via WithEntityID and
	// used to partition memories per-entity. Nil when the document is unscoped.
	EntityID *string `json:"entity_id,omitempty"`
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
	// CreatedAt is the RFC 3339 timestamp when the document was first inserted.
	CreatedAt *string `json:"created_at,omitempty"`
	// UpdatedAt is the RFC 3339 timestamp when the document was last modified.
	UpdatedAt *string `json:"updated_at,omitempty"`
}

// SearchResult represents a single similarity search hit.
type SearchResult struct {
	// DocID is the identifier of the matched document.
	DocID string `json:"doc_id"`
	// Score is the calibrated relevance of the match, an integer 0–100 where
	// higher is better (~100 for a near-exact match).
	Score int `json:"score"`
	// Title is the optional document title.
	Title *string `json:"title,omitempty"`
	// EntityID is the optional owning entity the matched document is scoped to.
	// Nil when the document is unscoped.
	EntityID *string `json:"entity_id,omitempty"`
	// ContentType is the MIME type of the matched document.
	ContentType string `json:"content_type"`
	// Passage is the specific text chunk that matched the query. Fetch the full
	// document text with Get/DownloadText rather than inline.
	Passage *string `json:"passage,omitempty"`
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
	// EntityID scopes the document to an owning entity (user, agent, tenant, …).
	// Leave empty to insert unscoped.
	EntityID string `json:"entity_id,omitempty"`
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
	EntityID    string         `json:"entity_id,omitempty"`
}

type vectorSearchRequest struct {
	Embedding   []float32 `json:"embedding"`
	K           int       `json:"k"`
	Tags        []string  `json:"tags,omitempty"`
	MaxDistance *float32  `json:"max_distance,omitempty"`
	EntityID    string    `json:"entity_id,omitempty"`
	Since       string    `json:"since,omitempty"`
	Until       string    `json:"until,omitempty"`
	LastNDays   int       `json:"last_n_days,omitempty"`
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
}

// BatchSearchQuery represents a single query in a batch search request.
type BatchSearchQuery struct {
	// Q is the natural-language search query.
	Q string `json:"q"`
	// K is the maximum number of results to return for this query.
	K int `json:"k,omitempty"`
	// Tags filters results to documents matching these metadata tags.
	Tags []string `json:"tags,omitempty"`
	// MaxDistance is an optional relevance-distance ceiling. Results with
	// distance > max are dropped server-side. Leave nil to
	// return the top-k regardless of distance.
	MaxDistance *float32 `json:"max_distance,omitempty"`
	// EntityID restricts this query to documents scoped to the given entity.
	// Leave empty to search across all entities.
	EntityID string `json:"entity_id,omitempty"`
	// Since restricts this query to documents created on or after the given
	// RFC 3339 timestamp (inclusive). Leave empty for no lower bound.
	Since string `json:"since,omitempty"`
	// Until restricts this query to documents created on or before the given
	// RFC 3339 timestamp (inclusive). Leave empty for no upper bound.
	Until string `json:"until,omitempty"`
	// LastNDays restricts this query to documents created within the last n
	// days (server-side shorthand for since; cannot be combined with Since).
	// Zero is ignored.
	LastNDays int `json:"last_n_days,omitempty"`
}

// EntityBackfillReport summarizes a tag→entity_id backfill run, returned by
// Client.BackfillEntityFromTags. Every active document the server considered
// lands in exactly one bucket, so Scanned == Updated + SkippedExisting +
// SkippedNoMatch + SkippedAmbiguous + SkippedInvalid.
type EntityBackfillReport struct {
	// Scanned is the total number of active documents examined.
	Scanned int `json:"scanned"`
	// Updated is the number of documents whose entity_id was set from a tag this run.
	Updated int `json:"updated"`
	// SkippedExisting counts documents that already had an entity_id (and
	// overwrite was false), or whose derived value already matched.
	SkippedExisting int `json:"skipped_existing"`
	// SkippedNoMatch counts documents with no tag matching the prefix (or an
	// empty suffix).
	SkippedNoMatch int `json:"skipped_no_match"`
	// SkippedAmbiguous counts documents with two or more tags matching the
	// prefix (never guessed).
	SkippedAmbiguous int `json:"skipped_ambiguous"`
	// SkippedInvalid counts documents whose derived entity_id exceeded the
	// length bound.
	SkippedInvalid int `json:"skipped_invalid"`
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
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

// batchInsertItemWire is the wire representation of a BatchInsertItem. The
// batch deserializer expects tags as a single comma-joined string (mirroring
// the comma-joined "tags" query param used by single-insert), not a JSON array.
type batchInsertItemWire struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
	Tags     string `json:"tags,omitempty"`
}

// batchSearchQueryWire is the wire representation of a BatchSearchQuery, with
// tags joined into a comma-separated string for the batch deserializer.
type batchSearchQueryWire struct {
	Q           string   `json:"q"`
	K           int      `json:"k,omitempty"`
	Tags        string   `json:"tags,omitempty"`
	MaxDistance *float32 `json:"max_distance,omitempty"`
	EntityID    string   `json:"entity_id,omitempty"`
	Since       string   `json:"since,omitempty"`
	Until       string   `json:"until,omitempty"`
	LastNDays   int      `json:"last_n_days,omitempty"`
}

type batchInsertRequest struct {
	Documents []batchInsertItemWire `json:"documents"`
	ChunkSize *int                  `json:"chunk_size,omitempty"`
	Overlap   *int                  `json:"overlap,omitempty"`
}

type batchInsertResponse struct {
	Results []DocumentRecord `json:"results"`
}

type batchSearchRequest struct {
	Queries []batchSearchQueryWire `json:"queries"`
}

type batchSearchResponseWrapper struct {
	Results []BatchSearchResponse `json:"results"`
}

type entityBackfillRequest struct {
	TagPrefix string `json:"tag_prefix"`
	Overwrite bool   `json:"overwrite"`
}
