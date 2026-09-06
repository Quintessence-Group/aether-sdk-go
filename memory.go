package aether

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory is an entity-scoped, ergonomic facade over the raw Client. Construct it
// once with an entity id (a user, a customer, a patient, an agent session) and
// every call is automatically scoped to that entity.
//
// Memory owns a Client by composition. Text calls compose document routes and
// media calls use the additive media-memory route. Existing behavior and transport,
// retry, error, and timeout semantics are inherited
// unchanged from the underlying Client — Memory surfaces the same error types
// (*APIError, *NetworkError, sentinels) without additional wrapping.
//
//	mem, _ := aether.NewMemory("patient-john", aether.WithAPIKey("ak_..."))
//	mem.Remember(ctx, "Anxious about flying; uses 4-7-8 breathing", nil)
//	hits, _ := mem.Recall(ctx, "anxiety coping")
type Memory struct {
	client       *Client
	entityID     string
	halfLifeDays float64
	extractFacts bool
	// now is an injectable clock for deterministic recency scoring in tests.
	// It defaults to func() time.Time { return time.Now().UTC() }.
	now func() time.Time
}

// MemoryItem is the shared result type returned by Remember, Recall, and List.
//
// CreatedAt is populated by Remember and List, and by Recall only when a positive
// recency weight is used (see Recall). It is an unparsed RFC 3339 string.
//
// Score is a relevance signal (higher = more relevant) populated by Recall only;
// it is relative within a single Recall call and not comparable across calls.
type MemoryItem struct {
	// ID is the underlying doc_id.
	ID string
	// Text is the remembered text.
	Text string
	// CreatedAt is the RFC 3339 creation timestamp (unparsed), or nil.
	CreatedAt *string
	// EntityID is the owning entity id (always the Memory's entity id).
	EntityID *string
	// Metadata is the structured metadata attached to the memory.
	Metadata Metadata
	// Score is the relevance signal for Recall results, or nil.
	Score *float64
	// Modality is "image" or "audio" for a multimodal memory, or nil for text.
	Modality *string
}

// MemoryMediaOption configures Memory.RememberImage and Memory.RememberAudio.
// A Memory convenience accepts bytes, a local path, or an HTTP(S) URL; URL
// fetches occur in the caller process and are never forwarded to the server.
type MemoryMediaOption func(*memoryMediaConfig)

type memoryMediaConfig struct {
	contentType    string
	filename       string
	caption        string
	transcript     string
	metadata       Metadata
	source         string
	autoTranscribe bool
}

// WithMediaContentType explicitly sets the media MIME type. It is only needed
// when the SDK cannot infer the type from a path/URL name, HTTP response
// header, or media magic bytes.
func WithMediaContentType(contentType string) MemoryMediaOption {
	return func(c *memoryMediaConfig) { c.contentType = contentType }
}

// WithMediaFilename sets optional display metadata for the original media.
func WithMediaFilename(filename string) MemoryMediaOption {
	return func(c *memoryMediaConfig) { c.filename = filename }
}

// WithMediaCaption supplies the caller-derived caption for an image memory.
// A supplied caption bypasses automatic server-side vision processing.
func WithMediaCaption(caption string) MemoryMediaOption {
	return func(c *memoryMediaConfig) { c.caption = caption }
}

// WithMediaTranscript supplies the caller-derived transcript for an audio
// memory. A supplied transcript bypasses automatic server-side transcription.
func WithMediaTranscript(transcript string) MemoryMediaOption {
	return func(c *memoryMediaConfig) { c.transcript = transcript }
}

// WithMediaMetadata attaches structured metadata to the media memory. It is
// also mirrored into legacy key:value tags when that representation is lossless.
func WithMediaMetadata(metadata Metadata) MemoryMediaOption {
	return func(c *memoryMediaConfig) { c.metadata = metadata }
}

// WithMediaSource labels the media memory's origin, for example "camera" or
// "voice-note".
func WithMediaSource(source string) MemoryMediaOption {
	return func(c *memoryMediaConfig) { c.source = source }
}

// WithMediaAutoTranscription controls whether an audio memory without an
// explicit transcript may use the server's configured transcription processor.
// It defaults to true. Setting it false requires WithMediaTranscript.
func WithMediaAutoTranscription(enabled bool) MemoryMediaOption {
	return func(c *memoryMediaConfig) { c.autoTranscribe = enabled }
}

const (
	// defaultHalfLifeDays is the recency half-life used when WithHalfLife is not
	// provided. At one half-life the recency contribution is 0.5.
	defaultHalfLifeDays = 30.0
	// maxEntityIDLen is the server's entity_id length constraint.
	maxEntityIDLen = 256

	// scoreScale normalizes the calibrated 0–100 relevance score (higher =
	// better) onto the [0, 1] range so it shares a scale with the recency term
	// in the Mode B blend.
	scoreScale = 100.0

	// recencyOverfetch is the candidate over-fetch factor for recency re-ranking.
	recencyOverfetch = 4
	// recencyMaxCandidates caps the over-fetched candidate set for recency re-ranking.
	recencyMaxCandidates = 100
	// recencyGetConcurrency bounds the number of concurrent Get calls used to
	// resolve created_at during recency re-ranking.
	recencyGetConcurrency = 8

	// forgetAllPageSize is the listing page size used by ForgetAll.
	forgetAllPageSize = 1000

	defaultThreadTurns    = 10
	threadSemanticMatches = 5
	maxThreadTurns        = 1000
	threadContextMaxBytes = 16 * 1024 * 1024
)

// MemoryOption configures a Memory beyond the entity id. These apply to both
// NewMemory and NewMemoryWithClient.
type MemoryOption func(*Memory)

