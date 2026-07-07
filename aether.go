// Package aether provides a Go client for Aether agent memory.
package aether

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Version is the SDK version, sent in the User-Agent header.
const Version = "0.4.0"

// userAgent identifies the SDK + version + Go runtime so the server can
// attribute traffic, track version adoption, and target deprecations.
var userAgent = "aether-sdk-go/" + Version + " (" + runtime.Version() + ")"

// newIdempotencyKey returns a fresh RFC 4122 v4 UUID for one logical write.
// The same key is reused across retries of a single call so the server can
// deduplicate a request whose response was lost in transit. On the (extremely
// unlikely) failure to read randomness it returns "", which simply omits the
// header rather than failing the request.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// validateBaseURL rejects configurations that would send an API key over
// cleartext HTTP to a non-loopback host. Loopback addresses (localhost,
// 127.0.0.0/8, ::1) are allowed so local development against a non-TLS node
// still works. Returns nil when no API key is set or the URL is HTTPS.
func validateBaseURL(baseURL, apiKey string) error {
	if apiKey == "" {
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" {
		return nil // malformed URLs surface later at request time
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf(
		"aether: refusing to send API key over insecure HTTP to %q; "+
			"use an https:// base URL, or omit the API key for local non-TLS endpoints", host)
}

// apiVersionPrefix is the canonical public API version prefix. Every data
// route (documents, search, memory, partitions, archive) is served under it.
// The public probe route GET /status is intentionally unversioned.
const apiVersionPrefix = "/v1"

// versionedPath prefixes a relative request path with the public API version.
// The prefix always goes before the path itself, never into the query string.
// Unversioned probe routes (/status) pass through untouched.
func versionedPath(path string) string {
	bare, _, _ := strings.Cut(path, "?")
	if bare == "/status" {
		return path
	}
	return apiVersionPrefix + path
}

// ClientOptions holds optional configuration for the Aether client.
type ClientOptions struct {
	// MaxRetries is the maximum number of retries for transient errors.
	// Default: 2 (3 total attempts). Set to 0 to disable retries.
	MaxRetries int
	// RetryBaseDelay is the base delay for exponential backoff.
	// Actual delay: base * 2^attempt + jitter (0-50% of delay).
	// Default: 500ms.
	RetryBaseDelay time.Duration
}

// Client is a client for the Aether dRAG HTTP API.
type Client struct {
	baseURL      string
	apiKey       string
	httpClient   *http.Client
	maxRetries   int
	retryBackoff time.Duration
	// partition, when non-empty, scopes every partition-aware read and write to
	// a single partition. It is set only via Partition and is otherwise empty
	// (unscoped). See Partition for the full semantics.
	partition string
	// cfgErr is set when the resolved configuration is unsafe (e.g. an API
	// key paired with a cleartext-HTTP non-loopback base URL). It is returned
	// from every request so misconfiguration fails fast and consistently.
	cfgErr error
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key for authentication (sent as Bearer token).
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// WithHTTPClient sets a custom http.Client for the Aether client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithTimeout sets the request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithBaseURL overrides the base URL (useful with New()).
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(url, "/") }
}

// WithMaxRetries sets the number of retries for transient errors (429, 502, 503, 504, network).
// Default: 2 (3 total attempts). Set to 0 to disable retries.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithRetryBackoff sets the base backoff duration. Actual delay: base * 2^attempt + jitter (0-50%).
// Default: 500ms.
func WithRetryBackoff(d time.Duration) Option {
	return func(c *Client) { c.retryBackoff = d }
}

// WithClientOptions applies MaxRetries and RetryBaseDelay from a ClientOptions struct.
func WithClientOptions(opts ClientOptions) Option {
	return func(c *Client) {
		if opts.MaxRetries > 0 {
			c.maxRetries = opts.MaxRetries
		}
		if opts.RetryBaseDelay > 0 {
			c.retryBackoff = opts.RetryBaseDelay
		}
	}
}

// New creates a new Aether API client with configuration resolved from
// environment variables. Resolution priority:
//
//	base_url: WithBaseURL option > AETHER_BASE_URL env var > "https://api.aetherdb.ai"
//	api_key:  WithAPIKey option > AETHER_API_KEY env var > ""
func New(opts ...Option) *Client {
	baseURL := os.Getenv("AETHER_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.aetherdb.ai"
	}
	c := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		apiKey:       os.Getenv("AETHER_API_KEY"),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		maxRetries:   2,
		retryBackoff: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.cfgErr = validateBaseURL(c.baseURL, c.apiKey)
	return c
}

// NewClient creates a new Aether API client with an explicit base URL.
// Prefer New() for production usage which reads configuration from environment variables.
func NewClient(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		httpClient:   &http.Client{Timeout: 30 * time.Second},
		maxRetries:   2,
		retryBackoff: 500 * time.Millisecond,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.cfgErr = validateBaseURL(c.baseURL, c.apiKey)
	return c
}

// ── Partition scoping ─────────────────────────────────────────────

// maxPartitionIDLen is the maximum length of a partition id.
const maxPartitionIDLen = 256

// validatePartition checks a partition id client-side, before any HTTP call.
// It mirrors validateEntityID: the id must be non-empty after trimming and at
// most maxPartitionIDLen characters.
func validatePartition(partitionID string) error {
	if strings.TrimSpace(partitionID) == "" {
		return fmt.Errorf("aether: partition cannot be empty")
	}
	if len(partitionID) > maxPartitionIDLen {
		return fmt.Errorf("aether: partition must be 1-%d characters, got %d", maxPartitionIDLen, len(partitionID))
	}
	return nil
}

// Partition returns a scoped clone of this client whose every partition-aware
// read and write is automatically scoped to the given partition. A multi-tenant
// key requires a partition on every call; the scoped handle is the ergonomic way
// to never forget it. The default (unscoped) client keeps operating on the
// default partition, so single-tenant usage stays frictionless.
//
// Doc_id-addressed methods (Get, Download, DownloadText, Delete, HardDelete,
// Restore, Update) are partition-checked when scoped: the handle sends the
// partition as a guard, and a document in a different partition is a 404
// identical to a nonexistent id — a scoped client can no longer reach another
// partition's document via a bare doc id.
//
// The returned client shares the underlying transport and all configuration
// (base url, api key, timeout, retries, backoff) with the receiver — it does not
// own the transport, so the base client remains responsible for it. Scoping is
// bound to the returned object: the only way to reach a different partition is to
// obtain a distinct handle, and there is no per-call partition argument on any
// data method. Re-scoping is last-wins, so
// client.Partition("a").Partition("b") is scoped to "b".
//
// The partition id is validated client-side (non-empty, 1-256 chars); an invalid
// id returns an error without a network round-trip.
func (c *Client) Partition(partitionID string) (*Client, error) {
	if err := validatePartition(partitionID); err != nil {
		return nil, err
	}
	cp := *c
	cp.partition = partitionID
	return &cp, nil
}

// ── Internal helpers ──────────────────────────────────────────────

