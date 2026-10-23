package aether

// Memory files: named, versioned markdown files that belong to an entity.
//
// The entity-scoped facade (Memory.Files) covers every per-file operation; the
// raw client adds the two cross-entity calls, ListMemoryFiles and
// GrepMemoryFiles. Everything flows through the owned client's transport, so
// its partition handle, retries and error types apply unchanged.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ── Names and ETags ───────────────────────────────────────────────────────

// maxMemoryFileNameLen is the longest valid memory-file name.
const maxMemoryFileNameLen = 128

// ValidateMemoryFileName reports whether name is a valid memory-file name: 1-128
// ASCII letters, digits, '.', '_' or '-', starting with a letter or digit, not
// containing "..", and not exactly "grep". Names are case-sensitive. Every
// MemoryFiles method checks the name this way before sending a request.
func ValidateMemoryFileName(name string) error {
	if !isValidMemoryFileName(name) {
		return fmt.Errorf(
			"aether: invalid memory file name %q: use 1-%d letters, digits, '.', '_' or '-', "+
				`starting with a letter or digit, without "..", and not "grep"`,
			name, maxMemoryFileNameLen)
	}
	return nil
}

func isValidMemoryFileName(name string) bool {
	if name == "" || len(name) > maxMemoryFileNameLen || name == "grep" || strings.Contains(name, "..") {
		return false
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		alnum := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
		if alnum {
			continue
		}
		if i == 0 || (b != '.' && b != '_' && b != '-') {
			return false
		}
	}
	return true
}

// quoteETag formats a strong entity tag for If-Match: the value in double
// quotes, e.g. "7".
func quoteETag(value string) string {
	return `"` + value + `"`
}

// parseETagVersion reads a memory-file version from an ETag header value. It
// accepts "7", a weak W/"7" and a bare 7; anything else is not a version.
func parseETagVersion(etag string) (int, bool) {
	s := strings.TrimPrefix(strings.TrimSpace(etag), "W/")
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}

// versionOrETag returns version, or the ETag's version when the body carried
// none.
func versionOrETag(version int, headers http.Header) int {
	if version != 0 {
		return version
	}
	if v, ok := parseETagVersion(headers.Get("ETag")); ok {
		return v
	}
	return version
}

func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// ── Result types ──────────────────────────────────────────────────────────

// MemoryFileInfo describes the current version of a memory file (a list item),
// and is embedded in MemoryFile and MemoryFileWriteResult. Timestamps are
// unparsed RFC 3339 strings.
type MemoryFileInfo struct {
	// EntityID is the entity the file belongs to.
	EntityID string `json:"entity_id"`
	// Partition is the file's partition, or nil for the default partition.
	Partition *string `json:"partition"`
	// Name is the file name.
	Name string `json:"name"`
	// FileID is stable across a soft delete and re-create; a hard delete
	// starts a new one.
	FileID string `json:"file_id"`
	// DocID is the document holding this version.
	DocID string `json:"doc_id"`
	// Version is the file version (1, 2, …). It is unrelated to
	// DocumentRecord.Version. Pass it to WithExpectedVersion.
	Version int `json:"version"`
	// Bytes is the size of this version's content.
	Bytes int `json:"bytes"`
	// ContentHash is a hash of this version's content.
	ContentHash string `json:"content_hash"`
	// Author is the caller-supplied attribution label (WithAuthor), or nil.
	Author *string `json:"author"`
	// Principal is the authenticated identity that wrote this version.
	Principal string `json:"principal"`
	// Op is the write that produced this version: "put", "append",
	// "append_section" or "replace_section".
	Op string `json:"op"`
	// CreatedAt is when the file (its version 1) was created.
	CreatedAt string `json:"created_at"`
	// UpdatedAt is when this version was written.
	UpdatedAt string `json:"updated_at"`
}

// MemoryFile is the result of MemoryFiles.Read: one version of a file, or one
// page or section of it.
type MemoryFile struct {
	MemoryFileInfo
	// IsCurrent is false when an older version was requested.
	IsCurrent bool `json:"is_current"`
	// Content is this page of the selected text ("" with WithFileOutline).
	Content string `json:"content"`
	// Section is the selected section when WithFileSection was given.
	Section *MemoryFileSection `json:"section"`
	// Offset is the byte offset of this page within the selected text.
	Offset int `json:"offset"`
	// NextOffset is the offset of the next page, or nil on the last page.
	NextOffset *int `json:"next_offset"`
	// TotalBytes is the byte length of the selected text (the whole version,
	// or the section).
	TotalBytes int `json:"total_bytes"`
	// Truncated reports whether more pages follow (NextOffset != nil).
	Truncated bool `json:"truncated"`
	// Sections is always the whole version's outline, with whole-file line
	// numbers.
	Sections []MemoryFileSection `json:"sections"`
}