// WithHalfLife sets the recency half-life used by Recall when a positive recency
// weight is supplied. At one half-life the recency contribution is 0.5. Defaults
// to 30 days. Non-positive durations are ignored.
func WithHalfLife(d time.Duration) MemoryOption {
	return func(m *Memory) {
		if d > 0 {
			m.halfLifeDays = d.Hours() / 24.0
		}
	}
}

// WithFactExtraction enables server-side fact extraction for this Memory:
// Remember distills the text into atomic facts, each stored as a sibling
// "kind:fact" memory and recallable like any other. Requires fact extraction to
// be configured on the node. Default disabled. The flag applies to every
// Remember on this Memory (Go has no per-call override); use the raw client's
// WithExtractFacts insert option for one-off control.
func WithFactExtraction(enabled bool) MemoryOption {
	return func(m *Memory) { m.extractFacts = enabled }
}

// WithClock injects a clock used for recency scoring. It is test-only/advanced;
// the default is the system UTC time. A nil function is ignored.
func WithClock(now func() time.Time) MemoryOption {
	return func(m *Memory) {
		if now != nil {
			m.now = now
		}
	}
}

func validateEntityID(entityID string) error {
	if strings.TrimSpace(entityID) == "" {
		return fmt.Errorf("aether: entityID cannot be empty")
	}
	if len(entityID) > maxEntityIDLen {
		return fmt.Errorf("aether: entityID must be 1-%d characters, got %d", maxEntityIDLen, len(entityID))
	}
	return nil
}

func newMemory(entityID string, client *Client, opts ...MemoryOption) (*Memory, error) {
	if err := validateEntityID(entityID); err != nil {
		return nil, err
	}
	m := &Memory{
		client:       client,
		entityID:     entityID,
		halfLifeDays: defaultHalfLifeDays,
		now:          func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// NewMemory builds a Memory with its own Client constructed from the given client
// options (the same options accepted by New, e.g. WithAPIKey, WithBaseURL).
// Memory-specific options (WithHalfLife, WithFactExtraction, WithClock) are not
// accepted here; use NewMemoryWithClient when you need them, or set the half-life
// via the underlying client is not applicable. The entity id is validated
// client-side (non-empty, 1-256 chars) — an invalid id returns an error without a
// network round-trip.
func NewMemory(entityID string, opts ...Option) (*Memory, error) {
	if err := validateEntityID(entityID); err != nil {
		return nil, err
	}
	return newMemory(entityID, New(opts...))
}

// NewMemoryWithClient builds a Memory around an already-configured Client
// (dependency injection). Use this to share one client across many entities, and
// in tests. Memory-specific options (WithHalfLife, WithFactExtraction, WithClock)
// are applied here. The entity id is validated client-side (non-empty, 1-256
// chars) — an invalid id returns an error without a network round-trip.
func NewMemoryWithClient(entityID string, c *Client, opts ...MemoryOption) (*Memory, error) {
	if c == nil {
		return nil, fmt.Errorf("aether: client cannot be nil")
	}
	return newMemory(entityID, c, opts...)
}

// EntityID returns the entity id this Memory is scoped to.
func (m *Memory) EntityID() string { return m.entityID }

// Client returns the underlying raw Client. Useful for advanced operations not
// exposed by Memory (e.g. restore).
func (m *Memory) Client() *Client { return m.client }

// Thread creates an entity-scoped ordered conversation helper. It is
// equivalent to NewThread(m, threadID).
func (m *Memory) Thread(threadID string) (*Thread, error) {
	return NewThread(m, threadID)
}

// Thread composes a canonical ordered conversation over an entity-scoped
// Memory. Recent turns and semantic matches are returned as one flat
// []MemoryItem context list.
type Thread struct {
	memory   *Memory
	threadID string
}

// NewThread constructs a Thread directly. Memory.Thread is shorthand.
func NewThread(memory *Memory, threadID string) (*Thread, error) {
	if memory == nil {
		return nil, fmt.Errorf("aether: memory cannot be nil")
	}
	if err := validateThreadID(threadID); err != nil {
		return nil, err
	}
	return &Thread{memory: memory, threadID: threadID}, nil
}

// ThreadID returns the canonical conversation identity.
func (t *Thread) ThreadID() string { return t.threadID }

// Append adds one turn and automatically scopes it to the Memory's entity.
func (t *Thread) Append(ctx context.Context, text string, metadata any) (*MemoryItem, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("aether: text cannot be empty")
	}
	metadataMap, err := normalizeMemoryMetadata(metadata)
	if err != nil {
		return nil, err
	}
	doc, err := t.memory.client.AppendThread(ctx, t.threadID, ThreadAppendInput{
		Text:     text,
		Metadata: metadataMap,
		EntityID: t.memory.entityID,
	})
	if err != nil {
		return nil, err
	}
	entityID := doc.EntityID
	if entityID == nil {
		entityID = &t.memory.entityID
	}
	itemMetadata := doc.Metadata
	if len(itemMetadata) == 0 {
		itemMetadata = metadataMap
	}
	return &MemoryItem{
		ID:        doc.DocID,
		Text:      text,
		CreatedAt: doc.CreatedAt,
		EntityID:  entityID,
		Metadata:  itemMetadata,
	}, nil
}

// Context returns a bounded recent window followed by up to five additional
// semantic matches from the same entity and thread. Duplicate document IDs are
// returned once. lastNTurns=0 uses the default window of ten; otherwise it must
// be between 1 and 1000. Recent turns are chronological unless recentFirst is
// true. At most eight downloads run concurrently and recent + semantic content
// shares a 16 MiB UTF-8/body-byte budget.
func (t *Thread) Context(
	ctx context.Context,
	query string,
	lastNTurns int,
	recentFirst bool,
) ([]MemoryItem, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("aether: query cannot be empty")
	}
	if lastNTurns == 0 {
		lastNTurns = defaultThreadTurns
	}
	if lastNTurns < 1 || lastNTurns > maxThreadTurns {
		return nil, fmt.Errorf("aether: lastNTurns must be between 1 and %d", maxThreadTurns)
	}

	readOptions := []ThreadReadOption{WithLastNThreadTurns(lastNTurns)}
	if recentFirst {
		readOptions = append(readOptions, WithRecentThreadTurns())
	}
	var (
		conversation *ConversationThread
		matches      []RetrievalResult
		recentErr    error
		semanticErr  error
		wg           sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		conversation, recentErr = t.memory.client.GetThread(ctx, t.threadID, readOptions...)
	}()
	go func() {
		defer wg.Done()
		matches, semanticErr = t.memory.client.Retrieve(
			ctx,
			query,
			threadSemanticMatches,
			WithSearchEntityID(t.memory.entityID),
			WithSearchThreadID(t.threadID),
		)
	}()
	wg.Wait()
	if recentErr != nil {
		return nil, recentErr
	}
	if semanticErr != nil {
		return nil, semanticErr
	}

	records := make([]DocumentRecord, 0, len(conversation.Documents))
	for _, record := range conversation.Documents {
		if record.EntityID != nil && *record.EntityID == t.memory.entityID {
			records = append(records, record)
		}
	}
	texts, contextBytes, err := t.memory.resolveThreadContextTexts(ctx, records)
	if err != nil {
		return nil, err
	}
	items := make([]MemoryItem, 0, len(records)+len(matches))
	seen := make(map[string]struct{}, len(records)+len(matches))
	for index, record := range records {
		seen[record.DocID] = struct{}{}
		items = append(items, MemoryItem{
			ID:        record.DocID,
			Text:      texts[index],
			CreatedAt: record.CreatedAt,
			EntityID:  record.EntityID,
			Metadata:  record.Metadata,
		})
	}
	for _, match := range matches {
		if _, exists := seen[match.DocID]; exists {
			continue
		}
		seen[match.DocID] = struct{}{}
		if contextBytes+len(match.Content) > threadContextMaxBytes {
			return nil, fmt.Errorf(
				"aether: thread context exceeds the %d-byte safety limit",
				threadContextMaxBytes,
			)
		}
		contextBytes += len(match.Content)
		score := float64(match.Score) / scoreScale
		entityID := t.memory.entityID
		items = append(items, MemoryItem{
			ID:       match.DocID,
			Text:     match.Content,
			EntityID: &entityID,
			Metadata: match.Metadata,
			Score:    &score,
		})
	}
	return items, nil
}

