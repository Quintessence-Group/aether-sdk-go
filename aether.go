// Package aether provides a Go client for the Aether decentralized RAG API.
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
const Version = "0.3.3"

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

// ── Internal helpers ──────────────────────────────────────────────

func (c *Client) newRequest(ctx context.Context, method, path, contentType, idempotencyKey string, body io.Reader) (*http.Request, error) {
	reqURL := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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

func (c *Client) doJSON(ctx context.Context, method, path string, body io.Reader, result any) error {
	return c.doJSONCT(ctx, method, path, "", body, result)
}

// doJSONCT is like doJSON but sets an explicit Content-Type header on the
// request. Used by endpoints that send a JSON-encoded body; the raw document
// upload endpoints carry their content type in the query string and pass "".
func (c *Client) doJSONCT(ctx context.Context, method, path, contentType string, body io.Reader, result any) error {
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
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}
		return c.newRequest(ctx, method, path, contentType, idempotencyKey, bodyReader)
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
// re-readable.
func (c *Client) doJSONNoRetry(ctx context.Context, method, path string, body io.Reader, result any) error {
	if c.cfgErr != nil {
		return c.cfgErr
	}
	idempotencyKey := ""
	if method == http.MethodPost {
		idempotencyKey = newIdempotencyKey()
	}
	req, err := c.newRequest(ctx, method, path, "", idempotencyKey, body)
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
	tags      []string
	chunkSize int
	overlap   int
	entityID  string
}

// InsertOption configures an insert operation.
type InsertOption func(*insertConfig)

// WithTags sets metadata tags for the document.
func WithTags(tags []string) InsertOption {
	return func(c *insertConfig) { c.tags = tags }
}

// WithEntityID scopes the inserted (or updated) document to an owning entity —
// a user, agent, tenant, or any identifier of your choosing. Scoped documents
// can later be filtered at search time with WithSearchEntityID. Passes the
// value as the entity_id query parameter. An empty id is ignored (unscoped).
func WithEntityID(id string) InsertOption {
	return func(c *insertConfig) { c.entityID = id }
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

// applyInsertParams adds tags, chunking, and entity scoping query parameters to
// the URL values.
func applyInsertParams(params url.Values, cfg insertConfig) {
	if len(cfg.tags) > 0 {
		params.Set("tags", strings.Join(cfg.tags, ","))
	}
	if cfg.chunkSize > 0 {
		params.Set("chunk_size", fmt.Sprintf("%d", cfg.chunkSize))
	}
	if cfg.overlap > 0 {
		params.Set("overlap", fmt.Sprintf("%d", cfg.overlap))
	}
	if cfg.entityID != "" {
		params.Set("entity_id", cfg.entityID)
	}
}

// searchConfig holds optional parameters for search operations.
type searchConfig struct {
	tags        []string
	maxDistance *float32
	entityID    string
	since       string
	until       string
	lastNDays   int
}

// SearchOption configures a search operation.
type SearchOption func(*searchConfig)

// WithSearchTags filters search results by metadata tags.
func WithSearchTags(tags []string) SearchOption {
	return func(c *searchConfig) { c.tags = tags }
}

// WithMaxDistance sets an optional relevance-distance ceiling. Results with
// distance > max are dropped server-side. Smaller is stricter
// (0.0 = exact match, ~1.0 = unrelated). Omit (do not pass this option) to
// return the top-k regardless of distance — the historical behavior.
func WithMaxDistance(max float32) SearchOption {
	return func(c *searchConfig) { c.maxDistance = &max }
}

// WithSearchEntityID restricts results to documents scoped to the given entity
// (see WithEntityID at insert time). Pass the value as the entity_id query
// parameter. An empty id is ignored (search across all entities).
func WithSearchEntityID(id string) SearchOption {
	return func(c *searchConfig) { c.entityID = id }
}

// WithSince restricts results to documents created on or after the given
// instant (inclusive). Takes an RFC 3339 timestamp ("2026-06-01T00:00:00Z");
// sent as the since query parameter. An empty value is ignored (no lower bound).
func WithSince(since string) SearchOption {
	return func(c *searchConfig) { c.since = since }
}

// WithUntil restricts results to documents created on or before the given
// instant (inclusive). Takes an RFC 3339 timestamp; sent as the until query
// parameter. An empty value is ignored (no upper bound).
func WithUntil(until string) SearchOption {
	return func(c *searchConfig) { c.until = until }
}

// WithLastNDays restricts results to documents created within the last n days
// (server-side shorthand for since = now - n days, UTC). Sent as the integer
// last_n_days query parameter; cannot be combined with WithSince server-side. A
// non-positive n is ignored.
func WithLastNDays(n int) SearchOption {
	return func(c *searchConfig) {
		if n > 0 {
			c.lastNDays = n
		}
	}
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
	applyInsertParams(params, cfg)
	var doc DocumentRecord
	err := c.doJSON(ctx, http.MethodPost, "/documents?"+params.Encode(), bytes.NewReader(data), &doc)
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
	// Tags and entity scoping apply to streams; chunking does not (server
	// handles stream chunking).
	if len(cfg.tags) > 0 {
		params.Set("tags", strings.Join(cfg.tags, ","))
	}
	if cfg.entityID != "" {
		params.Set("entity_id", cfg.entityID)
	}
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
	applyInsertParams(params, cfg)
	var doc DocumentRecord
	err := c.doJSON(ctx, http.MethodPut,
		"/documents/"+url.PathEscape(docID)+"?"+params.Encode(),
		bytes.NewReader(data), &doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// Get retrieves document metadata.
func (c *Client) Get(ctx context.Context, docID string) (*DocumentRecord, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	var doc DocumentRecord
	err := c.doJSON(ctx, http.MethodGet, "/documents/"+url.PathEscape(docID), nil, &doc)
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// Download retrieves a document's raw bytes.
func (c *Client) Download(ctx context.Context, docID string) ([]byte, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	return c.doRaw(ctx, "/documents/"+url.PathEscape(docID)+"/download")
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
type ListOptions struct {
	// Offset is the number of documents to skip (for pagination). Default: 0.
	Offset int
	// Limit is the maximum number of documents to return. Default: 50.
	Limit int
	// EntityID restricts the listing to documents scoped to the given entity.
	// Empty lists across all entities.
	EntityID string
	// Since restricts the listing to documents created at or after the given
	// instant. Accepts an RFC 3339 timestamp or a relative window (e.g. "7d").
	// Empty applies no lower bound.
	Since string
	// Until restricts the listing to documents created at or before the given
	// instant. Accepts an RFC 3339 timestamp or a relative window (e.g. "7d").
	// Empty applies no upper bound.
	Until string
	// LastNDays restricts the listing to documents created within the last n
	// days, sent as a relative since window ("<n>d"). When > 0 it takes
	// precedence over Since. Non-positive values are ignored.
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
		// last_n_days and since are mutually exclusive server-side; prefer
		// last_n_days when set. Both are integers/RFC-3339 respectively.
		if opts.LastNDays > 0 {
			params.Set("last_n_days", strconv.Itoa(opts.LastNDays))
		} else if opts.Since != "" {
			params.Set("since", opts.Since)
		}
		if opts.Until != "" {
			params.Set("until", opts.Until)
		}
	}
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

// Delete tombstones a document.
func (c *Client) Delete(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("aether: docID cannot be empty")
	}
	return c.doVoid(ctx, http.MethodDelete, "/documents/"+url.PathEscape(docID))
}

// Restore un-tombstones a document.
func (c *Client) Restore(ctx context.Context, docID string) error {
	if docID == "" {
		return fmt.Errorf("aether: docID cannot be empty")
	}
	return c.doVoid(ctx, http.MethodPost, "/documents/"+url.PathEscape(docID)+"/restore")
}

// BackfillEntityFromTags derives an entity_id for existing documents from their
// metadata tags. For every active document carrying a tag that begins with
// tagPrefix, the server sets entity_id to the remainder of that tag (the tag
// value with tagPrefix stripped). This is a one-shot migration helper for
// collections that encoded ownership in tags before entity scoping existed.
//
// When overwrite is false, documents that already have an entity_id are left
// untouched; when true, their entity_id is replaced with the tag-derived value.
// The returned EntityBackfillReport accounts for every document the server
// considered. tagPrefix must not be empty.
func (c *Client) BackfillEntityFromTags(ctx context.Context, tagPrefix string, overwrite bool) (*EntityBackfillReport, error) {
	if tagPrefix == "" {
		return nil, fmt.Errorf("aether: tagPrefix cannot be empty")
	}
	payload, err := json.Marshal(entityBackfillRequest{TagPrefix: tagPrefix, Overwrite: overwrite})
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var report EntityBackfillReport
	if err := c.doJSONCT(ctx, http.MethodPost, "/documents/backfill-entity", "application/json", bytes.NewReader(payload), &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// ── Search ────────────────────────────────────────────────────────

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
	if len(cfg.tags) > 0 {
		params.Set("tags", strings.Join(cfg.tags, ","))
	}
	if cfg.maxDistance != nil {
		params.Set("max_distance", strconv.FormatFloat(float64(*cfg.maxDistance), 'g', -1, 32))
	}
	if cfg.entityID != "" {
		params.Set("entity_id", cfg.entityID)
	}
	// last_n_days and since are mutually exclusive server-side; prefer
	// last_n_days when set.
	if cfg.lastNDays > 0 {
		params.Set("last_n_days", strconv.Itoa(cfg.lastNDays))
	} else if cfg.since != "" {
		params.Set("since", cfg.since)
	}
	if cfg.until != "" {
		params.Set("until", cfg.until)
	}
	var resp searchResponse
	err := c.doJSON(ctx, http.MethodGet, "/search?"+params.Encode(), nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// Retrieve performs a search and returns results enriched with full document
// content for RAG workflows. Results are deduplicated by DocID (closest match
// wins). Search no longer returns document content inline, so each unique
// document's full text is fetched by ID and attached as Content.
func (c *Client) Retrieve(ctx context.Context, query string, k int, opts ...SearchOption) ([]RetrievalResult, error) {
	if query == "" {
		return nil, fmt.Errorf("aether: query cannot be empty")
	}
	if k < 1 {
		return nil, fmt.Errorf("aether: k must be at least 1")
	}
	results, err := c.Search(ctx, query, k, opts...)
	if err != nil {
		return nil, err
	}

	// Deduplicate by DocID, keeping the closest match
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
		// Search returns only the matched passage now (never full document
		// content), so fetch each unique document's text by ID for RAG prompts.
		content, err := c.DownloadText(ctx, r.DocID)
		if err != nil {
			return nil, fmt.Errorf("aether: failed to download doc %s: %w", r.DocID, err)
		}
		out = append(out, RetrievalResult{
			DocID:       r.DocID,
			Score:       r.Score,
			Content:     content,
			Title:       r.Title,
			EntityID:    r.EntityID,
			ContentType: r.ContentType,
			Passage:     r.Passage,
		})
	}
	return out, nil
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
		EntityID:    opts.EntityID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var doc DocumentRecord
	if err := c.doJSONCT(ctx, http.MethodPost, "/documents/embed", "application/json", bytes.NewReader(payload), &doc); err != nil {
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
		Embedding:   embedding,
		K:           k,
		Tags:        cfg.tags,
		MaxDistance: cfg.maxDistance,
		EntityID:    cfg.entityID,
		Since:       cfg.since,
		Until:       cfg.until,
		LastNDays:   cfg.lastNDays,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var resp searchResponse
	if err := c.doJSONCT(ctx, http.MethodPost, "/search/embed", "application/json", bytes.NewReader(payload), &resp); err != nil {
		return nil, err
	}
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
	applyInsertParams(params, cfg)
	var result AsyncJobResult
	err := c.doJSON(ctx, http.MethodPost, "/documents/async?"+params.Encode(), bytes.NewReader(data), &result)
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
	wireDocs := make([]batchInsertItemWire, len(documents))
	for i, d := range documents {
		wireDocs[i] = batchInsertItemWire{
			Filename: d.Filename,
			Content:  d.Content,
			Tags:     strings.Join(d.Tags, ","),
		}
	}
	payload := batchInsertRequest{
		Documents: wireDocs,
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
	if err := c.doJSONCT(ctx, http.MethodPost, "/documents/batch", "application/json", bytes.NewReader(data), &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
}

// BatchSearch performs multiple similarity search queries in a single request.
// Each query can specify its own k and tags.
// Returns one BatchSearchResponse per input query, in the same order.
func (c *Client) BatchSearch(ctx context.Context, queries []BatchSearchQuery) ([]BatchSearchResponse, error) {
	if len(queries) == 0 {
		return nil, fmt.Errorf("aether: queries cannot be empty")
	}
	wireQueries := make([]batchSearchQueryWire, len(queries))
	for i, q := range queries {
		wireQueries[i] = batchSearchQueryWire{
			Q:           q.Q,
			K:           q.K,
			Tags:        strings.Join(q.Tags, ","),
			MaxDistance: q.MaxDistance,
			EntityID:    q.EntityID,
			Since:       q.Since,
			Until:       q.Until,
			LastNDays:   q.LastNDays,
		}
	}
	payload, err := json.Marshal(batchSearchRequest{Queries: wireQueries})
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var resp batchSearchResponseWrapper
	if err := c.doJSONCT(ctx, http.MethodPost, "/search/batch", "application/json", bytes.NewReader(payload), &resp); err != nil {
		return nil, err
	}
	return resp.Results, nil
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