// MemoryFileWriteResult is the result of Write, Append, AppendSection and
// ReplaceSection: the file's current version after the write.
type MemoryFileWriteResult struct {
	MemoryFileInfo
	// Unchanged is true when the write would not change the content, so
	// nothing was written and the info fields describe the existing current
	// version. A retried write that already succeeded comes back this way.
	Unchanged bool `json:"unchanged"`
	// PreviousVersion is the version this write replaced, or nil when it
	// created the file.
	PreviousVersion *int `json:"previous_version"`
	// FileCreated is true when this write created the file.
	FileCreated bool `json:"file_created"`
	// Section is the affected section after an AppendSection or
	// ReplaceSection, otherwise nil.
	Section *MemoryFileSection `json:"section"`
	// SectionCreated is true when the section did not exist and was created.
	SectionCreated bool `json:"section_created"`
	// Sections is the outline of the resulting content.
	Sections []MemoryFileSection `json:"sections"`
}

// MemoryFileVersion is one retained version in MemoryFileVersions.
type MemoryFileVersion struct {
	DocID       string  `json:"doc_id"`
	Version     int     `json:"version"`
	Bytes       int     `json:"bytes"`
	ContentHash string  `json:"content_hash"`
	Author      *string `json:"author"`
	Principal   string  `json:"principal"`
	Op          string  `json:"op"`
	UpdatedAt   string  `json:"updated_at"`
	IsCurrent   bool    `json:"is_current"`
}

// MemoryFileVersions is the result of MemoryFiles.Versions: the file's retained
// versions, newest first.
type MemoryFileVersions struct {
	EntityID       string              `json:"entity_id"`
	Partition      *string             `json:"partition"`
	Name           string              `json:"name"`
	FileID         string              `json:"file_id"`
	CurrentVersion int                 `json:"current_version"`
	Versions       []MemoryFileVersion `json:"versions"`
}

// MemoryFileDiff is the result of MemoryFiles.Diff: a line-based unified diff
// between two versions, headed "--- <name>@v<from>" / "+++ <name>@v<to>".
type MemoryFileDiff struct {
	EntityID  string  `json:"entity_id"`
	Partition *string `json:"partition"`
	Name      string  `json:"name"`
	FileID    string  `json:"file_id"`
	// FromVersion is the old side; 0 denotes the empty file.
	FromVersion int `json:"from_version"`
	// ToVersion is the new side.
	ToVersion int `json:"to_version"`
	// Context is the number of unchanged context lines around each change.
	Context int `json:"context"`
	// Diff is the unified diff text ("" when the versions are identical).
	Diff         string `json:"diff"`
	AddedLines   int    `json:"added_lines"`
	RemovedLines int    `json:"removed_lines"`
	// Approximate is true when the diff is correct but may not be minimal
	// (the server hit its time limit computing it).
	Approximate bool `json:"approximate"`
	// Truncated is true when the diff output was cut short.
	Truncated bool `json:"truncated"`
}

// MemoryFilePage is one page of MemoryFiles.List or Client.ListMemoryFiles.
type MemoryFilePage struct {
	// Files are the current versions of live files, ordered by partition,
	// entity and name.
	Files []MemoryFileInfo `json:"files"`
	// NextCursor is the cursor for the next page (WithFileCursor), or nil on
	// the last page.
	NextCursor *string `json:"next_cursor"`
}

// MemoryGrepMatch is one matching line in a MemoryGrepResult.
type MemoryGrepMatch struct {
	EntityID  string  `json:"entity_id"`
	Partition *string `json:"partition"`
	Name      string  `json:"name"`
	Version   int     `json:"version"`
	// Line is the 1-based line number.
	Line int `json:"line"`
	// Section is the heading of the innermost section containing the line,
	// or nil before the first heading.
	Section *string `json:"section"`
	// SectionSlug is that section's slug, or nil.
	SectionSlug *string `json:"section_slug"`
	// Text is the matching line.
	Text string `json:"text"`
	// Before holds up to WithGrepContext lines preceding the match.
	Before []string `json:"before"`
	// After holds up to WithGrepContext lines following the match.
	After []string `json:"after"`
}

// MemoryGrepResult is the result of MemoryFiles.Grep and
// Client.GrepMemoryFiles.
type MemoryGrepResult struct {
	Matches []MemoryGrepMatch `json:"matches"`
	// Truncated is true when the scan stopped at a limit before covering
	// every file.
	Truncated bool `json:"truncated"`
	// TruncatedReason is "max_matches", "max_files", "max_bytes" or
	// "deadline" when Truncated, otherwise nil.
	TruncatedReason *string `json:"truncated_reason"`
	FilesScanned    int     `json:"files_scanned"`
	BytesScanned    int     `json:"bytes_scanned"`
}

// MemoryFileDeleteResult is the result of MemoryFiles.Delete.
type MemoryFileDeleteResult struct {
	EntityID  string  `json:"entity_id"`
	Partition *string `json:"partition"`
	Name      string  `json:"name"`
	FileID    string  `json:"file_id"`
	// Version is the version that was current when the file was deleted.
	Version int `json:"version"`
	// Hard is true for a hard delete (WithHardDelete).
	Hard bool `json:"hard"`
	// VersionsDeleted is 1 for a soft delete, or the number of versions
	// purged by a hard delete.
	VersionsDeleted int `json:"versions_deleted"`
}

// ── Options ───────────────────────────────────────────────────────────────

type memoryFileListConfig struct {
	prefix       string
	limit        *int
	cursor       string
	entityID     string
	entityPrefix string
}