// Restore un-tombstones this conversation (the thread-level analog of the raw
// client's Restore), making its turns visible to Context, Recall, and List
// again. It is owner/admin-scoped.
func (t *Thread) Restore(ctx context.Context) (*ThreadLifecycleResult, error) {
	return t.memory.client.ThreadRestore(ctx, t.threadID)
}

// SetACL sets the read-ACL applied to this conversation. A nil readers pointer
// unlabels the thread (tenant-visible); a non-nil pointer to an empty slice
// quarantines it to admins only; a non-nil pointer to a non-empty slice
// restricts reads to exactly those principals. It is owner/admin-scoped.
func (t *Thread) SetACL(ctx context.Context, readers *[]string) (*ThreadLifecycleResult, error) {
	return t.memory.client.ThreadSetACL(ctx, t.threadID, readers)
}

// Move re-homes this conversation into another partition. The Memory's current
// partition is asserted as the expected source (expect_partition); a nil
// toPartition names the default partition. It is owner/admin-scoped.
func (t *Thread) Move(ctx context.Context, toPartition *string) (*ThreadLifecycleResult, error) {
	var expect *string
	if p := t.memory.client.partition; p != "" {
		expect = &p
	}
	return t.memory.client.ThreadMove(ctx, t.threadID, expect, toPartition)
}

// Delete tombstones this conversation. By default it is a recoverable soft
// tombstone (restore with Restore); hard=true performs the irreversible crypto
// hard delete (right-to-be-forgotten) from which nothing is recoverable. It is
// owner/admin-scoped.
func (t *Thread) Delete(ctx context.Context, hard bool) (*ThreadLifecycleResult, error) {
	return t.memory.client.ThreadDelete(ctx, t.threadID, hard)
}

// resolveThreadContextTexts preserves turn order, schedules at most eight
// downloads at a time, and stops before scheduling another batch once the
// shared UTF-8/body-byte budget is exceeded.
func (m *Memory) resolveThreadContextTexts(
	ctx context.Context,
	records []DocumentRecord,
) ([]string, int, error) {
	texts := make([]string, len(records))
	totalBytes := 0
	for offset := 0; offset < len(records); offset += recencyGetConcurrency {
		end := offset + recencyGetConcurrency
		if end > len(records) {
			end = len(records)
		}
		var (
			mu       sync.Mutex
			firstErr error
			wg       sync.WaitGroup
		)
		for index := offset; index < end; index++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				text, err := m.resolveMemoryText(ctx, records[idx])
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				texts[idx] = text
			}(index)
		}
		wg.Wait()
		if firstErr != nil {
			return nil, 0, firstErr
		}
		for _, text := range texts[offset:end] {
			totalBytes += len(text)
			if totalBytes > threadContextMaxBytes {
				return nil, 0, fmt.Errorf(
					"aether: thread context exceeds the %d-byte safety limit",
					threadContextMaxBytes,
				)
			}
		}
	}
	return texts, totalBytes, nil
}