// newRequest builds a request against the client's base URL. Data routes are
// rewritten under the /v1 API version prefix here, at the transport boundary,
// so every caller (including the Memory facade) versions its paths in one
// place. The same choke point stamps the SDK User-Agent and, for logical
// writes, the caller-minted Idempotency-Key (empty for reads).
func (c *Client) newRequest(ctx context.Context, method, path, idempotencyKey, contentType string, body io.Reader) (*http.Request, error) {
	reqURL := c.baseURL + versionedPath(path)
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return req, nil
}

// isRetryableStatus returns true for HTTP status codes that are transient.
func isRetryableStatus(code int) bool {
	return code == 429 || code == 502 || code == 503 || code == 504
}

// doJSON sends a request whose body (when present) is a JSON payload, labelled
// `Content-Type: application/json` — the node's typed-body routes reject an
// unlabelled body with 415. Raw content uploads go through doRaw instead: the
// stored document's content_type comes from the `content_type` query param,
// never the transport header, so those requests deliberately leave it unset.
func (c *Client) doJSON(ctx context.Context, method, path string, body io.Reader, result any) error {
	return c.doBody(ctx, method, path, "application/json", body, result)
}

// doRawBody sends a request whose body is raw document content (no Content-Type).
func (c *Client) doRawBody(ctx context.Context, method, path string, body io.Reader, result any) error {
	return c.doBody(ctx, method, path, "", body, result)
}

func (c *Client) doBody(ctx context.Context, method, path, contentType string, body io.Reader, result any) error {
	if c.cfgErr != nil {
		return c.cfgErr
	}
	// Buffer the body so it can be replayed on retry.
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return &NetworkError{Err: err}
		}
	}

	// Mint one idempotency key per logical write, reused across retries so the
	// server can deduplicate a request whose response was lost in transit.
	idempotencyKey := ""
	if method == http.MethodPost {
		idempotencyKey = newIdempotencyKey()
	}

	resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		var bodyReader io.Reader
		ct := ""
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
			ct = contentType
		}
		return c.newRequest(ctx, method, path, idempotencyKey, ct, bodyReader)
	})
	if err != nil {
		return err
	}

	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return &NetworkError{Err: err}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			if err := json.Unmarshal(respBody, result); err != nil {
				return fmt.Errorf("aether: failed to decode response: %w", err)
			}
		}
		return nil
	}

	var errResp errorResponse
	msg := resp.Status
	if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
		msg = errResp.Error
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg, ErrorCode: errResp.Code}
}

func (c *Client) sleepBackoff(ctx context.Context, attempt int, resp *http.Response) {
	delay := time.Duration(float64(c.retryBackoff) * math.Pow(2, float64(attempt)))

	// For 429 responses, respect the Retry-After header if present.
	if resp != nil && resp.StatusCode == 429 {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				delay = time.Duration(secs) * time.Second
			}
		}
	}

	// Add jitter: random 0-50% of delay. Guard against sub-2ns delays, where
	// int64(delay)/2 would be 0 and rand.Int63n(0) would panic.
	var jitter time.Duration
	if half := int64(delay) / 2; half > 0 {
		jitter = time.Duration(rand.Int63n(half))
	}
	timer := time.NewTimer(delay + jitter)
	select {
	case <-ctx.Done():
		timer.Stop()
	case <-timer.C:
	}
}

// doWithRetry executes an HTTP request with exponential backoff retry logic.
// It retries on status codes 429, 502, 503, 504, and network/connection errors.
// The buildReq function is called on each attempt to construct a fresh request.
func (c *Client) doWithRetry(ctx context.Context, buildReq func() (*http.Request, error)) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		req, err := buildReq()
		if err != nil {
			return nil, &NetworkError{Err: err}
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = &NetworkError{Err: err}
			if attempt < c.maxRetries {
				c.sleepBackoff(ctx, attempt, nil)
				continue
			}
			return nil, lastErr
		}

		if isRetryableStatus(resp.StatusCode) && attempt < c.maxRetries {
			// Drain body before retry to reuse connections.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			c.sleepBackoff(ctx, attempt, resp)
			continue
		}

		return resp, nil
	}
	return nil, lastErr
}

// doJSONNoRetry sends a single HTTP request without retries and decodes the
// JSON response into result. Used for streaming uploads where the body is not
// re-readable — the body is raw document content, so no Content-Type is set
// (the content_type query param carries the document's type; see doRaw).
func (c *Client) doJSONNoRetry(ctx context.Context, method, path string, body io.Reader, result any) error {
	if c.cfgErr != nil {
		return c.cfgErr
	}
	idempotencyKey := ""
	if method == http.MethodPost {
		idempotencyKey = newIdempotencyKey()
	}
	req, err := c.newRequest(ctx, method, path, idempotencyKey, "", body)
	if err != nil {
		return &NetworkError{Err: err}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &NetworkError{Err: err}
	}

	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return &NetworkError{Err: err}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if result != nil {
			if err := json.Unmarshal(respBody, result); err != nil {
				return fmt.Errorf("aether: failed to decode response: %w", err)
			}
		}
		return nil
	}

	var errResp errorResponse
	msg := resp.Status
	if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
		msg = errResp.Error
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg, ErrorCode: errResp.Code}
}

func (c *Client) doRaw(ctx context.Context, path string) ([]byte, error) {
	if c.cfgErr != nil {
		return nil, c.cfgErr
	}
	resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return c.newRequest(ctx, http.MethodGet, path, "", "", nil)
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &NetworkError{Err: err}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp errorResponse
		msg := resp.Status
		if json.Unmarshal(data, &errResp) == nil && errResp.Error != "" {
			msg = errResp.Error
		}
		return nil, &APIError{StatusCode: resp.StatusCode, Message: msg, ErrorCode: errResp.Code}
	}

	return data, nil
}

func (c *Client) doVoid(ctx context.Context, method, path string) error {
	return c.doJSON(ctx, method, path, nil, nil)
}

// ── Functional Options ───────────────────────────────────────────

// insertConfig holds optional parameters for insert operations.
type insertConfig struct {
	tags         []string
	metadata     Metadata
	chunkSize    int
	overlap      int
	entityID     string
	source       string
	extractFacts bool
}

// InsertOption configures an insert operation.
type InsertOption func(*insertConfig)

// WithTags sets metadata tags for the document.
func WithTags(tags []string) InsertOption {
	return func(c *insertConfig) { c.tags = tags }
}

// WithMetadata sets structured metadata for the document. Values should be
// strings, numbers, or booleans; timestamp metadata should use RFC 3339 strings.
func WithMetadata(metadata Metadata) InsertOption {
	return func(c *insertConfig) { c.metadata = metadata }
}

// WithChunking sets the chunk size and overlap for document splitting.
// chunkSize must be > 0 and overlap must be >= 0; invalid values are ignored.
func WithChunking(chunkSize, overlap int) InsertOption {
	return func(c *insertConfig) {
		if chunkSize > 0 {
			c.chunkSize = chunkSize
		}
		if overlap >= 0 {
			c.overlap = overlap
		}
	}
}