// MemoryFileListOption configures MemoryFiles.List and Client.ListMemoryFiles.
type MemoryFileListOption func(*memoryFileListConfig)

// WithFilePrefix lists only files whose name starts with prefix
// (case-sensitive).
func WithFilePrefix(prefix string) MemoryFileListOption {
	return func(c *memoryFileListConfig) { c.prefix = prefix }
}

// WithFileLimit sets the page size, 1-1000 (server default 100).
func WithFileLimit(limit int) MemoryFileListOption {
	return func(c *memoryFileListConfig) { c.limit = &limit }
}

// WithFileCursor fetches the page after a previous page's NextCursor.
func WithFileCursor(cursor string) MemoryFileListOption {
	return func(c *memoryFileListConfig) { c.cursor = cursor }
}

// WithFileEntityID lists only this entity's files. Valid only on
// Client.ListMemoryFiles; a MemoryFiles facade is already scoped to its
// entity.
func WithFileEntityID(entityID string) MemoryFileListOption {
	return func(c *memoryFileListConfig) { c.entityID = entityID }
}

// WithFileEntityPrefix lists the files of every entity whose id starts with
// prefix (case-sensitive), e.g. "acme:customer:". Valid only on
// Client.ListMemoryFiles, and not together with WithFileEntityID.
func WithFileEntityPrefix(prefix string) MemoryFileListOption {
	return func(c *memoryFileListConfig) { c.entityPrefix = prefix }
}

type memoryFileReadConfig struct {
	version  *int
	section  *string
	outline  bool
	offset   *int
	maxBytes *int
}

// MemoryFileReadOption configures MemoryFiles.Read.
type MemoryFileReadOption func(*memoryFileReadConfig)

// WithFileVersion reads a retained older version instead of the current one.
func WithFileVersion(version int) MemoryFileReadOption {
	return func(c *memoryFileReadConfig) { c.version = &version }
}

// WithFileSection narrows the read to one section (its heading line included),
// selected by heading text (case- and whitespace-insensitive) or exact slug.
func WithFileSection(section string) MemoryFileReadOption {
	return func(c *memoryFileReadConfig) { c.section = &section }
}

// WithFileOutline returns only the outline (Sections), with Content "". It
// cannot be combined with WithFileSection, WithFileOffset or WithFileMaxBytes.
func WithFileOutline() MemoryFileReadOption {
	return func(c *memoryFileReadConfig) { c.outline = true }
}

// WithFileOffset starts the page at this byte offset — a previous page's
// NextOffset.
func WithFileOffset(offset int) MemoryFileReadOption {
	return func(c *memoryFileReadConfig) { c.offset = &offset }
}

// WithFileMaxBytes sets the page budget in bytes, 1-524288 (default: the whole
// file). Pages end after the last newline that fits.
func WithFileMaxBytes(maxBytes int) MemoryFileReadOption {
	return func(c *memoryFileReadConfig) { c.maxBytes = &maxBytes }
}

// Option names, as reported when an option is passed to a method that does
// not accept it.
const (
	optExpectedVersion = "0.7.0"
	optCreateOnly      = "WithCreateOnly"
	optAuthor          = "WithAuthor"
	optLevel           = "WithLevel"
	optCreateIfMissing = "WithCreateIfMissing"
	optIdempotencyKey  = "WithIdempotencyKey"
)

type memoryFileWriteConfig struct {
	expectedVersion *int
	create          bool
	author          string
	level           int
	createIfMissing bool
	idempotencyKey  string
	// applied lists the options given, to reject ones the method doesn't take.
	applied []string
}

// MemoryFileWriteOption configures MemoryFiles.Write, Append, AppendSection and
// ReplaceSection. Each method accepts only the options that apply to it and
// returns an error, before any request, for any other:
//
//   - Write: WithExpectedVersion or WithCreateOnly (exactly one), WithAuthor,
//     WithIdempotencyKey.
//   - Append: WithExpectedVersion, WithCreateIfMissing, WithAuthor,
//     WithIdempotencyKey.
//   - AppendSection: Append's options plus WithLevel.
//   - ReplaceSection: WithExpectedVersion or WithCreateOnly (exactly one),
//     WithLevel, WithCreateIfMissing, WithAuthor, WithIdempotencyKey.
type MemoryFileWriteOption interface {
	applyWrite(*memoryFileWriteConfig)
}

type memoryFileDeleteConfig struct {
	expectedVersion *int
	hard            bool
}

// MemoryFileDeleteOption configures MemoryFiles.Delete: WithExpectedVersion and
// WithHardDelete.
type MemoryFileDeleteOption interface {
	applyDelete(*memoryFileDeleteConfig)
}

// ExpectedVersionOption is the version precondition returned by
// WithExpectedVersion. It is both a MemoryFileWriteOption and a
// MemoryFileDeleteOption.
type ExpectedVersionOption struct {
	version int
}

// WithExpectedVersion makes a write or delete conditional on the file's current
// version being version (≥ 1), sent as If-Match: "<version>". If another write
// got there first the call fails with *VersionConflictError; re-read the file
// and retry. Write and ReplaceSection need it (or WithCreateOnly); Append,
// AppendSection and Delete take it optionally.
func WithExpectedVersion(version int) ExpectedVersionOption {
	return ExpectedVersionOption{version: version}
}