// Remember stores one memory for this entity. It performs a single HTTP call.
//
// metadata (optional) is sent as structured typed document metadata. For older
// tag-based callers, string-safe metadata is also mirrored into key:value tags
// where doing so is lossless. Keys must be non-empty.
//
// Empty/whitespace-only text is a client-side argument error.
//
// When fact extraction is enabled (WithFactExtraction), the inserted text is
// distilled server-side into atomic facts, each stored as a sibling "kind:fact"
// memory; the returned item is still the raw memory (not the facts).
func (m *Memory) Remember(ctx context.Context, text string, metadata any) (*MemoryItem, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("aether: text cannot be empty")
	}
	metadataMap, err := normalizeMemoryMetadata(metadata)
	if err != nil {
		return nil, err
	}
	tags, err := encodeMetadataTags(metadataMap)
	if err != nil {
		return nil, err
	}
	opts := []InsertOption{WithEntityID(m.entityID)}
	if len(tags) > 0 {
		opts = append(opts, WithTags(tags))
	}
	if len(metadataMap) > 0 {
		opts = append(opts, WithMetadata(metadataMap))
	}
	if m.extractFacts {
		opts = append(opts, WithExtractFacts(true))
	}
	doc, err := m.client.InsertText(ctx, text, "", opts...)
	if err != nil {
		return nil, err
	}
	entityID := m.entityID
	return &MemoryItem{
		ID:        doc.DocID,
		Text:      text,
		CreatedAt: doc.CreatedAt,
		EntityID:  &entityID,
		Metadata:  doc.Metadata,
		Score:     nil,
	}, nil
}

// RememberImage stores an entity-scoped image memory. source may be []byte, a
// local path string, an HTTP(S) URL string, or *url.URL. The SDK loads URL/path
// inputs in the caller process, then sends only the resulting bytes to the
// authenticated Aether media route.
//
// Use WithMediaCaption to supply caller-derived text and avoid automatic server
// vision processing. Otherwise the server's configured processor derives the
// indexed caption.
func (m *Memory) RememberImage(ctx context.Context, source any, opts ...MemoryMediaOption) (*MemoryItem, error) {
	return m.rememberMediaSource(ctx, source, "image", opts...)
}

// RememberAudio stores an entity-scoped audio memory. source may be []byte, a
// local path string, an HTTP(S) URL string, or *url.URL. The SDK loads URL/path
// inputs in the caller process, then sends only the resulting bytes to the
// authenticated Aether media route.
//
// Use WithMediaTranscript to supply caller-derived text and avoid automatic
// server transcription. Automatic transcription is enabled by default;
// WithMediaAutoTranscription(false) requires an explicit transcript.
func (m *Memory) RememberAudio(ctx context.Context, source any, opts ...MemoryMediaOption) (*MemoryItem, error) {
	return m.rememberMediaSource(ctx, source, "audio", opts...)
}

func (m *Memory) rememberMediaSource(ctx context.Context, source any, modality string, opts ...MemoryMediaOption) (*MemoryItem, error) {
	cfg := memoryMediaConfig{autoTranscribe: true}
	for _, opt := range opts {
		opt(&cfg)
	}
	if modality == "image" && cfg.transcript != "" {
		return nil, fmt.Errorf("aether: transcript is valid only for audio memories")
	}
	if modality == "audio" && cfg.caption != "" {
		return nil, fmt.Errorf("aether: caption is valid only for image memories")
	}
	if modality == "audio" && !cfg.autoTranscribe && cfg.transcript == "" {
		return nil, fmt.Errorf("aether: automatic transcription disabled; provide a transcript")
	}

	media, contentType, filename, err := loadMemoryMediaSource(
		ctx, m.client, source, modality, cfg.contentType, cfg.filename,
	)
	if err != nil {
		return nil, err
	}
	tags, err := encodeMetadataTags(cfg.metadata)
	if err != nil {
		return nil, err
	}
	record, err := m.client.RememberMedia(ctx, media, MediaMemoryOptions{
		Modality:    modality,
		ContentType: contentType,
		EntityID:    m.entityID,
		Filename:    filename,
		Caption:     cfg.caption,
		Transcript:  cfg.transcript,
		Tags:        tags,
		Metadata:    cfg.metadata,
		Source:      cfg.source,
	})
	if err != nil {
		return nil, err
	}

	entityID := m.entityID
	if record.EntityID != nil && *record.EntityID != "" {
		entityID = *record.EntityID
	}
	returnedModality := record.Modality
	if returnedModality == "" {
		returnedModality = modality
	}
	metadata := record.Metadata
	if len(metadata) == 0 && len(cfg.metadata) > 0 {
		metadata = cfg.metadata
	}
	return &MemoryItem{
		ID:        record.DocID,
		Text:      record.DerivedText,
		CreatedAt: record.CreatedAt,
		EntityID:  &entityID,
		Metadata:  metadata,
		Score:     nil,
		Modality:  &returnedModality,
	}, nil
}