// WithEntityID associates the document with an entity (e.g. a user or
// customer id). Documents can later be filtered by entity in list and search.
func WithEntityID(id string) InsertOption {
	return func(c *insertConfig) { c.entityID = id }
}

// WithSource labels the document's origin (e.g. "slack", "upload", "crawler").
// Documents can later be filtered by source in list and search.
func WithSource(s string) InsertOption {
	return func(c *insertConfig) { c.source = s }
}

// WithExtractFacts requests server-side fact extraction: the inserted text is
// distilled into atomic facts, each stored as a sibling document tagged
// "kind:fact" and linked to this document. Requires fact extraction to be
// configured on the node, otherwise the insert fails.
func WithExtractFacts(enabled bool) InsertOption {
	return func(c *insertConfig) { c.extractFacts = enabled }
}

func setJSONParam(params url.Values, key string, value any) error {
	switch v := value.(type) {
	case nil:
		return nil
	case Metadata:
		if len(v) == 0 {
			return nil
		}
	case MetadataFilter:
		if len(v) == 0 {
			return nil
		}
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("aether: failed to encode %s: %w", key, err)
	}
	params.Set(key, string(payload))
	return nil
}

// applyInsertParams adds tags, structured metadata, entity, source, and chunking query parameters to the URL values.
func applyInsertParams(params url.Values, cfg insertConfig) error {
	if len(cfg.tags) > 0 {
		params.Set("tags", strings.Join(cfg.tags, ","))
	}
	if err := setJSONParam(params, "metadata", cfg.metadata); err != nil {
		return err
	}
	if cfg.entityID != "" {
		params.Set("entity_id", cfg.entityID)
	}
	if cfg.source != "" {
		params.Set("source", cfg.source)
	}
	if cfg.chunkSize > 0 {
		params.Set("chunk_size", fmt.Sprintf("%d", cfg.chunkSize))
	}
	if cfg.overlap > 0 {
		params.Set("overlap", fmt.Sprintf("%d", cfg.overlap))
	}
	if cfg.extractFacts {
		params.Set("extract_facts", "true")
	}
	return nil
}

// searchConfig holds optional parameters for search operations.
type searchConfig struct {
	includeContent bool
	tags           []string
	anyTags        []string
	contentTypes   []string
	sources        []string
	filter         MetadataFilter
	entityID       string
	since          string
	until          string
	lastNDays      int
	maxDistance    float32
	recencyWeight  float32
	halfLifeDays   float32

	freshnessWeight       float32
	freshnessHalfLifeDays float32
}

// SearchOption configures a search operation.
type SearchOption func(*searchConfig)

// WithIncludeContent requests document content inline in search results.
func WithIncludeContent() SearchOption {
	return func(c *searchConfig) { c.includeContent = true }
}

// WithSearchTags filters search results by metadata tags. A document must carry
// ALL of the given tags to match (AND).
func WithSearchTags(tags []string) SearchOption {
	return func(c *searchConfig) { c.tags = tags }
}

// WithAnyTags filters search results to documents carrying AT LEAST ONE of the
// given metadata tags (OR).
func WithAnyTags(tags ...string) SearchOption {
	return func(c *searchConfig) { c.anyTags = tags }
}

// WithContentTypes filters search results to documents whose content type is any
// one of the given values (OR), e.g. "application/pdf", "text/markdown".
func WithContentTypes(contentTypes ...string) SearchOption {
	return func(c *searchConfig) { c.contentTypes = contentTypes }
}

// WithSources filters search results to documents whose source is any one of the
// given values (OR), e.g. "slack", "upload" (source is set at insert time via
// WithSource).
func WithSources(sources ...string) SearchOption {
	return func(c *searchConfig) { c.sources = sources }
}

// WithMetadataFilter filters search results by structured metadata. Keys may be
// "metadata.<key>" or bare metadata keys; values are equality shorthand or
// operator objects using eq/ne/gt/lt/gte/lte/in.
func WithMetadataFilter(filter MetadataFilter) SearchOption {
	return func(c *searchConfig) { c.filter = filter }
}

// WithSearchEntityID filters search results to documents associated with the
// given entity id (set at insert time via WithEntityID).
func WithSearchEntityID(id string) SearchOption {
	return func(c *searchConfig) { c.entityID = id }
}

// WithSince filters search results to documents created at or after the given
// RFC 3339 timestamp (e.g. "2026-06-01T00:00:00Z"). The bound is inclusive.
func WithSince(ts string) SearchOption {
	return func(c *searchConfig) { c.since = ts }
}

// WithUntil filters search results to documents created at or before the given
// RFC 3339 timestamp (e.g. "2026-06-30T23:59:59Z"). The bound is inclusive.
func WithUntil(ts string) SearchOption {
	return func(c *searchConfig) { c.until = ts }
}

// WithLastNDays filters search results to documents created within the last
// n days (server clock, UTC). It cannot be combined with WithSince but may be
// combined with WithUntil. n must be >= 1; invalid values are ignored.
func WithLastNDays(n int) SearchOption {
	return func(c *searchConfig) {
		if n > 0 {
			c.lastNDays = n
		}
	}
}

// WithMaxDistance sets an optional relevance-distance ceiling: results whose
// distance from the query exceeds d are dropped server-side. Smaller is
// stricter (0.0 = exact match, ~1.0 = unrelated). d must be > 0; invalid
// values are ignored (top-k returned regardless of distance).
func WithMaxDistance(d float32) SearchOption {
	return func(c *searchConfig) {
		if d > 0 {
			c.maxDistance = d
		}
	}
}

// WithRecency blends server-side recency into the result ranking. weight is the
// recency_weight in [0, 1] (0 = pure relevance, 1 = strongly favor recent
// documents) and is forwarded as recency_weight. halfLifeDays is the recency
// decay half-life in days; pass <= 0 to leave it at the server default (30
// days), in which case only recency_weight is sent. weight is clamped to [0, 1].
//
// A combined option keeps the two recency knobs together (the server treats them
// as a pair) and avoids colliding with the Memory facade's client-side
// WithRecencyWeight RecallOption.
func WithRecency(weight, halfLifeDays float32) SearchOption {
	return func(c *searchConfig) {
		if weight < 0 {
			weight = 0
		} else if weight > 1 {
			weight = 1
		}
		c.recencyWeight = weight
		if halfLifeDays > 0 {
			c.halfLifeDays = halfLifeDays
		}
	}
}

// WithHalfLifeDays sets the recency decay half-life (in days) for recency-blended
// ranking, forwarded as half_life_days. It is only meaningful together with a
// positive recency weight (see WithRecency); on its own it sets the half-life the
// server would use once recency is enabled. h must be > 0; invalid values are
// ignored.
func WithHalfLifeDays(h float32) SearchOption {
	return func(c *searchConfig) {
		if h > 0 {
			c.halfLifeDays = h
		}
	}
}