func (o ExpectedVersionOption) applyWrite(c *memoryFileWriteConfig) {
	v := o.version
	c.expectedVersion = &v
	c.applied = append(c.applied, optExpectedVersion)
}

func (o ExpectedVersionOption) applyDelete(c *memoryFileDeleteConfig) {
	v := o.version
	c.expectedVersion = &v
}

// writeOption is a named MemoryFileWriteOption.
type writeOption struct {
	name  string
	apply func(*memoryFileWriteConfig)
}

func (o writeOption) applyWrite(c *memoryFileWriteConfig) {
	o.apply(c)
	c.applied = append(c.applied, o.name)
}

// WithCreateOnly makes Write or ReplaceSection create the file, failing with
// *VersionConflictError if it already exists (sent as If-None-Match: *).
func WithCreateOnly() MemoryFileWriteOption {
	return writeOption{optCreateOnly, func(c *memoryFileWriteConfig) { c.create = true }}
}

// WithAuthor attaches a free-text attribution label (1-128 characters, e.g. the
// end user an agent acts for) to the version this write produces. It is stored
// next to the authenticated principal and never used for authorization. An
// empty author is not sent.
func WithAuthor(author string) MemoryFileWriteOption {
	return writeOption{optAuthor, func(c *memoryFileWriteConfig) { c.author = author }}
}

// WithLevel sets the heading level (1-6, default 2) used when AppendSection or
// ReplaceSection creates a missing section.
func WithLevel(level int) MemoryFileWriteOption {
	return writeOption{optLevel, func(c *memoryFileWriteConfig) { c.level = level }}
}

// WithCreateIfMissing controls whether Append, AppendSection and ReplaceSection
// create a missing file and a missing section (default true). With false, a
// missing file or section fails with a 404.
func WithCreateIfMissing(create bool) MemoryFileWriteOption {
	return writeOption{optCreateIfMissing, func(c *memoryFileWriteConfig) { c.createIfMissing = create }}
}

// WithIdempotencyKey sets the write's Idempotency-Key (1-256 printable ASCII
// characters). Without it, each call mints its own key; either way the key is
// reused across the client's automatic retries, so a retried write is applied
// at most once. Reusing a key with a different request is a 409 with code
// CodeIdempotencyKeyReused.
func WithIdempotencyKey(key string) MemoryFileWriteOption {
	return writeOption{optIdempotencyKey, func(c *memoryFileWriteConfig) { c.idempotencyKey = key }}
}

type hardDeleteOption struct{}

func (hardDeleteOption) applyDelete(c *memoryFileDeleteConfig) { c.hard = true }

// WithHardDelete makes Delete irreversibly purge every version of the file.
// Afterwards the name starts fresh at version 1. Use it for erasure.
func WithHardDelete() MemoryFileDeleteOption {
	return hardDeleteOption{}
}

type memoryFileVersionsConfig struct {
	limit *int
}

// MemoryFileVersionsOption configures MemoryFiles.Versions.
type MemoryFileVersionsOption func(*memoryFileVersionsConfig)

// WithVersionsLimit caps the number of versions returned, 1-100 (default 100).
func WithVersionsLimit(limit int) MemoryFileVersionsOption {
	return func(c *memoryFileVersionsConfig) { c.limit = &limit }
}

type memoryFileDiffConfig struct {
	from    *int
	to      *int
	context int
}

// MemoryFileDiffOption configures MemoryFiles.Diff.
type MemoryFileDiffOption func(*memoryFileDiffConfig)

// WithFromVersion sets the old side of the diff (≥ 0; 0 is the empty file, so
// WithFromVersion(0) shows a whole version as added). Default: the version
// before the new side.
func WithFromVersion(version int) MemoryFileDiffOption {
	return func(c *memoryFileDiffConfig) { c.from = &version }
}

// WithToVersion sets the new side of the diff (≥ 1). Default: the current
// version.
func WithToVersion(version int) MemoryFileDiffOption {
	return func(c *memoryFileDiffConfig) { c.to = &version }
}

// WithDiffContext sets the number of unchanged context lines, 0-20 (default 3).
func WithDiffContext(lines int) MemoryFileDiffOption {
	return func(c *memoryFileDiffConfig) { c.context = lines }
}

type grepConfig struct {
	regex        bool
	ignoreCase   bool
	files        []string
	filesSet     bool
	context      int
	maxMatches   int
	entityID     string
	entityPrefix string
}

// GrepOption configures MemoryFiles.Grep and Client.GrepMemoryFiles.
type GrepOption func(*grepConfig)

// WithRegex treats the pattern as a regular expression (linear-time syntax:
// no back-references or look-around) instead of a literal substring.
func WithRegex() GrepOption {
	return func(c *grepConfig) { c.regex = true }
}

// WithIgnoreCase matches case-insensitively.
func WithIgnoreCase() GrepOption {
	return func(c *grepConfig) { c.ignoreCase = true }
}

// WithGrepFiles restricts the search to these exact file names (at most 100);
// unknown names are skipped.
func WithGrepFiles(names ...string) GrepOption {
	return func(c *grepConfig) {
		c.files = names
		c.filesSet = true
	}
}