// loadMemoryMediaSource normalizes a supported Memory media source to bytes.
// It deliberately uses a plain HTTP request for caller URLs, never Client's
// authenticated request helper, so an Aether API key is not sent to arbitrary
// media hosts.
func loadMemoryMediaSource(
	ctx context.Context,
	client *Client,
	source any,
	modality, explicitContentType, explicitFilename string,
) ([]byte, string, string, error) {
	if u, ok := source.(*url.URL); ok {
		if u == nil {
			return nil, "", "", fmt.Errorf("aether: media source cannot be nil")
		}
		source = u.String()
	}

	filename := explicitFilename
	var (
		data                []byte
		detectedContentType string
		err                 error
	)
	switch value := source.(type) {
	case []byte:
		data = value
	case string:
		if parsed, isHTTPURL := parseMemoryMediaURL(value); isHTTPURL {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
			if reqErr != nil {
				return nil, "", "", fmt.Errorf("aether: create media URL request: %w", reqErr)
			}
			httpClient := http.DefaultClient
			if client != nil && client.httpClient != nil {
				httpClient = client.httpClient
			}
			resp, requestErr := httpClient.Do(req)
			if requestErr != nil {
				return nil, "", "", fmt.Errorf("aether: fetch media URL: %w", requestErr)
			}
			defer resp.Body.Close()
			if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
				return nil, "", "", fmt.Errorf("aether: media URL returned HTTP %d", resp.StatusCode)
			}
			data, err = io.ReadAll(resp.Body)
			if err != nil {
				return nil, "", "", fmt.Errorf("aether: read media URL: %w", err)
			}
			detectedContentType = resp.Header.Get("Content-Type")
			if filename == "" {
				filename = filenameFromMediaURL(parsed)
			}
		} else {
			data, err = os.ReadFile(value)
			if err != nil {
				return nil, "", "", fmt.Errorf("aether: read media path %q: %w", value, err)
			}
			if filename == "" {
				filename = filepath.Base(value)
			}
		}
	default:
		return nil, "", "", fmt.Errorf("aether: media source must be []byte, a local path, or an HTTP(S) URL")
	}
	if len(data) == 0 {
		return nil, "", "", fmt.Errorf("aether: media cannot be empty")

	}

	contentType := normalizeMediaContentType(explicitContentType)
	if contentType == "" {
		candidate := normalizeMediaContentType(detectedContentType)
		if strings.HasPrefix(candidate, modality+"/") {
			contentType = candidate
		}
	}
	if contentType == "" {
		contentType = mediaContentTypeFromFilename(filename, modality)
	}
	if contentType == "" {
		contentType = sniffMediaContentType(data, modality)
	}
	if contentType == "" {
		return nil, "", "", fmt.Errorf("aether: contentType is required for unrecognized media bytes")
	}
	return data, contentType, filename, nil
}

func parseMemoryMediaURL(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return nil, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return parsed, scheme == "http" || scheme == "https"
}

func filenameFromMediaURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	filename := path.Base(u.Path)
	if filename == "." || filename == "/" {
		return ""
	}
	return filename
}

func mediaContentTypeFromFilename(filename, modality string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".jpg", ".jpeg":
		if modality == "image" {
			return "image/jpeg"
		}
	case ".png":
		if modality == "image" {
			return "image/png"
		}
	case ".webp":
		if modality == "image" {
			return "image/webp"
		}
	case ".gif":
		if modality == "image" {
			return "image/gif"
		}
	case ".mp3":
		if modality == "audio" {
			return "audio/mpeg"
		}
	case ".m4a", ".mp4":
		if modality == "audio" {
			return "audio/mp4"
		}
	case ".wav":
		if modality == "audio" {
			return "audio/wav"
		}
	case ".webm":
		if modality == "audio" {
			return "audio/webm"
		}
	case ".ogg":
		if modality == "audio" {
			return "audio/ogg"
		}
	case ".flac":
		if modality == "audio" {
			return "audio/flac"
		}
	}
	return ""
}

func sniffMediaContentType(data []byte, modality string) string {
	hasPrefix := func(prefix ...byte) bool { return bytes.HasPrefix(data, prefix) }
	hasASCII := func(offset int, value string) bool {
		return offset >= 0 && len(data) >= offset+len(value) && string(data[offset:offset+len(value)]) == value
	}
	if modality == "image" {
		switch {
		case hasPrefix(0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a):
			return "image/png"
		case hasPrefix(0xff, 0xd8, 0xff):
			return "image/jpeg"
		case hasASCII(0, "GIF87a") || hasASCII(0, "GIF89a"):
			return "image/gif"
		case hasASCII(0, "RIFF") && hasASCII(8, "WEBP"):
			return "image/webp"
		}
		return ""
	}

	switch {
	case hasASCII(0, "RIFF") && hasASCII(8, "WAVE"):
		return "audio/wav"
	case hasASCII(0, "ID3") || hasPrefix(0xff, 0xfb) || hasPrefix(0xff, 0xf3) || hasPrefix(0xff, 0xf2):
		return "audio/mpeg"
	case hasASCII(0, "OggS"):
		return "audio/ogg"
	case hasASCII(0, "fLaC"):
		return "audio/flac"
	case hasPrefix(0x1a, 0x45, 0xdf, 0xa3):
		return "audio/webm"
	case hasASCII(4, "ftyp"):
		return "audio/mp4"
	}
	return ""
}

func normalizeMemoryMetadata(metadata any) (Metadata, error) {
	switch v := metadata.(type) {
	case nil:
		return nil, nil
	case Metadata:
		return v, nil
	case map[string]any:
		return Metadata(v), nil
	case map[string]string:
		out := make(Metadata, len(v))
		for k, val := range v {
			out[k] = val
		}
		return out, nil
	default:
		return nil, fmt.Errorf("aether: metadata must be nil, Metadata, map[string]any, or map[string]string")
	}
}