// WithFreshness blends server-side freshness into the result ranking, boosting
// documents that were recently updated (updated_at, falling back to created_at
// for never-updated documents). weight is the freshness_weight in [0, 1]
// (0 = pure relevance, 1 = strongly favor freshly updated documents) and is
// forwarded as freshness_weight. halfLifeDays is the freshness decay half-life
// in days; pass <= 0 to leave it at the server default (14 days), in which case
// only freshness_weight is sent. weight is clamped to [0, 1].
//
// Freshness composes with WithRecency; the server rejects a combined
// recency + freshness weight above 1. May require a Scale plan or higher.
func WithFreshness(weight, halfLifeDays float32) SearchOption {
	return func(c *searchConfig) {
		if weight < 0 {
			weight = 0
		} else if weight > 1 {
			weight = 1
		}
		c.freshnessWeight = weight
		if halfLifeDays > 0 {
			c.freshnessHalfLifeDays = halfLifeDays
		}
	}
}

// WithFreshnessHalfLifeDays sets the freshness decay half-life (in days) for
// freshness-blended ranking, forwarded as freshness_half_life_days. It is only
// meaningful together with a positive freshness weight (see WithFreshness); on
// its own it sets the half-life the server would use once freshness is enabled.
// h must be > 0; invalid values are ignored.
func WithFreshnessHalfLifeDays(h float32) SearchOption {
	return func(c *searchConfig) {
		if h > 0 {
			c.freshnessHalfLifeDays = h
		}
	}
}

// joinCSV comma-joins a slice for the wire, returning the empty string for an
// empty slice so omitempty fields are dropped from the request body.
func joinCSV(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return strings.Join(vals, ",")
}

// applySearchParams adds entity, metadata-facet, and time-window filter query
// parameters to the URL values. Zero values are treated as unset and omitted.
// The OR-list facets (any_tags, content_type, source) are comma-joined, the
// same CSV convention used for tags.
func applySearchParams(params url.Values, cfg searchConfig) error {
	if len(cfg.anyTags) > 0 {
		params.Set("any_tags", strings.Join(cfg.anyTags, ","))
	}
	if len(cfg.contentTypes) > 0 {
		params.Set("content_type", strings.Join(cfg.contentTypes, ","))
	}
	if len(cfg.sources) > 0 {
		params.Set("source", strings.Join(cfg.sources, ","))
	}
	if err := setJSONParam(params, "filter", cfg.filter); err != nil {
		return err
	}
	if cfg.entityID != "" {
		params.Set("entity_id", cfg.entityID)
	}
	if cfg.since != "" {
		params.Set("since", cfg.since)
	}
	if cfg.until != "" {
		params.Set("until", cfg.until)
	}
	if cfg.lastNDays > 0 {
		params.Set("last_n_days", fmt.Sprintf("%d", cfg.lastNDays))
	}
	if cfg.maxDistance > 0 {
		params.Set("max_distance", strconv.FormatFloat(float64(cfg.maxDistance), 'f', -1, 32))
	}
	if cfg.recencyWeight > 0 {
		params.Set("recency_weight", strconv.FormatFloat(float64(cfg.recencyWeight), 'f', -1, 32))
	}
	if cfg.halfLifeDays > 0 {
		params.Set("half_life_days", strconv.FormatFloat(float64(cfg.halfLifeDays), 'f', -1, 32))
	}
	if cfg.freshnessWeight > 0 {
		params.Set("freshness_weight", strconv.FormatFloat(float64(cfg.freshnessWeight), 'f', -1, 32))
	}
	if cfg.freshnessHalfLifeDays > 0 {
		params.Set("freshness_half_life_days", strconv.FormatFloat(float64(cfg.freshnessHalfLifeDays), 'f', -1, 32))
	}
	return nil
}

// applyPartitionParam adds the handle's partition to the query params when the
// client is scoped. An empty (unscoped) partition is omitted, so an unscoped
// client sends exactly what it sent before. The value is encoded by
// url.Values.Encode the same way entity_id is.
func (c *Client) applyPartitionParam(params url.Values) {
	if c.partition != "" {
		params.Set("partition", c.partition)
	}
}

// appendPartitionParam appends the handle's partition as a query param to a
// path that otherwise carries no partition-aware params — the doc_id-addressed
// routes, where it acts as a guard: a scoped client can no longer reach another
// partition's document via a bare doc id (a mismatch is the same 404 as a
// nonexistent id). An unscoped client leaves the path untouched, so unscoped
// requests are byte-identical to before.
func (c *Client) appendPartitionParam(path string) string {
	if c.partition == "" {
		return path
	}
	params := url.Values{}
	c.applyPartitionParam(params)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + params.Encode()
}

// ── Documents ─────────────────────────────────────────────────────

// GuessContentType returns a MIME type for the given filename based on its extension.
// Returns "application/octet-stream" for unknown extensions.
func GuessContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	m := map[string]string{
		".pdf":  "application/pdf",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".doc":  "application/msword",
		".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".xls":  "application/vnd.ms-excel",
		".csv":  "text/csv",
		".html": "text/html",
		".htm":  "text/html",
		".json": "application/json",
		".xml":  "application/xml",
		".md":   "text/markdown",
		".txt":  "text/plain",
	}
	if ct, ok := m[ext]; ok {
		return ct
	}
	return "application/octet-stream"
}