// WithGrepContext returns up to lines lines of context before and after each
// match, 0-10 (default 0).
func WithGrepContext(lines int) GrepOption {
	return func(c *grepConfig) { c.context = lines }
}

// WithMaxMatches stops the search after this many matches, 1-1000 (default
// 100).
func WithMaxMatches(n int) GrepOption {
	return func(c *grepConfig) { c.maxMatches = n }
}

// WithGrepEntityID searches only this entity's files. Valid only on
// Client.GrepMemoryFiles; a MemoryFiles facade is already scoped to its
// entity.
func WithGrepEntityID(entityID string) GrepOption {
	return func(c *grepConfig) { c.entityID = entityID }
}

// WithGrepEntityPrefix searches the files of every entity whose id starts with
// prefix (case-sensitive). Valid only on Client.GrepMemoryFiles, and not
// together with WithGrepEntityID.
func WithGrepEntityPrefix(prefix string) GrepOption {
	return func(c *grepConfig) { c.entityPrefix = prefix }
}

// ── Wire shapes ───────────────────────────────────────────────────────────

// memoryFilePatchRequest is the JSON body of PATCH /memory/files/{name}.
// Section and Level are omitted for "append".
type memoryFilePatchRequest struct {
	Op              string  `json:"op"`
	Section         *string `json:"section,omitempty"`
	Content         string  `json:"content"`
	Level           *int    `json:"level,omitempty"`
	CreateIfMissing bool    `json:"create_if_missing"`
}

// memoryFileGrepRequest is the JSON body of POST /memory/files/grep. Files is
// sent only when given (an explicit empty list included).
type memoryFileGrepRequest struct {
	EntityID     string    `json:"entity_id,omitempty"`
	EntityPrefix string    `json:"entity_prefix,omitempty"`
	Pattern      string    `json:"pattern"`
	Regex        bool      `json:"regex"`
	IgnoreCase   bool      `json:"ignore_case"`
	Files        *[]string `json:"files,omitempty"`
	Context      int       `json:"context"`
	MaxMatches   int       `json:"max_matches"`
}

// ── Raw client: cross-entity list and grep ────────────────────────────────

// ListMemoryFiles lists the current version of live memory files across
// entities, one page at a time: every file in scope, one entity's
// (WithFileEntityID), or every entity under an id prefix
// (WithFileEntityPrefix), optionally narrowed by name (WithFilePrefix). Pass
// NextCursor back through WithFileCursor for the next page. A partition handle
// scopes the listing to its partition; a multi-tenant key requires one.
func (c *Client) ListMemoryFiles(ctx context.Context, opts ...MemoryFileListOption) (*MemoryFilePage, error) {
	var cfg memoryFileListConfig
	for _, o := range opts {
		o(&cfg)
	}
	return c.listMemoryFiles(ctx, cfg)
}

func (c *Client) listMemoryFiles(ctx context.Context, cfg memoryFileListConfig) (*MemoryFilePage, error) {
	if cfg.entityID != "" && cfg.entityPrefix != "" {
		return nil, fmt.Errorf("aether: entity_id and entity_prefix are mutually exclusive")
	}
	params := url.Values{}
	if cfg.entityID != "" {
		params.Set("entity_id", cfg.entityID)
	}
	if cfg.entityPrefix != "" {
		params.Set("entity_prefix", cfg.entityPrefix)
	}
	if cfg.prefix != "" {
		params.Set("prefix", cfg.prefix)
	}
	if cfg.limit != nil {
		params.Set("limit", strconv.Itoa(*cfg.limit))
	}
	if cfg.cursor != "" {
		params.Set("cursor", cfg.cursor)
	}
	c.applyPartitionParam(params)
	path := "/memory/files"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var page MemoryFilePage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &page); err != nil {
		return nil, err
	}
	page.Files = emptyIfNil(page.Files)
	return &page, nil
}

// GrepMemoryFiles searches the current version of live memory files line by
// line, across entities: every file in scope, one entity's (WithGrepEntityID),
// or every entity under an id prefix (WithGrepEntityPrefix). The pattern is a
// literal substring unless WithRegex is given. A line matches at most once.
// The search stops at WithMaxMatches (default 100) or a server scan limit and
// then reports Truncated.
func (c *Client) GrepMemoryFiles(ctx context.Context, pattern string, opts ...GrepOption) (*MemoryGrepResult, error) {
	cfg := grepConfig{maxMatches: 100}
	for _, o := range opts {
		o(&cfg)
	}
	return c.grepMemoryFiles(ctx, pattern, cfg)
}