// encodeMetadataTags best-effort mirrors structured metadata into legacy
// key:value tags. Pairs that cannot be represented losslessly in the old
// comma-joined tag format are skipped.
func encodeMetadataTags(metadata Metadata) ([]string, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(metadata))
	for k := range metadata {
		if k == "" {
			return nil, fmt.Errorf("aether: metadata key cannot be empty")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]string, 0, len(metadata))
	for _, k := range keys {
		v := fmt.Sprint(metadata[k])
		if strings.ContainsRune(k, ':') {
			return nil, fmt.Errorf("aether: metadata key %q must not contain a colon", k)
		}
		if strings.ContainsRune(k, ',') {
			return nil, fmt.Errorf("aether: metadata key %q must not contain a comma", k)
		}
		if strings.ContainsRune(v, ',') {
			return nil, fmt.Errorf("aether: metadata value for key %q must not contain a comma", k)
		}
		tags = append(tags, k+":"+v)
	}
	return tags, nil
}

// recallConfig holds the resolved options for a Recall call.
type recallConfig struct {
	k             int
	recencyWeight float64
	since         string
	until         string
	filter        MetadataFilter
}

// RecallOption configures a Recall call.
type RecallOption func(*recallConfig)

// WithRecallK sets the maximum number of memories Recall returns. It must be
// >= 1; passing k < 1 makes Recall return a client-side argument error (before
// any HTTP call). Default 5.
func WithRecallK(k int) RecallOption {
	return func(c *recallConfig) { c.k = k }
}

// WithRecencyWeight blends client-side recency decay into the relevance ranking.
// The weight is clamped to [0, 1]. At 0 (the default) Recall issues exactly one
// search call and returns server order. A positive weight enables recency mode,
// which over-fetches candidates and resolves each memory's created_at via Get
// (N+1 calls) before re-ranking.
func WithRecencyWeight(w float64) RecallOption {
	return func(c *recallConfig) { c.recencyWeight = w }
}

// WithRecallSince filters recalled memories to those created at or after the
// given RFC 3339 timestamp (inclusive). Forwarded to the server verbatim.
func WithRecallSince(ts string) RecallOption {
	return func(c *recallConfig) { c.since = ts }
}

// WithRecallUntil filters recalled memories to those created at or before the
// given RFC 3339 timestamp (inclusive). Forwarded to the server verbatim.
func WithRecallUntil(ts string) RecallOption {
	return func(c *recallConfig) { c.until = ts }
}

// WithRecallFilter filters recalled memories by structured metadata.
func WithRecallFilter(filter MetadataFilter) RecallOption {
	return func(c *recallConfig) { c.filter = filter }
}

// Recall performs a semantic search scoped to this entity, with optional
// client-side recency decay.
//
// An empty/whitespace-only query and an explicitly-requested k < 1 (via
// WithRecallK) are client-side argument errors, returned before any HTTP call.
//
// With the default recency weight of 0, Recall issues exactly one retrieve call,
// returns memories in server order (descending relevance score), populates Score
// from the hit's calibrated score, and leaves CreatedAt nil.
//
// With a positive recency weight (WithRecencyWeight), Recall over-fetches
// candidates, resolves each memory's created_at via Get (N+1 calls,
// parallelized), and re-ranks by a blend of similarity and exponential recency
// decay. See the contract for the exact, deterministic algorithm.
func (m *Memory) Recall(ctx context.Context, query string, opts ...RecallOption) ([]MemoryItem, error) {
	cfg := recallConfig{k: 5, recencyWeight: 0.0}
	for _, o := range opts {
		o(&cfg)
	}
	// Client-side argument validation (before any HTTP call): reject an empty or
	// whitespace-only query and an explicitly-requested k < 1.
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("aether: query cannot be empty")
	}
	if cfg.k < 1 {
		return nil, fmt.Errorf("aether: k must be >= 1, got %d", cfg.k)
	}
	// Clamp recency weight to [0, 1].
	w := cfg.recencyWeight
	if w < 0 {
		w = 0
	} else if w > 1 {
		w = 1
	}

	searchOpts := []SearchOption{WithSearchEntityID(m.entityID)}
	if cfg.since != "" {
		searchOpts = append(searchOpts, WithSince(cfg.since))
	}
	if cfg.until != "" {
		searchOpts = append(searchOpts, WithUntil(cfg.until))
	}
	if len(cfg.filter) > 0 {
		searchOpts = append(searchOpts, WithMetadataFilter(cfg.filter))
	}

	if w == 0 {
		return m.recallSimple(ctx, query, cfg.k, searchOpts)
	}
	return m.recallRecency(ctx, query, cfg.k, w, searchOpts)
}

// recallSimple is Mode A: one retrieve call, server order, no timestamps.
func (m *Memory) recallSimple(ctx context.Context, query string, k int, searchOpts []SearchOption) ([]MemoryItem, error) {
	hits, err := m.client.Retrieve(ctx, query, k, searchOpts...)
	if err != nil {
		return nil, err
	}
	entityID := m.entityID
	items := make([]MemoryItem, 0, len(hits))
	for _, h := range hits {
		score := similarityScore(h.Score)
		items = append(items, MemoryItem{
			ID:        h.DocID,
			Text:      h.Content,
			CreatedAt: nil,
			EntityID:  &entityID,
			Metadata:  h.Metadata,
			Score:     &score,
			Modality:  h.Modality,
		})
	}
	return items, nil
}

// recallCandidate is an intermediate carrying everything needed to re-rank.
type recallCandidate struct {
	docID     string
	text      string
	score     int
	metadata  Metadata
	createdAt *string
	modality  *string
	blended   float64
}