// Insert uploads a document from raw bytes.
// If contentType is empty, it is guessed from the filename extension.
func (c *Client) Insert(ctx context.Context, data []byte, filename, contentType string, opts ...InsertOption) (*DocumentRecord, error) {
	if filename == "" {
		return nil, fmt.Errorf("aether: filename cannot be empty")
	}
	if contentType == "" {
		contentType = GuessContentType(filename)
	}
	var cfg insertConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{
		"filename":     {filename},
		"content_type": {contentType},
	}
	if err := applyInsertParams(params, cfg); err != nil {
		return nil, err
	}
	c.applyPartitionParam(params)
	var doc DocumentRecord
	err := c.doRawBody(ctx, http.MethodPost, "/documents?"+params.Encode(), bytes.NewReader(data), &doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// InsertStream uploads a document from a streaming io.Reader without buffering
// the entire body in memory. Unlike Insert, this method does not retry on
// transient errors because the stream may not be re-readable.
// If filename is empty it defaults to "upload.bin".
// If contentType is empty it is guessed from the filename extension.
func (c *Client) InsertStream(ctx context.Context, r io.Reader, filename, contentType string, opts ...InsertOption) (*DocumentRecord, error) {
	if filename == "" {
		filename = "upload.bin"
	}
	if contentType == "" {
		contentType = GuessContentType(filename)
	}
	var cfg insertConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{
		"filename":     {filename},
		"content_type": {contentType},
	}
	// Tags, entity association, and source apply to streams; chunking does not
	// (server handles stream chunking).
	if len(cfg.tags) > 0 {
		params.Set("tags", strings.Join(cfg.tags, ","))
	}
	if cfg.entityID != "" {
		params.Set("entity_id", cfg.entityID)
	}
	if cfg.source != "" {
		params.Set("source", cfg.source)
	}
	if err := setJSONParam(params, "metadata", cfg.metadata); err != nil {
		return nil, err
	}
	c.applyPartitionParam(params)
	var doc DocumentRecord
	err := c.doJSONNoRetry(ctx, http.MethodPost, "/documents?"+params.Encode(), r, &doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// InsertText uploads raw text content.
func (c *Client) InsertText(ctx context.Context, text, filename string, opts ...InsertOption) (*DocumentRecord, error) {
	if text == "" {
		return nil, fmt.Errorf("aether: text cannot be empty")
	}
	if filename == "" {
		filename = "text.txt"
	}
	return c.Insert(ctx, []byte(text), filename, "text/plain", opts...)
}

// Update replaces an existing document.
// If contentType is empty, it is guessed from the filename extension.
func (c *Client) Update(ctx context.Context, docID string, data []byte, filename, contentType string, opts ...InsertOption) (*DocumentRecord, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	if filename == "" {
		return nil, fmt.Errorf("aether: filename cannot be empty")
	}
	if contentType == "" {
		contentType = GuessContentType(filename)
	}
	var cfg insertConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{
		"filename":     {filename},
		"content_type": {contentType},
	}
	if err := applyInsertParams(params, cfg); err != nil {
		return nil, err
	}
	c.applyPartitionParam(params)
	var doc DocumentRecord
	err := c.doRawBody(ctx, http.MethodPut,
		"/documents/"+url.PathEscape(docID)+"?"+params.Encode(),
		bytes.NewReader(data), &doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// Get retrieves document metadata. Under a partition handle the partition is
// sent as a guard: a document in a different partition is a 404, identical to
// a nonexistent id.
func (c *Client) Get(ctx context.Context, docID string) (*DocumentRecord, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	var doc DocumentRecord
	err := c.doJSON(ctx, http.MethodGet, c.appendPartitionParam("/documents/"+url.PathEscape(docID)), nil, &doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// Lineage retrieves the signed provenance/lineage trail for a document: the
// ordered list of committed actions (insert, update, tombstone, …) recorded in
// the ledger, each with its cryptographic AuditProof. The endpoint is
// tenant-scoped by the API key, so no partition guard is sent; a document that
// does not exist for the calling tenant is a 404, identical to Get.
func (c *Client) Lineage(ctx context.Context, docID string) ([]AuditRecord, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	var resp struct {
		DocID   string        `json:"doc_id"`
		Records []AuditRecord `json:"records"`
	}
	err := c.doJSON(ctx, http.MethodGet, "/audit/records/"+url.PathEscape(docID), nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Records, nil
}

// Download retrieves a document's raw bytes. Under a partition handle the
// partition is sent as a guard, exactly as in Get.
func (c *Client) Download(ctx context.Context, docID string) ([]byte, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	return c.doRaw(ctx, c.appendPartitionParam("/documents/"+url.PathEscape(docID)+"/download"))
}

// DownloadText retrieves a document's content as a string.
func (c *Client) DownloadText(ctx context.Context, docID string) (string, error) {
	if docID == "" {
		return "", fmt.Errorf("aether: docID cannot be empty")
	}
	data, err := c.Download(ctx, docID)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// ListOptions configures pagination and filtering for the List operation.
// Zero-value fields are treated as unset and omitted from the request.
type ListOptions struct {
	// Offset is the number of documents to skip (for pagination). Default: 0.
	Offset int
	// Limit is the maximum number of documents to return. Default: 50.
	Limit int
	// EntityID filters the listing to documents associated with the given
	// entity id (set at insert time via WithEntityID).
	EntityID string
	// Tags filters the listing to documents carrying ALL of these metadata
	// tags (AND).
	Tags []string
	// AnyTags filters the listing to documents carrying AT LEAST ONE of these
	// metadata tags (OR).
	AnyTags []string
	// ContentTypes filters the listing to documents whose content type is any
	// one of these values (OR).
	ContentTypes []string
	// Sources filters the listing to documents whose source is any one of these
	// values (OR), where source is set at insert time via WithSource.
	Sources []string
	// Filter is a structured metadata filter with equality or operator predicates.
	Filter MetadataFilter
	// Since filters the listing to documents created at or after this
	// RFC 3339 timestamp (e.g. "2026-06-01T00:00:00Z"). The bound is inclusive.
	Since string
	// Until filters the listing to documents created at or before this
	// RFC 3339 timestamp. The bound is inclusive.
	Until string
	// LastNDays filters the listing to documents created within the last
	// N days (server clock, UTC). It cannot be combined with Since but may
	// be combined with Until.
	LastNDays int
}

// ListResult contains a paginated list of documents returned by List.
type ListResult struct {
	// Documents is the slice of document records for the current page.
	Documents []DocumentRecord
	// Total is the total number of active documents in the collection.
	Total int `json:"total"`
	// HasMore is true when additional pages of results are available.
	HasMore bool `json:"has_more"`
}

// List returns active documents with pagination.
// Pass nil for opts to use server defaults (offset=0, limit=50).
func (c *Client) List(ctx context.Context, opts *ListOptions) (*ListResult, error) {
	params := url.Values{}
	if opts != nil {
		if opts.Offset > 0 {
			params.Set("offset", fmt.Sprintf("%d", opts.Offset))
		}
		if opts.Limit > 0 {
			params.Set("limit", fmt.Sprintf("%d", opts.Limit))
		}
		if opts.EntityID != "" {
			params.Set("entity_id", opts.EntityID)
		}
		if len(opts.Tags) > 0 {
			params.Set("tags", strings.Join(opts.Tags, ","))
		}
		if len(opts.AnyTags) > 0 {
			params.Set("any_tags", strings.Join(opts.AnyTags, ","))
		}
		if len(opts.ContentTypes) > 0 {
			params.Set("content_type", strings.Join(opts.ContentTypes, ","))
		}
		if len(opts.Sources) > 0 {
			params.Set("source", strings.Join(opts.Sources, ","))
		}
		if err := setJSONParam(params, "filter", opts.Filter); err != nil {
			return nil, err
		}
		if opts.Since != "" {
			params.Set("since", opts.Since)
		}
		if opts.Until != "" {
			params.Set("until", opts.Until)
		}
		if opts.LastNDays > 0 {
			params.Set("last_n_days", fmt.Sprintf("%d", opts.LastNDays))
		}
	}
	c.applyPartitionParam(params)
	path := "/documents"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var resp struct {
		Documents []DocumentRecord `json:"documents"`
		Count     int              `json:"count"`
		Total     int              `json:"total"`
		HasMore   bool             `json:"has_more"`
	}
	err := c.doJSON(ctx, http.MethodGet, path, nil, &resp)
	if err != nil {
		return nil, err
	}
	return &ListResult{Documents: resp.Documents, Total: resp.Total, HasMore: resp.HasMore}, nil
}

// Delete tombstones a document (soft delete): it is hidden from list/search and
// can be brought back with Restore. Under a partition handle the partition is
// sent as a guard, exactly as in Get.
func (c *Client) Delete(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("aether: docID cannot be empty")
	}
	return c.doVoid(ctx, http.MethodDelete, c.appendPartitionParam("/documents/"+url.PathEscape(docID)))
}

// HardDelete permanently purges a document: it is removed from the primary
// store and both the vector and keyword indexes, and its encryption key
// is shredded. This is irreversible — nothing is recoverable afterwards (the
// right-to-be-forgotten path). Use Delete for a recoverable tombstone. Under a
// partition handle the partition is sent as a guard, exactly as in Get.
func (c *Client) HardDelete(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("aether: docID cannot be empty")
	}
	return c.doVoid(ctx, http.MethodDelete, c.appendPartitionParam("/documents/"+url.PathEscape(docID)+"?hard=true"))
}

// Restore un-tombstones a document. Under a partition handle the partition is
// sent as a guard, exactly as in Get.
func (c *Client) Restore(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("aether: docID cannot be empty")
	}
	return c.doVoid(ctx, http.MethodPost, c.appendPartitionParam("/documents/"+url.PathEscape(docID)+"/restore"))
}

// BackfillEntityFromTags backfills entity_id on the tenant's existing documents
// from a tag convention. For every active document, a tag starting with
// tagPrefix (e.g. "patient:") sets entity_id to the suffix after the prefix when
// exactly one such tag exists; ambiguous (2+) or absent matches are skipped.
// Documents that already have an entity_id are left alone unless overwrite is
// true. This is a metadata-only operation: documents are not re-embedded.
// It returns an EntityBackfillReport summarizing how documents were classified.
// Under a partition handle the scan is constrained to that partition; a
// multi-tenant key must be scoped (400 partition_required otherwise).
func (c *Client) BackfillEntityFromTags(ctx context.Context, tagPrefix string, overwrite bool) (*EntityBackfillReport, error) {
	if tagPrefix == "" {
		return nil, fmt.Errorf("aether: tagPrefix cannot be empty")
	}
	payload, err := json.Marshal(backfillEntityRequest{
		TagPrefix: tagPrefix,
		Overwrite: overwrite,
	})
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var report EntityBackfillReport
	if err := c.doJSON(ctx, http.MethodPost, c.appendPartitionParam("/documents/backfill-entity"), bytes.NewReader(payload), &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// MoveDocument re-homes a document into another partition — the explicit,
// metadata-only partition move and the only way to move a document between
// named partitions. from asserts the partition the document lives in NOW and
// to is the destination; nil names the default partition for either. Content,
// CID, chunks, and vectors are unchanged (no re-embed); Version increments on
// a real move. A wrong from assertion, a missing id, or a tombstoned id is the
// same 404 as a nonexistent document (the call is never a partition-existence
// oracle); from == to is an idempotent 200 no-op. Returns the updated record.
//
// A move operates on the partition boundary itself, so it names both
// partitions explicitly and is never scoped by a partition handle.
func (c *Client) MoveDocument(ctx context.Context, docID string, from, to *string) (*DocumentRecord, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	// Non-nil partition ids are validated like the handle id; nil is exempt
	// because it is a meaningful value (the default partition), not an omission.
	if from != nil {
		if err := validatePartition(*from); err != nil {
			return nil, err
		}
	}
	if to != nil {
		if err := validatePartition(*to); err != nil {
			return nil, err
		}
	}
	payload, err := json.Marshal(moveDocumentRequest{
		ToPartition:     to,
		ExpectPartition: from,
	})
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var doc DocumentRecord
	if err := c.doJSON(ctx, http.MethodPost, "/documents/"+url.PathEscape(docID)+"/move", bytes.NewReader(payload), &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// ── Search ────────────────────────────────────────────────────────

// stampQueryID copies the response-level usage-feedback query_id (present only
// when feedback capture is enabled for the tenant) onto every hit, so a caller
// can pass any hit's QueryID straight to SendSearchFeedback. A nil queryID
// (feedback capture disabled) leaves every hit's QueryID nil — the tolerant
// path for servers that don't send the field.
func stampQueryID(results []SearchResult, queryID *string) {
	if queryID == nil {
		return
	}
	for i := range results {
		results[i].QueryID = queryID
	}
}

// Search performs a similarity search across documents.
func (c *Client) Search(ctx context.Context, query string, k int, opts ...SearchOption) ([]SearchResult, error) {
	if query == "" {
		return nil, fmt.Errorf("aether: query cannot be empty")
	}
	if k < 1 {
		return nil, fmt.Errorf("aether: k must be at least 1")
	}
	var cfg searchConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{
		"q": {query},
		"k": {fmt.Sprintf("%d", k)},
	}
	if cfg.includeContent {
		params.Set("include_content", "true")
	}
	if len(cfg.tags) > 0 {
		params.Set("tags", strings.Join(cfg.tags, ","))
	}
	if err := applySearchParams(params, cfg); err != nil {
		return nil, err
	}
	c.applyPartitionParam(params)
	var resp searchResponse
	err := c.doJSON(ctx, http.MethodGet, "/search?"+params.Encode(), nil, &resp)
	if err != nil {
		return nil, err
	}
	stampQueryID(resp.Results, resp.QueryID)
	return resp.Results, nil
}

// SendSearchFeedback reports how a search result was actually used, tying
// retrieval quality back to real outcomes. signal is one of "used" (the hit
// informed the answer), "cited" (quoted/referenced directly), or "ignored"
// (retrieved but unused).
//
// Requires usage-feedback capture to be enabled for your tenant; search
// results then carry a QueryID to pass here (nil otherwise). The server
// rejects an unknown queryID with 404 and an invalid signal with 400 (both
// surface as *APIError).
func (c *Client) SendSearchFeedback(ctx context.Context, queryID, docID, signal string) error {
	if queryID == "" {
		return fmt.Errorf("aether: queryID cannot be empty")
	}
	if docID == "" {
		return fmt.Errorf("aether: docID cannot be empty")
	}
	if signal == "" {
		return fmt.Errorf("aether: signal cannot be empty")
	}
	payload, err := json.Marshal(searchFeedbackRequest{
		QueryID: queryID,
		DocID:   docID,
		Signal:  signal,
	})
	if err != nil {
		return fmt.Errorf("aether: failed to encode request: %w", err)
	}
	return c.doJSON(ctx, http.MethodPost, "/search/feedback", bytes.NewReader(payload), nil)
}

// Retrieve performs a search and returns results enriched with document content.
// Results are deduplicated by DocID (highest-scoring match wins). Content is
// returned inline when the server supports it; otherwise it falls back to
// downloading each unique document's text by ID.
func (c *Client) Retrieve(ctx context.Context, query string, k int, opts ...SearchOption) ([]RetrievalResult, error) {
	if query == "" {
		return nil, fmt.Errorf("aether: query cannot be empty")
	}
	if k < 1 {
		return nil, fmt.Errorf("aether: k must be at least 1")
	}
	// Always request inline content to avoid extra downloads when possible.
	searchOpts := append([]SearchOption{WithIncludeContent()}, opts...)
	results, err := c.Search(ctx, query, k, searchOpts...)
	if err != nil {
		return nil, err
	}

	// Deduplicate by DocID, keeping the best match (search returns
	// highest-scoring first).
	seen := make(map[string]SearchResult)
	var unique []SearchResult
	for _, r := range results {
		if _, ok := seen[r.DocID]; !ok {
			seen[r.DocID] = r
			unique = append(unique, r)
		}
	}

	out := make([]RetrievalResult, 0, len(unique))
	for _, r := range unique {
		var content string
		if r.Content != nil {
			content = *r.Content
		} else {
			downloaded, err := c.DownloadText(ctx, r.DocID)
			if err != nil {
				return nil, fmt.Errorf("aether: failed to download doc %s: %w", r.DocID, err)
			}
			content = downloaded
		}
		out = append(out, RetrievalResult{
			DocID:       r.DocID,
			Score:       r.Score,
			Content:     content,
			Title:       r.Title,
			EntityID:    r.EntityID,
			ContentType: r.ContentType,
			Passage:     r.Passage,
			Tags:        r.Tags,
			Source:      r.Source,
			Partition:   r.Partition,
			Metadata:    r.Metadata,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
		})
	}
	return out, nil
}

// SearchTrace is like Search, but additionally returns evidence of which
// partition(s) the query actually touched. The trace is computed from the
// records the query returned, so it is evidence — not intent. Under a
// partition handle the scope is injected exactly as in Search; the trace then
// shows the boundary held (PartitionsTouched is empty or [ScopedTo], and
// CandidatesInScope is the partition's own size).
func (c *Client) SearchTrace(ctx context.Context, query string, k int, opts ...SearchOption) (*TracedSearch, error) {
	if query == "" {
		return nil, fmt.Errorf("aether: query cannot be empty")
	}
	if k < 1 {
		return nil, fmt.Errorf("aether: k must be at least 1")
	}
	var cfg searchConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{
		"q":     {query},
		"k":     {fmt.Sprintf("%d", k)},
		"trace": {"true"},
	}
	if cfg.includeContent {
		params.Set("include_content", "true")
	}
	if len(cfg.tags) > 0 {
		params.Set("tags", strings.Join(cfg.tags, ","))
	}
	if err := applySearchParams(params, cfg); err != nil {
		return nil, err
	}
	c.applyPartitionParam(params)
	var resp tracedSearchResponse
	if err := c.doJSON(ctx, http.MethodGet, "/search?"+params.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	stampQueryID(resp.Results, resp.QueryID)
	return &TracedSearch{Results: resp.Results, Trace: resp.Trace}, nil
}

// VerifyIsolation self-tests that a scoped search never leaks out of this
// partition. It runs SearchTrace under the handle's partition and checks that
// every returned record stayed in scope: OK is true iff nothing leaked.
//
// It is only valid on a partition handle — calling it on an unscoped client
// returns an error. It is only meaningful for a query that returns results; a
// 0-result query passes vacuously (Results == 0). Drop one line into your own
// tests to prove isolation against your data:
//
//	check, err := client.Partition("client-42").VerifyIsolation(ctx, "returns policy")
//	// then assert check.OK
func (c *Client) VerifyIsolation(ctx context.Context, query string) (*IsolationCheck, error) {
	if c.partition == "" {
		return nil, fmt.Errorf("aether: VerifyIsolation requires a partition handle — call client.Partition(id).VerifyIsolation(...)")
	}
	traced, err := c.SearchTrace(ctx, query, 10)
	if err != nil {
		return nil, err
	}
	scoped := c.partition
	leaked := make([]string, 0)
	for _, p := range traced.Trace.PartitionsTouched {
		if p != scoped {
			leaked = append(leaked, p)
		}
	}
	ok := len(leaked) == 0 && !traced.Trace.DefaultPartitionTouched
	scopedCopy := scoped
	return &IsolationCheck{
		OK:                ok,
		ScopedTo:          &scopedCopy,
		PartitionsTouched: traced.Trace.PartitionsTouched,
		Results:           traced.Trace.Results,
		CandidatesInScope: traced.Trace.CandidatesInScope,
		Leaked:            leaked,
	}, nil
}

// ── Partitions ────────────────────────────────────────────────────

// ListPartitions lists this tenant's partitions with their active document
// counts. It is tenant-level and does not use the partition handle. The result
// includes advisory warnings flagging likely typos or ghost partitions; the
// default (unkeyed) partition is not listed.
func (c *Client) ListPartitions(ctx context.Context) (*PartitionList, error) {
	var list PartitionList
	if err := c.doJSON(ctx, http.MethodGet, "/partitions", nil, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// DeletePartition deletes a partition and shreds every document in it (active
// and tombstoned) in a single call — the one-call client-offboarding teardown.
// It is tenant-level and names the target explicitly, so it does not use the
// partition handle. It returns the number of documents deleted. It is
// idempotent: deleting an unknown or empty partition returns 0 and is never an
// error.
func (c *Client) DeletePartition(ctx context.Context, partitionID string) (int, error) {
	if err := validatePartition(partitionID); err != nil {
		return 0, err
	}
	var resp partitionDeleteResponse
	if err := c.doJSON(ctx, http.MethodDelete, "/partitions/"+url.PathEscape(partitionID), nil, &resp); err != nil {
		return 0, err
	}
	return resp.DocumentsDeleted, nil
}

// InsertWithEmbeddings uploads a document with precomputed embeddings (BYOE).
func (c *Client) InsertWithEmbeddings(ctx context.Context, content string, opts InsertWithEmbeddingsOptions) (*DocumentRecord, error) {
	if content == "" {
		return nil, fmt.Errorf("aether: content cannot be empty")
	}
	body := insertWithEmbeddingsRequest{
		Content:     content,
		Passages:    opts.Passages,
		Embedding:   opts.Embedding,
		Filename:    opts.Filename,
		ContentType: opts.ContentType,
		Tags:        opts.Tags,
		Metadata:    opts.Metadata,
		Partition:   c.partition,
	}
	if opts.EntityID != "" {
		body.EntityID = &opts.EntityID
	}
	if opts.Source != "" {
		body.Source = &opts.Source
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var doc DocumentRecord
	if err := c.doJSON(ctx, http.MethodPost, "/documents/embed", bytes.NewReader(payload), &doc); err != nil {
		return nil, err
	}
	return &doc, nil
}

// SearchByVector performs a similarity search using a raw embedding vector (BYOE).
func (c *Client) SearchByVector(ctx context.Context, embedding []float32, k int, opts ...SearchOption) ([]SearchResult, error) {
	if len(embedding) == 0 {
		return nil, fmt.Errorf("aether: embedding cannot be empty")
	}
	if k < 1 {
		return nil, fmt.Errorf("aether: k must be at least 1")
	}
	var cfg searchConfig
	for _, o := range opts {
		o(&cfg)
	}
	body := vectorSearchRequest{
		Embedding:      embedding,
		K:              k,
		IncludeContent: cfg.includeContent,
		Tags:           cfg.tags,
		AnyTags:        cfg.anyTags,
		ContentType:    cfg.contentTypes,
		Source:         cfg.sources,
		EntityID:       cfg.entityID,
		Since:          cfg.since,
		Until:          cfg.until,
		LastNDays:      cfg.lastNDays,
		MaxDistance:    cfg.maxDistance,
		RecencyWeight:  cfg.recencyWeight,
		HalfLifeDays:   cfg.halfLifeDays,

		FreshnessWeight:       cfg.freshnessWeight,
		FreshnessHalfLifeDays: cfg.freshnessHalfLifeDays,

		Filter:    cfg.filter,
		Partition: c.partition,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var resp searchResponse
	if err := c.doJSON(ctx, http.MethodPost, "/search/embed", bytes.NewReader(payload), &resp); err != nil {
		return nil, err
	}
	stampQueryID(resp.Results, resp.QueryID)
	return resp.Results, nil
}

// ── Async & Batch ────────────────────────────────────────────────

// InsertAsync uploads a document for asynchronous background processing.
// Returns an AsyncJobResult containing a job ID that can be polled with WaitForJob
// to track completion. This is useful for large documents that would otherwise
// exceed request timeout limits.
func (c *Client) InsertAsync(ctx context.Context, data []byte, filename, contentType string, opts ...InsertOption) (*AsyncJobResult, error) {
	if filename == "" {
		return nil, fmt.Errorf("aether: filename cannot be empty")
	}
	if contentType == "" {
		contentType = GuessContentType(filename)
	}
	var cfg insertConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{
		"filename":     {filename},
		"content_type": {contentType},
	}
	if err := applyInsertParams(params, cfg); err != nil {
		return nil, err
	}
	c.applyPartitionParam(params)
	var result AsyncJobResult
	err := c.doRawBody(ctx, http.MethodPost, "/documents/async?"+params.Encode(), bytes.NewReader(data), &result)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// WaitForJob polls a background job until it reaches a terminal state ("completed" or "failed")
// or the timeout is reached. Pass zero for timeout or pollInterval to use defaults (60s and 1s
// respectively). Returns a *JobStatus with the final state, or an APIError with status 408 on timeout.
func (c *Client) WaitForJob(ctx context.Context, jobID string, timeout, pollInterval time.Duration) (*JobStatus, error) {
	if jobID == "" {
		return nil, fmt.Errorf("aether: jobID cannot be empty")
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if pollInterval <= 0 {
		pollInterval = time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status JobStatus
		err := c.doJSON(ctx, http.MethodGet, "/documents/jobs/"+url.PathEscape(jobID), nil, &status)
		if err != nil {
			return nil, err
		}
		if status.Status == "completed" || status.Status == "failed" {
			return &status, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return nil, &APIError{StatusCode: 408, Message: "Job polling timed out"}
}

// BatchInsert uploads multiple documents in a single request and returns a
// DocumentRecord for each successfully inserted document. Chunking options
// (WithChunking) apply uniformly to all documents in the batch.
func (c *Client) BatchInsert(ctx context.Context, documents []BatchInsertItem, opts ...InsertOption) ([]DocumentRecord, error) {
	if len(documents) == 0 {
		return nil, fmt.Errorf("aether: documents cannot be empty")
	}
	var cfg insertConfig
	for _, o := range opts {
		o(&cfg)
	}
	items := make([]batchInsertItemWire, len(documents))
	for i, d := range documents {
		items[i] = batchInsertItemWire{
			Filename:  d.Filename,
			Content:   d.Content,
			Tags:      joinCSV(d.Tags),
			EntityID:  d.EntityID,
			Source:    d.Source,
			Metadata:  d.Metadata,
			Partition: c.partition,
		}
	}
	payload := batchInsertRequest{
		Documents: items,
	}
	if cfg.chunkSize > 0 {
		payload.ChunkSize = &cfg.chunkSize
	}
	if cfg.overlap > 0 {
		payload.Overlap = &cfg.overlap
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var resp batchInsertResponse
	if err := c.doJSON(ctx, http.MethodPost, "/documents/batch", bytes.NewReader(data), &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// BatchSearch performs multiple similarity search queries in a single request.
// Each query can specify its own k, tags, and include_content settings.
// Returns one BatchSearchResponse per input query, in the same order.
func (c *Client) BatchSearch(ctx context.Context, queries []BatchSearchQuery) ([]BatchSearchResponse, error) {
	if len(queries) == 0 {
		return nil, fmt.Errorf("aether: queries cannot be empty")
	}
	wire := make([]batchSearchQueryWire, len(queries))
	for i, q := range queries {
		wire[i] = batchSearchQueryWire{
			Q:              q.Q,
			K:              q.K,
			Tags:           joinCSV(q.Tags),
			AnyTags:        joinCSV(q.AnyTags),
			ContentType:    joinCSV(q.ContentTypes),
			Source:         joinCSV(q.Sources),
			Filter:         q.Filter,
			IncludeContent: q.IncludeContent,
			EntityID:       q.EntityID,
			Since:          q.Since,
			Until:          q.Until,
			LastNDays:      q.LastNDays,
			MaxDistance:    q.MaxDistance,
			RecencyWeight:  q.RecencyWeight,
			HalfLifeDays:   q.HalfLifeDays,

			FreshnessWeight:       q.FreshnessWeight,
			FreshnessHalfLifeDays: q.FreshnessHalfLifeDays,

			Partition: c.partition,
		}
	}
	payload, err := json.Marshal(batchSearchRequest{Queries: wire})
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var resp batchSearchResponseWrapper
	if err := c.doJSON(ctx, http.MethodPost, "/search/batch", bytes.NewReader(payload), &resp); err != nil {
		return nil, err
	}
	// The feedback handle arrives per query; stamp it onto that query's hits.
	out := make([]BatchSearchResponse, len(resp.Results))
	for i, r := range resp.Results {
		stampQueryID(r.Results, r.QueryID)
		out[i] = BatchSearchResponse{Query: r.Query, Results: r.Results}
	}
	return out, nil
}

// ── Cluster ───────────────────────────────────────────────────────

// Status returns the node status.
func (c *Client) Status(ctx context.Context) (*NodeStatus, error) {
	var s NodeStatus
	err := c.doJSON(ctx, http.MethodGet, "/status", nil, &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// ArchivePrice fetches the live $/GiB price for permanent archive uploads
// (Arweave/Irys). Mirrors the gateway's 5-minute cached upstream price.
// Useful for showing customers their archive cost before flipping the
// PermanentArchive toggle. The server returns 404 when the gateway is
// configured without an upstream URL — surfaces here as an APIError.
func (c *Client) ArchivePrice(ctx context.Context) (*ArchivePrice, error) {
	var p ArchivePrice
	err := c.doJSON(ctx, http.MethodGet, "/archive/price", nil, &p)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// Note: Cluster operations (Sync, Snapshot, Checkpoint, Recover, Validate)
// are admin-only and not exposed in the public SDK. Use the REST API
// directly with an admin API key for operational tasks.