func (c *Client) grepMemoryFiles(ctx context.Context, pattern string, cfg grepConfig) (*MemoryGrepResult, error) {
	if pattern == "" {
		return nil, fmt.Errorf("aether: pattern cannot be empty")
	}
	if cfg.entityID != "" && cfg.entityPrefix != "" {
		return nil, fmt.Errorf("aether: entity_id and entity_prefix are mutually exclusive")
	}
	body := memoryFileGrepRequest{
		EntityID:     cfg.entityID,
		EntityPrefix: cfg.entityPrefix,
		Pattern:      pattern,
		Regex:        cfg.regex,
		IgnoreCase:   cfg.ignoreCase,
		Context:      cfg.context,
		MaxMatches:   cfg.maxMatches,
	}
	if cfg.filesSet {
		files := emptyIfNil(cfg.files)
		body.Files = &files
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var out MemoryGrepResult
	if err := c.doJSON(ctx, http.MethodPost, c.appendPartitionParam("/memory/files/grep"), bytes.NewReader(payload), &out); err != nil {
		return nil, err
	}
	out.Matches = emptyIfNil(out.Matches)
	for i := range out.Matches {
		out.Matches[i].Before = emptyIfNil(out.Matches[i].Before)
		out.Matches[i].After = emptyIfNil(out.Matches[i].After)
	}
	return &out, nil
}

// ── Memory.Files facade ───────────────────────────────────────────────────

// MemoryFiles is the entity-scoped memory-file facade returned by
// Memory.Files: named, versioned markdown files (e.g. "memory.md") that belong
// to the Memory's entity, within the client's partition when it is scoped.
//
// Read a file whole, by section or by page; replace it with a version
// precondition (Write) or edit it section by section (Append, AppendSection,
// ReplaceSection); list its versions, diff them, grep across files, and delete
// it. Every write produces a new version and keeps the previous ones.
//
// The read-modify-write recipe: Read, edit the content, then Write with
// WithExpectedVersion(file.Version); on *VersionConflictError, read again and
// retry. Append and AppendSection need no precondition: they apply atomically
// to whatever the current version is.
//
// Every write sends an Idempotency-Key that is reused across the client's
// automatic retries, so a retried write is never applied twice. A 404 with
// code CodeFeatureNotEnabled means memory files are not enabled on your
// deployment.
type MemoryFiles struct {
	memory *Memory
}

// Files returns the memory-file facade scoped to this Memory's entity.
func (m *Memory) Files() *MemoryFiles {
	return &MemoryFiles{memory: m}
}

// send executes a per-file request (/memory/files/{name}{suffix}), injecting
// entity_id and the client's partition, through the client's retrying
// transport. It returns the response headers for the ETag.
func (f *MemoryFiles) send(
	ctx context.Context,
	method, name, suffix string,
	params url.Values,
	contentType string,
	body []byte,
	idempotencyKey string,
	headers http.Header,
	result any,
) (http.Header, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("entity_id", f.memory.entityID)
	f.memory.client.applyPartitionParam(params)
	path := "/memory/files/" + url.PathEscape(name) + suffix + "?" + params.Encode()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	return f.memory.client.doBodyWithHeaders(ctx, method, path, contentType, reader, result, idempotencyKey, headers)
}

// List lists this entity's files (current versions), optionally narrowed by
// name with WithFilePrefix and paged with WithFileLimit / WithFileCursor.
// WithFileEntityID and WithFileEntityPrefix are rejected: the entity is fixed.
func (f *MemoryFiles) List(ctx context.Context, opts ...MemoryFileListOption) (*MemoryFilePage, error) {
	var cfg memoryFileListConfig
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.entityID != "" {
		return nil, fmt.Errorf("aether: WithFileEntityID is not valid for MemoryFiles.List")
	}
	if cfg.entityPrefix != "" {
		return nil, fmt.Errorf("aether: WithFileEntityPrefix is not valid for MemoryFiles.List")
	}
	cfg.entityID = f.memory.entityID
	return f.memory.client.listMemoryFiles(ctx, cfg)
}

// Read reads a file: by default its current version, whole. WithFileVersion
// reads a retained older version, WithFileSection narrows to one section,
// WithFileOutline returns only the outline, and WithFileOffset /
// WithFileMaxBytes page through it (pass NextOffset back while Truncated).
// Sections is always the version's full outline.
func (f *MemoryFiles) Read(ctx context.Context, name string, opts ...MemoryFileReadOption) (*MemoryFile, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	var cfg memoryFileReadConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{}
	if cfg.version != nil {
		params.Set("version", strconv.Itoa(*cfg.version))
	}
	if cfg.section != nil {
		params.Set("section", *cfg.section)
	}
	if cfg.outline {
		params.Set("outline", "true")
	}
	if cfg.offset != nil {
		params.Set("offset", strconv.Itoa(*cfg.offset))
	}
	if cfg.maxBytes != nil {
		params.Set("max_bytes", strconv.Itoa(*cfg.maxBytes))
	}
	var out MemoryFile
	headers, err := f.send(ctx, http.MethodGet, name, "", params, "", nil, "", nil, &out)
	if err != nil {
		return nil, err
	}
	out.Version = versionOrETag(out.Version, headers)
	out.Sections = emptyIfNil(out.Sections)
	return &out, nil
}

// writeConfig applies opts, rejects any option method does not accept, and
// checks the expected version.
func writeConfig(method string, opts []MemoryFileWriteOption, accepted ...string) (memoryFileWriteConfig, error) {
	cfg := memoryFileWriteConfig{level: 2, createIfMissing: true}
	for _, o := range opts {
		if o != nil {
			o.applyWrite(&cfg)
		}
	}
	for _, name := range cfg.applied {
		ok := false
		for _, a := range accepted {
			if name == a {
				ok = true
				break
			}
		}
		if !ok {
			return cfg, fmt.Errorf("aether: %s is not valid for %s", name, method)
		}
	}
	if err := checkExpectedVersion(cfg.expectedVersion); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func checkExpectedVersion(version *int) error {
	if version != nil && *version < 1 {
		return fmt.Errorf("aether: expected version must be >= 1, got %d", *version)
	}
	return nil
}

// requireOnePrecondition enforces exactly one of WithExpectedVersion and
// WithCreateOnly.
func requireOnePrecondition(cfg memoryFileWriteConfig) error {
	if (cfg.expectedVersion != nil) == cfg.create {
		return fmt.Errorf("aether: pass exactly one of WithExpectedVersion or WithCreateOnly")
	}
	return nil
}

// writeHeaders builds the precondition headers of a write.
func writeHeaders(cfg memoryFileWriteConfig) http.Header {
	headers := http.Header{}
	if cfg.expectedVersion != nil {
		headers.Set("If-Match", quoteETag(strconv.Itoa(*cfg.expectedVersion)))
	}
	if cfg.create {
		headers.Set("If-None-Match", "*")
	}
	return headers
}

// write sends a PUT or PATCH and parses the MemoryFileWriteResult. One
// Idempotency-Key per call is reused across transport retries.
func (f *MemoryFiles) write(
	ctx context.Context,
	method, name, contentType string,
	body []byte,
	cfg memoryFileWriteConfig,
) (*MemoryFileWriteResult, error) {
	params := url.Values{}
	if cfg.author != "" {
		params.Set("author", cfg.author)
	}
	key := cfg.idempotencyKey
	if key == "" {
		key = newIdempotencyKey()
	}
	var out MemoryFileWriteResult
	headers, err := f.send(ctx, method, name, "", params, contentType, body, key, writeHeaders(cfg), &out)
	if err != nil {
		return nil, err
	}
	out.Version = versionOrETag(out.Version, headers)
	out.Sections = emptyIfNil(out.Sections)
	return &out, nil
}

// patch sends a section-aware PATCH.
func (f *MemoryFiles) patch(ctx context.Context, name string, req memoryFilePatchRequest, cfg memoryFileWriteConfig) (*MemoryFileWriteResult, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	return f.write(ctx, http.MethodPatch, name, "application/json", payload, cfg)
}

// Write replaces the whole file with content (1-524288 bytes of UTF-8
// markdown). It needs exactly one precondition: WithExpectedVersion(v) to
// replace version v, or WithCreateOnly to create the file. Writing content
// identical to the current version writes nothing and returns Unchanged. A
// stale version fails with *VersionConflictError.
func (f *MemoryFiles) Write(ctx context.Context, name, content string, opts ...MemoryFileWriteOption) (*MemoryFileWriteResult, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	cfg, err := writeConfig("MemoryFiles.Write", opts,
		optExpectedVersion, optCreateOnly, optAuthor, optIdempotencyKey)
	if err != nil {
		return nil, err
	}
	if err := requireOnePrecondition(cfg); err != nil {
		return nil, err
	}
	return f.write(ctx, http.MethodPut, name, "text/markdown; charset=utf-8", []byte(content), cfg)
}

// Append adds content to the end of the file (a newline is added before it if
// the file does not end with one, and after it if content does not). It needs
// no precondition: it applies atomically to the current version.
// WithExpectedVersion makes it conditional anyway. A missing file is created
// unless WithCreateIfMissing(false). content must be non-empty.
func (f *MemoryFiles) Append(ctx context.Context, name, content string, opts ...MemoryFileWriteOption) (*MemoryFileWriteResult, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	cfg, err := writeConfig("MemoryFiles.Append", opts,
		optExpectedVersion, optCreateIfMissing, optAuthor, optIdempotencyKey)
	if err != nil {
		return nil, err
	}
	if content == "" {
		return nil, fmt.Errorf("aether: content cannot be empty")
	}
	return f.patch(ctx, name, memoryFilePatchRequest{
		Op:              "append",
		Content:         content,
		CreateIfMissing: cfg.createIfMissing,
	}, cfg)
}

// AppendSection adds content to the end of a section's own text (after its last
// non-blank line, ahead of any sub-sections), selected by heading text (case-
// and whitespace-insensitive) or exact slug. A missing section is created at
// the end of the file as a heading of WithLevel (default 2), and a missing file
// is created, unless WithCreateIfMissing(false). Like Append it needs no
// precondition. content must be non-empty and section non-blank.
func (f *MemoryFiles) AppendSection(ctx context.Context, name, section, content string, opts ...MemoryFileWriteOption) (*MemoryFileWriteResult, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	cfg, err := writeConfig("MemoryFiles.AppendSection", opts,
		optExpectedVersion, optLevel, optCreateIfMissing, optAuthor, optIdempotencyKey)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(section) == "" {
		return nil, fmt.Errorf("aether: section cannot be blank")
	}
	if content == "" {
		return nil, fmt.Errorf("aether: content cannot be empty")
	}
	level := cfg.level
	return f.patch(ctx, name, memoryFilePatchRequest{
		Op:              "append_section",
		Section:         &section,
		Content:         content,
		Level:           &level,
		CreateIfMissing: cfg.createIfMissing,
	}, cfg)
}

// ReplaceSection replaces a section's body (everything under its heading,
// nested sub-sections included) with content; the heading is kept, and ""
// clears the body. It needs exactly one precondition: WithExpectedVersion(v)
// or WithCreateOnly. A missing section is created like AppendSection's unless
// WithCreateIfMissing(false).
func (f *MemoryFiles) ReplaceSection(ctx context.Context, name, section, content string, opts ...MemoryFileWriteOption) (*MemoryFileWriteResult, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	cfg, err := writeConfig("MemoryFiles.ReplaceSection", opts,
		optExpectedVersion, optCreateOnly, optLevel, optCreateIfMissing, optAuthor, optIdempotencyKey)
	if err != nil {
		return nil, err
	}
	if err := requireOnePrecondition(cfg); err != nil {
		return nil, err
	}
	if strings.TrimSpace(section) == "" {
		return nil, fmt.Errorf("aether: section cannot be blank")
	}
	level := cfg.level
	return f.patch(ctx, name, memoryFilePatchRequest{
		Op:              "replace_section",
		Section:         &section,
		Content:         content,
		Level:           &level,
		CreateIfMissing: cfg.createIfMissing,
	}, cfg)
}

// Versions lists the file's retained versions, newest first (the most recent
// 100 are kept).
func (f *MemoryFiles) Versions(ctx context.Context, name string, opts ...MemoryFileVersionsOption) (*MemoryFileVersions, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	var cfg memoryFileVersionsConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{}
	if cfg.limit != nil {
		params.Set("limit", strconv.Itoa(*cfg.limit))
	}
	var out MemoryFileVersions
	if _, err := f.send(ctx, http.MethodGet, name, "/versions", params, "", nil, "", nil, &out); err != nil {
		return nil, err
	}
	out.Versions = emptyIfNil(out.Versions)
	return &out, nil
}

// Diff returns a unified diff between two versions of the file: by default the
// current version against the one before it. WithFromVersion(0) diffs against
// the empty file.
func (f *MemoryFiles) Diff(ctx context.Context, name string, opts ...MemoryFileDiffOption) (*MemoryFileDiff, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	cfg := memoryFileDiffConfig{context: 3}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.from != nil && *cfg.from < 0 {
		return nil, fmt.Errorf("aether: from version must be >= 0, got %d", *cfg.from)
	}
	if cfg.to != nil && *cfg.to < 1 {
		return nil, fmt.Errorf("aether: to version must be >= 1, got %d", *cfg.to)
	}
	params := url.Values{}
	if cfg.from != nil {
		params.Set("from", strconv.Itoa(*cfg.from))
	}
	if cfg.to != nil {
		params.Set("to", strconv.Itoa(*cfg.to))
	}
	params.Set("context", strconv.Itoa(cfg.context))
	var out MemoryFileDiff
	if _, err := f.send(ctx, http.MethodGet, name, "/diff", params, "", nil, "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Grep searches this entity's files line by line (current versions only). The
// pattern is a literal substring unless WithRegex is given; WithGrepFiles
// restricts the search to named files. WithGrepEntityID and
// WithGrepEntityPrefix are rejected: the entity is fixed.
func (f *MemoryFiles) Grep(ctx context.Context, pattern string, opts ...GrepOption) (*MemoryGrepResult, error) {
	cfg := grepConfig{maxMatches: 100}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.entityID != "" {
		return nil, fmt.Errorf("aether: WithGrepEntityID is not valid for MemoryFiles.Grep")
	}
	if cfg.entityPrefix != "" {
		return nil, fmt.Errorf("aether: WithGrepEntityPrefix is not valid for MemoryFiles.Grep")
	}
	cfg.entityID = f.memory.entityID
	return f.memory.client.grepMemoryFiles(ctx, pattern, cfg)
}

// Delete deletes the file. By default it is a soft delete: the file disappears
// from reads, lists, grep and search, and writing the name again continues its
// version numbering. WithHardDelete irreversibly purges every version.
// WithExpectedVersion makes the delete conditional (*VersionConflictError on
// a mismatch). A retried delete may observe a 404.
func (f *MemoryFiles) Delete(ctx context.Context, name string, opts ...MemoryFileDeleteOption) (*MemoryFileDeleteResult, error) {
	if err := ValidateMemoryFileName(name); err != nil {
		return nil, err
	}
	var cfg memoryFileDeleteConfig
	for _, o := range opts {
		if o != nil {
			o.applyDelete(&cfg)
		}
	}
	if err := checkExpectedVersion(cfg.expectedVersion); err != nil {
		return nil, err
	}
	params := url.Values{}
	if cfg.hard {
		params.Set("hard", "true")
	}
	var headers http.Header
	if cfg.expectedVersion != nil {
		headers = http.Header{}
		headers.Set("If-Match", quoteETag(strconv.Itoa(*cfg.expectedVersion)))
	}
	var out MemoryFileDeleteResult
	if _, err := f.send(ctx, http.MethodDelete, name, "", params, "", nil, "", headers, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