// recallRecency is Mode B: over-fetch, resolve created_at, blend and re-rank.
func (m *Memory) recallRecency(ctx context.Context, query string, k int, w float64, searchOpts []SearchOption) ([]MemoryItem, error) {
	overfetch := k * recencyOverfetch
	if overfetch > recencyMaxCandidates {
		overfetch = recencyMaxCandidates
	}
	if overfetch < 1 {
		overfetch = 1
	}

	hits, err := m.client.Retrieve(ctx, query, overfetch, searchOpts...)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return []MemoryItem{}, nil
	}

	candidates := make([]recallCandidate, len(hits))
	for i, h := range hits {
		candidates[i] = recallCandidate{
			docID:    h.DocID,
			text:     h.Content,
			score:    h.Score,
			metadata: h.Metadata,
			modality: h.Modality,
		}
	}

	createdByDoc, err := m.resolveCreatedAt(ctx, candidates)
	if err != nil {
		return nil, err
	}

	now := m.now()
	for i := range candidates {
		c := &candidates[i]
		c.createdAt = createdByDoc[c.docID]
		similarity := similarityScore(c.score)
		recency := recencyScore(c.createdAt, now, m.halfLifeDays)
		c.blended = (1-w)*similarity + w*recency
	}

	// Total order: blended DESC, then score DESC, then doc_id ASC.
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.blended != b.blended {
			return a.blended > b.blended
		}
		if a.score != b.score {
			return a.score > b.score
		}
		return a.docID < b.docID
	})

	if len(candidates) > k {
		candidates = candidates[:k]
	}

	entityID := m.entityID
	items := make([]MemoryItem, 0, len(candidates))
	for _, c := range candidates {
		blended := c.blended
		items = append(items, MemoryItem{
			ID:        c.docID,
			Text:      c.text,
			CreatedAt: c.createdAt,
			EntityID:  &entityID,
			Metadata:  c.metadata,
			Score:     &blended,
			Modality:  c.modality,
		})
	}
	return items, nil
}

// resolveCreatedAt fetches created_at for each unique doc id via Get, bounded by
// recencyGetConcurrency. The first error encountered is returned.
func (m *Memory) resolveCreatedAt(ctx context.Context, candidates []recallCandidate) (map[string]*string, error) {
	// Collect unique doc ids (Retrieve already de-duplicates, but be defensive).
	uniqueIDs := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if _, ok := seen[c.docID]; ok {
			continue
		}
		seen[c.docID] = struct{}{}
		uniqueIDs = append(uniqueIDs, c.docID)
	}

	result := make(map[string]*string, len(uniqueIDs))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, recencyGetConcurrency)
	for _, id := range uniqueIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(docID string) {
			defer wg.Done()
			defer func() { <-sem }()

			doc, err := m.client.Get(cctx, docID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			result[docID] = doc.CreatedAt
		}(id)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return result, nil
}

// similarityScore normalizes a calibrated 0–100 relevance score (higher =
// better) onto [0, 1] so it shares a scale with the recency term in the
// Mode B blend.
func similarityScore(score int) float64 {
	return float64(score) / scoreScale
}

// recencyScore returns the exponential half-life recency score in [0, 1] for a
// created-at timestamp relative to now. A nil/unparseable timestamp scores 0.0; a
// future timestamp is clamped to age 0 (score 1.0).
func recencyScore(createdAt *string, now time.Time, halfLifeDays float64) float64 {
	if createdAt == nil || *createdAt == "" {
		return 0.0
	}
	created, ok := parseRFC3339(*createdAt)
	if !ok {
		return 0.0
	}
	ageDays := now.Sub(created).Hours() / 24.0
	if ageDays < 0 {
		ageDays = 0
	}
	if halfLifeDays <= 0 {
		halfLifeDays = defaultHalfLifeDays
	}
	// 0.5^(age/hl) == 2^(-(age/hl)).
	return math.Pow(0.5, ageDays/halfLifeDays)
}

// parseRFC3339 parses an RFC 3339 timestamp string into a time.Time. A trailing
// "Z" is handled natively by time.RFC3339; a timestamp with no offset (naive) is
// treated as UTC. Returns ok=false when the string cannot be parsed by any of the
// accepted layouts.
func parseRFC3339(s string) (time.Time, bool) {
	// Primary: full RFC 3339 (handles "Z" and explicit offsets, with or without
	// fractional seconds).
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	// Fallback: a naive timestamp with no zone — interpret as UTC.
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// MemoryListOption configures a List call.
type MemoryListOption func(*memoryListConfig)

type memoryListConfig struct {
	since  string
	until  string
	filter MetadataFilter
	limit  int
}

// WithListSince filters listed memories to those created at or after the given
// RFC 3339 timestamp (inclusive).
func WithListSince(ts string) MemoryListOption {
	return func(c *memoryListConfig) { c.since = ts }
}

// WithListUntil filters listed memories to those created at or before the given
// RFC 3339 timestamp (inclusive).
func WithListUntil(ts string) MemoryListOption {
	return func(c *memoryListConfig) { c.until = ts }
}

// WithListLimit caps the number of memories returned. Must be >= 1; non-positive
// values are ignored. Default 50.
func WithListLimit(limit int) MemoryListOption {
	return func(c *memoryListConfig) {
		if limit > 0 {
			c.limit = limit
		}
	}
}

// WithListFilter filters listed memories by structured metadata.
func WithListFilter(filter MetadataFilter) MemoryListOption {
	return func(c *memoryListConfig) { c.filter = filter }
}

// List returns a chronological view of this entity's memories, newest first.
//
// Cost note: List is 1 + N calls for text memories — one listing plus one content
// download per text memory. Media memories use their authorized derived_text
// caption/transcript from the list response instead of decoding original binary
// downloads. Callers who only need metadata can drop to the raw client's List
// with an entity filter.
func (m *Memory) List(ctx context.Context, opts ...MemoryListOption) ([]MemoryItem, error) {
	cfg := memoryListConfig{limit: 50}
	for _, o := range opts {
		o(&cfg)
	}

	res, err := m.client.List(ctx, &ListOptions{
		EntityID: m.entityID,
		Since:    cfg.since,
		Until:    cfg.until,
		Filter:   cfg.filter,
		Limit:    cfg.limit,
	})
	if err != nil {
		return nil, err
	}

	records := res.Documents
	if len(records) > cfg.limit {
		records = records[:cfg.limit]
	}

	texts, err := m.resolveMemoryTexts(ctx, records)
	if err != nil {
		return nil, err
	}

	items := make([]MemoryItem, 0, len(records))
	for i, r := range records {
		items = append(items, MemoryItem{
			ID:        r.DocID,
			Text:      texts[i],
			CreatedAt: r.CreatedAt,
			EntityID:  r.EntityID,
			Metadata:  r.Metadata,
			Score:     nil,
			Modality:  r.Modality,
		})
	}
	return items, nil
}

// ListExtractedFacts returns this entity's consolidated extracted facts
// (kind:fact memories), highest corroborated confidence first.
//
// These are the free-text facts produced by Remember with fact extraction
// enabled (WithFactExtraction) and deduped server-side — the clean, high-signal
// view of what's known about the entity, distinct from any structured
// memory-graph facts. Cost is 1 + N (one listing plus a content download per
// fact).
func (m *Memory) ListExtractedFacts(ctx context.Context, opts ...MemoryListOption) ([]MemoryItem, error) {
	cfg := memoryListConfig{limit: 50}
	for _, o := range opts {
		o(&cfg)
	}

	res, err := m.client.List(ctx, &ListOptions{
		EntityID: m.entityID,
		Tags:     []string{"kind:fact"},
		Filter:   cfg.filter,
		Limit:    cfg.limit,
	})
	if err != nil {
		return nil, err
	}

	records := res.Documents
	// Highest corroborated confidence first; ties broken by recency.
	sort.SliceStable(records, func(i, j int) bool {
		ci, cj := factConfidence(records[i].Tags), factConfidence(records[j].Tags)
		if ci != cj {
			return ci > cj
		}
		return derefStr(records[i].CreatedAt) > derefStr(records[j].CreatedAt)
	})
	if len(records) > cfg.limit {
		records = records[:cfg.limit]
	}

	texts, err := m.resolveMemoryTexts(ctx, records)
	if err != nil {
		return nil, err
	}

	items := make([]MemoryItem, 0, len(records))
	for i, r := range records {
		items = append(items, MemoryItem{
			ID:        r.DocID,
			Text:      texts[i],
			CreatedAt: r.CreatedAt,
			EntityID:  r.EntityID,
			Metadata:  r.Metadata,
			Score:     nil,
			Modality:  r.Modality,
		})
	}
	return items, nil
}

// factConfidence parses a fact's conf:<n> tag (corroborating-source count),
// defaulting to 1 when absent or unparseable.
func factConfidence(tags []string) int {
	for _, t := range tags {
		if n, ok := strings.CutPrefix(t, "conf:"); ok {
			if v, err := strconv.Atoi(n); err == nil && v > 0 {
				return v
			}
			return 1
		}
	}
	return 1
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// resolveMemoryTexts resolves each record's retrieval-safe text, preserving
// order and bounding concurrent work. Text records use DownloadText; media
// records use derived_text (or a metadata Get fallback), never binary download.
func (m *Memory) resolveMemoryTexts(ctx context.Context, records []DocumentRecord) ([]string, error) {
	texts := make([]string, len(records))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, recencyGetConcurrency)
	for i, r := range records {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, record DocumentRecord) {
			defer wg.Done()
			defer func() { <-sem }()

			text, err := m.resolveMemoryText(cctx, record)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			texts[idx] = text
		}(i, r)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	return texts, nil
}

// resolveMemoryText returns text safe for a Memory item. Download returns the
// original media asset, so media documents must use their indexed derived text
// rather than converting binary bytes to a string.
func (m *Memory) resolveMemoryText(ctx context.Context, record DocumentRecord) (string, error) {
	if record.Modality == nil {
		return m.client.DownloadText(ctx, record.DocID)
	}
	if record.DerivedText != nil {
		return *record.DerivedText, nil
	}

	// New servers include derived_text in list responses. A metadata Get keeps
	// the no-binary-download guarantee for an otherwise compatible response that
	// omitted it.
	doc, err := m.client.Get(ctx, record.DocID)
	if err != nil {
		return "", err
	}
	if doc.DerivedText == nil {
		return "", fmt.Errorf("aether: media memory %s has no derived_text", record.DocID)
	}
	return *doc.DerivedText, nil
}

// Forget deletes a single memory by id (a soft tombstone, restorable via the raw
// client). Empty id is a client-side argument error.
func (m *Memory) Forget(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("aether: id cannot be empty")
	}
	return m.client.Delete(ctx, id)
}

// ForgetAll deletes every memory for this entity and returns the number deleted.
// It pages the entity's listing and deletes each document until the listing is
// exhausted. Because deletes are tombstones, re-listing excludes already-deleted
// docs, so the loop terminates.
func (m *Memory) ForgetAll(ctx context.Context) (int, error) {
	deleted := 0
	for {
		res, err := m.client.List(ctx, &ListOptions{
			EntityID: m.entityID,
			Limit:    forgetAllPageSize,
		})
		if err != nil {
			return deleted, err
		}
		if len(res.Documents) == 0 {
			return deleted, nil
		}
		for _, doc := range res.Documents {
			if err := m.client.Delete(ctx, doc.DocID); err != nil {
				return deleted, err
			}
			deleted++
		}
	}
}
