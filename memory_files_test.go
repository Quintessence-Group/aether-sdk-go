package aether

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Shared fixtures: the same owner, partition handle and golden outline are used
// by every SDK's memory-file suite.
const (
	fileOwner     = "scoped:ws_42:company:c_7"
	fileHandle    = "scoped-ws_42"
	agreedContent = "## Agreed terms\n- 2026-09-01 — Net 30 payment terms (alice, ISS-12)\n\n"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func vectorAOutline() []any {
	return []any{
		map[string]any{"heading": "Acme memory", "level": 1, "slug": "acme-memory", "start_line": 1, "end_line": 9, "bytes": 122},
		map[string]any{"heading": "Agreed terms", "level": 2, "slug": "agreed-terms", "start_line": 4, "end_line": 6, "bytes": 71},
		map[string]any{"heading": "Exceptions", "level": 2, "slug": "exceptions", "start_line": 7, "end_line": 8, "bytes": 15},
		map[string]any{"heading": "Notes", "level": 2, "slug": "notes", "start_line": 9, "end_line": 9, "bytes": 9},
	}
}

var vectorASections = []MemoryFileSection{
	{Heading: "Acme memory", Level: 1, Slug: "acme-memory", StartLine: 1, EndLine: 9, Bytes: 122},
	{Heading: "Agreed terms", Level: 2, Slug: "agreed-terms", StartLine: 4, EndLine: 6, Bytes: 71},
	{Heading: "Exceptions", Level: 2, Slug: "exceptions", StartLine: 7, EndLine: 8, Bytes: 15},
	{Heading: "Notes", Level: 2, Slug: "notes", StartLine: 9, EndLine: 9, Bytes: 9},
}

func withOverrides(base, overrides map[string]any) map[string]any {
	for k, v := range overrides {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	return base
}

// fileInfoBody is a MemoryFileInfo response; an override of nil removes a key.
func fileInfoBody(overrides map[string]any) map[string]any {
	return withOverrides(map[string]any{
		"entity_id": fileOwner, "partition": fileHandle, "name": "memory.md",
		"file_id": "3d1f", "doc_id": "a8c2", "version": 7, "bytes": 122,
		"content_hash": "hash:9f", "author": "scoped:ws_42:user:u_3", "principal": "key:ak_1a2b3c4d",
		"op": "append_section", "created_at": "2026-09-01T10:00:00Z", "updated_at": "2026-10-10T09:12:44Z",
	}, overrides)
}

// readBody is the example read of section "Agreed terms" at version 7.
func readBody(overrides map[string]any) map[string]any {
	body := fileInfoBody(map[string]any{
		"is_current": true, "content": agreedContent,
		"section": map[string]any{
			"heading": "Agreed terms", "level": 2, "slug": "agreed-terms",
			"start_line": 4, "end_line": 6, "bytes": 71,
		},
		"offset": 0, "next_offset": json.RawMessage("null"), "total_bytes": 71, "truncated": false,
		"sections": vectorAOutline(),
	})
	return withOverrides(body, overrides)
}

func writeResultBody(overrides map[string]any) map[string]any {
	body := fileInfoBody(map[string]any{
		"version": 8, "unchanged": false, "previous_version": 7, "file_created": false,
		"section": json.RawMessage("null"), "section_created": false, "sections": vectorAOutline(),
	})
	return withOverrides(body, overrides)
}

type fileRequest struct {
	Method string
	Path   string
	Query  url.Values
	Header http.Header
	Body   []byte
}

// jsonBody decodes the request body as a JSON object.
func (r fileRequest) jsonBody(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("request body is not JSON: %v (%q)", err, r.Body)
	}
	return body
}

type fileReply struct {
	status  int
	headers map[string]string
	body    any
}

// fileServer records every request and answers with replies in order, the
// last one repeating. With no replies it answers 200 {} with ETag "7".
type fileServer struct {
	mu      sync.Mutex
	reqs    []fileRequest
	replies []fileReply
}

func (s *fileServer) requests() []fileRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]fileRequest(nil), s.reqs...)
}

func (s *fileServer) last(t *testing.T) fileRequest {
	t.Helper()
	reqs := s.requests()
	if len(reqs) == 0 {
		t.Fatal("no request was sent")
	}
	return reqs[len(reqs)-1]
}

func (s *fileServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.reqs = append(s.reqs, fileRequest{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(), Body: body,
	})
	reply := fileReply{status: http.StatusOK, headers: map[string]string{"ETag": `"7"`}, body: map[string]any{}}
	if n := len(s.replies); n > 0 {
		idx := len(s.reqs) - 1
		if idx >= n {
			idx = n - 1
		}
		reply = s.replies[idx]
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	for k, v := range reply.headers {
		w.Header().Set(k, v)
	}
	status := reply.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	switch b := reply.body.(type) {
	case nil:
		_, _ = w.Write([]byte("{}"))
	case string:
		_, _ = w.Write([]byte(b))
	default:
		_ = json.NewEncoder(w).Encode(b)
	}
}

// newFileServer starts a fake engine and a client that retries once with a
// tiny backoff.
func newFileServer(t *testing.T, replies ...fileReply) (*fileServer, *Client) {
	t.Helper()
	s := &fileServer{replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	return s, NewClient(srv.URL, WithMaxRetries(1), WithRetryBackoff(time.Millisecond))
}

// newScopedFiles returns the owner's facade through the partition handle.
func newScopedFiles(t *testing.T, replies ...fileReply) (*fileServer, *MemoryFiles) {
	t.Helper()
	s, client := newFileServer(t, replies...)
	scoped, err := client.Partition(fileHandle)
	if err != nil {
		t.Fatal(err)
	}
	mem, err := NewMemoryWithClient(fileOwner, scoped)
	if err != nil {
		t.Fatal(err)
	}
	return s, mem.Files()
}

func okReply(body any) fileReply { return fileReply{status: http.StatusOK, body: body} }

func errorReply(status int, code string, extra map[string]any) fileReply {
	body := map[string]any{"error": "rejected", "code": code, "request_id": "req-1"}
	for k, v := range extra {
		body[k] = v
	}
	return fileReply{status: status, body: body}
}

func expectNoRequests(t *testing.T, s *fileServer) {
	t.Helper()
	if n := len(s.requests()); n != 0 {
		t.Fatalf("expected no HTTP request, got %d", n)
	}
}

func intPtr(v int) *int { return &v }

// ── Names ─────────────────────────────────────────────────────────────────

var name128 = strings.Repeat("abcdefghijklmnopqrstuvwxyz", 4) + "abcdefghijklmnopqrstuvwx"

func TestMemoryFileNameGoldens(t *testing.T) {
	if len(name128) != 128 {
		t.Fatalf("fixture must be 128 chars, got %d", len(name128))
	}
	accepted := []string{"memory.md", "a", "A", "0", "a.", "Grep", "grep.md", "notes_2026-10.md", "x.y.z", name128}
	for _, name := range accepted {
		if err := ValidateMemoryFileName(name); err != nil {
			t.Errorf("%q must be accepted: %v", name, err)
		}
	}

	rejected := []string{
		"", ".hidden", "-x", "_x", ".", "..", "a..b", "a...b",
		"a/b", "a b", `a\b`, "memory.md ", "naïve.md", "grep", name128 + "y",
		"memory.md\n",
	}
	s, files := newScopedFiles(t)
	for _, name := range rejected {
		err := ValidateMemoryFileName(name)
		if err == nil || !strings.Contains(err.Error(), "invalid memory file name") {
			t.Errorf("%q must be rejected by the validator, got %v", name, err)
		}
		if _, err := files.Read(context.Background(), name); err == nil {
			t.Errorf("%q must be rejected by Read", name)
		}
		if _, err := files.Write(context.Background(), name, "x", WithCreateOnly()); err == nil {
			t.Errorf("%q must be rejected by Write", name)
		}
		if _, err := files.Delete(context.Background(), name); err == nil {
			t.Errorf("%q must be rejected by Delete", name)
		}
	}
	expectNoRequests(t, s)
}

// ── Scoping ───────────────────────────────────────────────────────────────

func TestMemoryFilesSendOwnerAndPartition(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t)

	if _, err := files.Read(ctx, "memory.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Write(ctx, "memory.md", "# x\n", WithCreateOnly()); err != nil {
		t.Fatal(err)
	}
	if _, err := files.List(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Delete(ctx, "memory.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Grep(ctx, "net"); err != nil {
		t.Fatal(err)
	}

	reqs := s.requests()
	wantPaths := []string{
		"/v1/memory/files/memory.md", "/v1/memory/files/memory.md", "/v1/memory/files",
		"/v1/memory/files/memory.md", "/v1/memory/files/grep",
	}
	for i, req := range reqs {
		if req.Path != wantPaths[i] {
			t.Errorf("request %d path %q, want %q", i, req.Path, wantPaths[i])
		}
		if got := req.Query.Get("partition"); got != fileHandle {
			t.Errorf("request %d partition %q", i, got)
		}
		if i < 4 {
			if got := req.Query.Get("entity_id"); got != fileOwner {
				t.Errorf("request %d entity_id %q", i, got)
			}
		}
	}
	grep := reqs[4]
	if grep.Method != http.MethodPost || grep.Query.Has("entity_id") {
		t.Errorf("grep must POST without an entity_id query param: %+v", grep)
	}
	if got := grep.jsonBody(t)["entity_id"]; got != fileOwner {
		t.Errorf("grep body entity_id %v", got)
	}
}

func TestCrossEntityListAndGrepSendEntityPrefix(t *testing.T) {
	ctx := context.Background()
	s, client := newFileServer(t, okReply(map[string]any{
		"files":       []any{fileInfoBody(nil)},
		"next_cursor": "opaque-2",
	}))
	scoped, _ := client.Partition(fileHandle)

	page, err := scoped.ListMemoryFiles(ctx, WithFileEntityPrefix("scoped:ws_42:company:"))
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodGet || req.Path != "/v1/memory/files" {
		t.Errorf("unexpected list request %s %s", req.Method, req.Path)
	}
	if req.Query.Get("entity_prefix") != "scoped:ws_42:company:" || req.Query.Has("entity_id") {
		t.Errorf("list must send entity_prefix only: %v", req.Query)
	}
	if req.Query.Get("partition") != fileHandle {
		t.Errorf("list partition %v", req.Query)
	}
	if len(page.Files) != 1 || page.Files[0].Name != "memory.md" || page.NextCursor == nil || *page.NextCursor != "opaque-2" {
		t.Errorf("unexpected page %+v", page)
	}

	if _, err := scoped.GrepMemoryFiles(ctx, "net", WithGrepEntityPrefix("scoped:ws_42:company:")); err != nil {
		t.Fatal(err)
	}
	body := s.last(t).jsonBody(t)
	if body["entity_prefix"] != "scoped:ws_42:company:" {
		t.Errorf("grep body entity_prefix %v", body)
	}
	if _, has := body["entity_id"]; has {
		t.Errorf("grep body must not carry entity_id: %v", body)
	}
	if s.last(t).Query.Get("partition") != fileHandle {
		t.Errorf("grep partition missing")
	}

	// Unscoped, neither entity option: no entity keys at all.
	if _, err := client.GrepMemoryFiles(ctx, "net"); err != nil {
		t.Fatal(err)
	}
	body = s.last(t).jsonBody(t)
	if _, has := body["entity_id"]; has {
		t.Errorf("unexpected entity_id: %v", body)
	}
	if _, has := body["entity_prefix"]; has {
		t.Errorf("unexpected entity_prefix: %v", body)
	}
	if s.last(t).Query.Has("partition") {
		t.Errorf("an unscoped client must not send partition")
	}

	sent := len(s.requests())
	if _, err := client.ListMemoryFiles(ctx, WithFileEntityID("a"), WithFileEntityPrefix("b")); err == nil {
		t.Error("list with both entity options must fail")
	}
	if _, err := client.GrepMemoryFiles(ctx, "net", WithGrepEntityID("a"), WithGrepEntityPrefix("b")); err == nil {
		t.Error("grep with both entity options must fail")
	}
	if len(s.requests()) != sent {
		t.Error("no request may be sent for a client-side error")
	}
}

func TestMemoryFilesFacadeRejectsEntityOptions(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t)
	if _, err := files.List(ctx, WithFileEntityID("other")); err == nil {
		t.Error("List must reject WithFileEntityID")
	}
	if _, err := files.List(ctx, WithFileEntityPrefix("scoped:")); err == nil {
		t.Error("List must reject WithFileEntityPrefix")
	}
	if _, err := files.Grep(ctx, "net", WithGrepEntityID("other")); err == nil {
		t.Error("Grep must reject WithGrepEntityID")
	}
	if _, err := files.Grep(ctx, "net", WithGrepEntityPrefix("scoped:")); err == nil {
		t.Error("Grep must reject WithGrepEntityPrefix")
	}
	expectNoRequests(t, s)
}

// ── Preconditions ─────────────────────────────────────────────────────────

func TestMemoryFilesPreconditionHeaders(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t)

	for _, tc := range []struct {
		version int
		want    string
	}{{7, `"7"`}, {1, `"1"`}, {123456, `"123456"`}} {
		if _, err := files.Write(ctx, "memory.md", "x", WithExpectedVersion(tc.version)); err != nil {
			t.Fatal(err)
		}
		h := s.last(t).Header
		if got := h.Get("If-Match"); got != tc.want {
			t.Errorf("expected_version %d: If-Match %q, want %q", tc.version, got, tc.want)
		}
		if _, has := h["If-None-Match"]; has {
			t.Errorf("expected_version %d must not send If-None-Match", tc.version)
		}
	}

	if _, err := files.Write(ctx, "memory.md", "x", WithCreateOnly()); err != nil {
		t.Fatal(err)
	}
	h := s.last(t).Header
	if h.Get("If-None-Match") != "*" {
		t.Errorf("create: If-None-Match %q", h.Get("If-None-Match"))
	}
	if _, has := h["If-Match"]; has {
		t.Error("create must not send If-Match")
	}

	if _, err := files.Append(ctx, "memory.md", "- a"); err != nil {
		t.Fatal(err)
	}
	h = s.last(t).Header
	if _, has := h["If-Match"]; has {
		t.Error("append without expected version must not send If-Match")
	}
	if _, has := h["If-None-Match"]; has {
		t.Error("append must not send If-None-Match")
	}

	if _, err := files.Append(ctx, "memory.md", "- a", WithExpectedVersion(3)); err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).Header.Get("If-Match"); got != `"3"` {
		t.Errorf("append If-Match %q", got)
	}

	if _, err := files.AppendSection(ctx, "memory.md", "Notes", "- a", WithExpectedVersion(5)); err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).Header.Get("If-Match"); got != `"5"` {
		t.Errorf("append_section If-Match %q", got)
	}

	if _, err := files.ReplaceSection(ctx, "memory.md", "Notes", "- a", WithCreateOnly()); err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).Header.Get("If-None-Match"); got != "*" {
		t.Errorf("replace_section create: If-None-Match %q", got)
	}

	if _, err := files.Delete(ctx, "memory.md", WithExpectedVersion(4)); err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).Header.Get("If-Match"); got != `"4"` {
		t.Errorf("delete If-Match %q", got)
	}
	if _, err := files.Delete(ctx, "memory.md"); err != nil {
		t.Fatal(err)
	}
	if _, has := s.last(t).Header["If-Match"]; has {
		t.Error("delete without expected version must not send If-Match")
	}
}

func TestMemoryFilesPreconditionArgumentErrors(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t)
	cases := map[string]func() error{
		"write neither": func() error { _, err := files.Write(ctx, "memory.md", "x"); return err },
		"write both": func() error {
			_, err := files.Write(ctx, "memory.md", "x", WithExpectedVersion(7), WithCreateOnly())
			return err
		},
		"write version 0": func() error {
			_, err := files.Write(ctx, "memory.md", "x", WithExpectedVersion(0))
			return err
		},
		"write version -1": func() error {
			_, err := files.Write(ctx, "memory.md", "x", WithExpectedVersion(-1))
			return err
		},
		"append version 0": func() error {
			_, err := files.Append(ctx, "memory.md", "x", WithExpectedVersion(0))
			return err
		},
		"replace_section neither": func() error {
			_, err := files.ReplaceSection(ctx, "memory.md", "Notes", "x")
			return err
		},
		"replace_section both": func() error {
			_, err := files.ReplaceSection(ctx, "memory.md", "Notes", "x", WithExpectedVersion(2), WithCreateOnly())
			return err
		},
		"delete version 0": func() error {
			_, err := files.Delete(ctx, "memory.md", WithExpectedVersion(0))
			return err
		},
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Errorf("%s: expected a client-side error", name)
		}
	}
	expectNoRequests(t, s)
}

func TestMemoryFilesRejectInapplicableOptions(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t)
	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"Write+WithLevel", func() error {
			_, err := files.Write(ctx, "memory.md", "x", WithCreateOnly(), WithLevel(3))
			return err
		}, "WithLevel is not valid for MemoryFiles.Write"},
		{"Write+WithCreateIfMissing", func() error {
			_, err := files.Write(ctx, "memory.md", "x", WithCreateOnly(), WithCreateIfMissing(false))
			return err
		}, "WithCreateIfMissing is not valid for MemoryFiles.Write"},
		{"Append+WithCreateOnly", func() error {
			_, err := files.Append(ctx, "memory.md", "x", WithCreateOnly())
			return err
		}, "WithCreateOnly is not valid for MemoryFiles.Append"},
		{"Append+WithLevel", func() error {
			_, err := files.Append(ctx, "memory.md", "x", WithLevel(2))
			return err
		}, "WithLevel is not valid for MemoryFiles.Append"},
		{"AppendSection+WithCreateOnly", func() error {
			_, err := files.AppendSection(ctx, "memory.md", "Notes", "x", WithCreateOnly())
			return err
		}, "WithCreateOnly is not valid for MemoryFiles.AppendSection"},
	}
	for _, tc := range cases {
		err := tc.call()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
	expectNoRequests(t, s)

	// ReplaceSection accepts all six write options.
	if _, err := files.ReplaceSection(ctx, "memory.md", "Notes", "x",
		WithExpectedVersion(2), WithLevel(3), WithCreateIfMissing(false),
		WithAuthor("bob"), WithIdempotencyKey("k-1")); err != nil {
		t.Fatalf("ReplaceSection must accept its options: %v", err)
	}
}

// ── Idempotency-Key ───────────────────────────────────────────────────────

func memoryFileWrites(files *MemoryFiles, opts ...MemoryFileWriteOption) map[string]func() error {
	ctx := context.Background()
	return map[string]func() error{
		"write": func() error {
			_, err := files.Write(ctx, "memory.md", "x", append([]MemoryFileWriteOption{WithExpectedVersion(7)}, opts...)...)
			return err
		},
		"append": func() error {
			_, err := files.Append(ctx, "memory.md", "x", opts...)
			return err
		},
		"append_section": func() error {
			_, err := files.AppendSection(ctx, "memory.md", "Notes", "x", opts...)
			return err
		},
		"replace_section": func() error {
			_, err := files.ReplaceSection(ctx, "memory.md", "Notes", "x", append([]MemoryFileWriteOption{WithExpectedVersion(7)}, opts...)...)
			return err
		},
	}
}

func TestMemoryFileWritesSendIdempotencyKey(t *testing.T) {
	s, files := newScopedFiles(t)
	for op, call := range memoryFileWrites(files) {
		if err := call(); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		first := s.last(t).Header.Get("Idempotency-Key")
		if !uuidV4.MatchString(first) {
			t.Errorf("%s: Idempotency-Key %q is not a UUID v4", op, first)
		}
		if err := call(); err != nil {
			t.Fatal(err)
		}
		if second := s.last(t).Header.Get("Idempotency-Key"); second == first {
			t.Errorf("%s: two calls must get different keys", op)
		}
	}

	for op, call := range memoryFileWrites(files, WithIdempotencyKey("turn-42-memory")) {
		if err := call(); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if got := s.last(t).Header.Get("Idempotency-Key"); got != "turn-42-memory" {
			t.Errorf("%s: caller key not sent verbatim: %q", op, got)
		}
	}

	ctx := context.Background()
	if _, err := files.Read(ctx, "memory.md"); err != nil {
		t.Fatal(err)
	}
	if _, has := s.last(t).Header["Idempotency-Key"]; has {
		t.Error("read must not send an Idempotency-Key")
	}
	if _, err := files.Delete(ctx, "memory.md"); err != nil {
		t.Fatal(err)
	}
	if _, has := s.last(t).Header["Idempotency-Key"]; has {
		t.Error("delete must not send an Idempotency-Key")
	}
}

func TestMemoryFileWriteRetryReusesIdempotencyKey(t *testing.T) {
	busy := fileReply{
		status:  http.StatusServiceUnavailable,
		headers: map[string]string{"Retry-After": "0"},
		body:    map[string]any{"error": "file is busy", "code": CodeMemoryFileBusy},
	}
	for _, op := range []string{"write", "append", "append_section", "replace_section"} {
		s, files := newScopedFiles(t, busy, okReply(writeResultBody(nil)))
		if err := memoryFileWrites(files)[op](); err != nil {
			t.Fatalf("%s: the retry must succeed: %v", op, err)
		}
		reqs := s.requests()
		if len(reqs) != 2 {
			t.Fatalf("%s: expected 2 attempts, got %d", op, len(reqs))
		}
		first, second := reqs[0].Header.Get("Idempotency-Key"), reqs[1].Header.Get("Idempotency-Key")
		if first == "" || first != second {
			t.Errorf("%s: retry must reuse the key: %q vs %q", op, first, second)
		}
		if !bytes.Equal(reqs[0].Body, reqs[1].Body) {
			t.Errorf("%s: retry must resend the same body", op)
		}
	}
}

// ── Typed version conflict ────────────────────────────────────────────────

func TestVersionConflictErrorIsTyped(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t, errorReply(http.StatusPreconditionFailed, CodeMemoryFileVersionConflict,
		map[string]any{"current_version": 8}))
	_, err := files.Write(ctx, "memory.md", "x", WithExpectedVersion(7))

	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *VersionConflictError, got %T %v", err, err)
	}
	if conflict.CurrentVersion == nil || *conflict.CurrentVersion != 8 {
		t.Errorf("CurrentVersion = %v, want 8", conflict.CurrentVersion)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 412 || apiErr.ErrorCode != CodeMemoryFileVersionConflict {
		t.Errorf("must also be the base *APIError (412, code): %+v", apiErr)
	}
	if !errors.Is(err, ErrVersionConflict) {
		t.Error("errors.Is(err, ErrVersionConflict) must be true")
	}
	if conflict.StatusCode != 412 || !strings.Contains(conflict.Error(), CodeMemoryFileVersionConflict) {
		t.Errorf("promoted fields/Error(): %d %q", conflict.StatusCode, conflict.Error())
	}
	if len(s.requests()) != 1 {
		t.Error("a 412 must not be retried")
	}

	// current_version null → nil, through PATCH and DELETE too.
	_, files = newScopedFiles(t, errorReply(http.StatusPreconditionFailed, CodeMemoryFileVersionConflict,
		map[string]any{"current_version": json.RawMessage("null")}))
	for name, call := range map[string]func() error{
		"append": func() error {
			_, err := files.Append(ctx, "memory.md", "x", WithExpectedVersion(3))
			return err
		},
		"delete": func() error {
			_, err := files.Delete(ctx, "memory.md", WithExpectedVersion(3))
			return err
		},
	} {
		err := call()
		var c *VersionConflictError
		if !errors.As(err, &c) || c.CurrentVersion != nil {
			t.Errorf("%s: expected a conflict with nil CurrentVersion, got %v", name, err)
		}
	}
}

func TestParseCurrentVersion(t *testing.T) {
	cases := map[string]*int{
		`{"current_version": 8}`:    intPtr(8),
		`{"current_version": null}`: nil,
		`{}`:                        nil,
		`{"current_version": "8"}`:  nil,
		`{"current_version": 8.5}`:  nil,
		`not json`:                  nil,
	}
	for body, want := range cases {
		got := parseCurrentVersion([]byte(body))
		if (got == nil) != (want == nil) || (got != nil && *got != *want) {
			t.Errorf("%s: got %v, want %v", body, got, want)
		}
	}
}

func TestVersionConflictIsTypedOnEveryTransportPath(t *testing.T) {
	ctx := context.Background()
	conflict := errorReply(http.StatusPreconditionFailed, CodeMemoryFileVersionConflict, map[string]any{"current_version": 2})

	_, client := newFileServer(t, conflict)
	_, err := client.DownloadText(ctx, "doc-1") // raw download path
	var c *VersionConflictError
	if !errors.As(err, &c) || c.CurrentVersion == nil || *c.CurrentVersion != 2 {
		t.Errorf("download path: %v", err)
	}

	_, client = newFileServer(t, conflict)
	_, err = client.InsertStream(ctx, strings.NewReader("x"), "a.txt", "") // no-retry path
	if !errors.As(err, &c) {
		t.Errorf("stream path: %v", err)
	}

	// A 412 with any other code stays a plain *APIError.
	_, client = newFileServer(t, errorReply(http.StatusPreconditionFailed, "other", nil))
	_, err = client.Get(ctx, "doc-1")
	if errors.As(err, &c) || errors.Is(err, ErrVersionConflict) {
		t.Errorf("only memory_file_version_conflict is typed: %v", err)
	}
}

// ── PATCH bodies ──────────────────────────────────────────────────────────

func TestMemoryFilePatchBodies(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t)

	if _, err := files.AppendSection(ctx, "memory.md", "Agreed terms", "x"); err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodPatch || req.Path != "/v1/memory/files/memory.md" {
		t.Errorf("append_section: %s %s", req.Method, req.Path)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("append_section Content-Type %q", ct)
	}
	want := map[string]any{"op": "append_section", "section": "Agreed terms", "content": "x", "level": float64(2), "create_if_missing": true}
	if got := req.jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("append_section body %v, want %v", got, want)
	}

	if _, err := files.Append(ctx, "memory.md", "- trailing"); err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"op": "append", "content": "- trailing", "create_if_missing": true}
	if got := s.last(t).jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("append body %v, want %v", got, want)
	}

	if _, err := files.Append(ctx, "memory.md", " ", WithCreateIfMissing(false), WithAuthor("bob")); err != nil {
		t.Fatalf("whitespace content is allowed for append: %v", err)
	}
	req = s.last(t)
	want = map[string]any{"op": "append", "content": " ", "create_if_missing": false}
	if got := req.jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("append body %v, want %v", got, want)
	}
	if req.Query.Get("author") != "bob" {
		t.Errorf("author not sent: %v", req.Query)
	}

	if _, err := files.ReplaceSection(ctx, "memory.md", "Notes", "", WithExpectedVersion(7),
		WithLevel(3), WithCreateIfMissing(false)); err != nil {
		t.Fatal(err)
	}
	want = map[string]any{"op": "replace_section", "section": "Notes", "content": "", "level": float64(3), "create_if_missing": false}
	if got := s.last(t).jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("replace_section body %v, want %v", got, want)
	}
}

func TestMemoryFilePatchGoldenRequests(t *testing.T) {
	ctx := context.Background()

	// P1: append_section under an existing heading.
	s, files := newScopedFiles(t)
	if _, err := files.AppendSection(ctx, "memory.md", "Agreed terms",
		"- 2026-10-10 — Net 45 for EU entities (bob, PRJ-3)"); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"op": "append_section", "section": "Agreed terms",
		"content": "- 2026-10-10 — Net 45 for EU entities (bob, PRJ-3)", "level": float64(2), "create_if_missing": true,
	}
	if got := s.last(t).jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("P1 body %v", got)
	}

	// P3: append_section creates a missing section (201, section_created).
	s, files = newScopedFiles(t, fileReply{status: http.StatusCreated, headers: map[string]string{"ETag": `"8"`},
		body: writeResultBody(map[string]any{
			"bytes": 165, "section_created": true,
			"section": map[string]any{
				"heading": "Key decisions", "level": 2, "slug": "key-decisions",
				"start_line": 11, "end_line": 12, "bytes": 42,
			},
		})})
	result, err := files.AppendSection(ctx, "memory.md", "Key decisions", "- Use Stripe for billing")
	if err != nil {
		t.Fatal(err)
	}
	want = map[string]any{
		"op": "append_section", "section": "Key decisions",
		"content": "- Use Stripe for billing", "level": float64(2), "create_if_missing": true,
	}
	if got := s.last(t).jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("P3 body %v", got)
	}
	if !result.SectionCreated || result.FileCreated || result.Section == nil ||
		result.Section.Slug != "key-decisions" || result.Bytes != 165 || result.Version != 8 {
		t.Errorf("P3 result %+v", result)
	}
	if result.PreviousVersion == nil || *result.PreviousVersion != 7 {
		t.Errorf("P3 previous_version %v", result.PreviousVersion)
	}

	// P4: replace_section by slug, conditional on version 7.
	s, files = newScopedFiles(t)
	if _, err := files.ReplaceSection(ctx, "memory.md", "agreed-terms", "- Net 60", WithExpectedVersion(7)); err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	want = map[string]any{
		"op": "replace_section", "section": "agreed-terms", "content": "- Net 60",
		"level": float64(2), "create_if_missing": true,
	}
	if got := req.jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("P4 body %v", got)
	}
	if req.Header.Get("If-Match") != `"7"` {
		t.Errorf("P4 If-Match %q", req.Header.Get("If-Match"))
	}
}

// ── Read ──────────────────────────────────────────────────────────────────

func TestMemoryFileReadParses(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t, fileReply{status: http.StatusOK, headers: map[string]string{"ETag": `"7"`}, body: readBody(nil)})
	file, err := files.Read(ctx, "memory.md", WithFileSection("Agreed terms"))
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodGet || req.Query.Get("section") != "Agreed terms" {
		t.Errorf("read request %s %v", req.Method, req.Query)
	}
	for _, absent := range []string{"version", "outline", "offset", "max_bytes"} {
		if req.Query.Has(absent) {
			t.Errorf("%s must be omitted: %v", absent, req.Query)
		}
	}
	if file.Version != 7 || !file.IsCurrent || file.Content != agreedContent || file.Name != "memory.md" {
		t.Errorf("unexpected file %+v", file)
	}
	if file.Partition == nil || *file.Partition != fileHandle || file.EntityID != fileOwner {
		t.Errorf("unexpected scope %+v", file.MemoryFileInfo)
	}
	if file.Author == nil || *file.Author != "scoped:ws_42:user:u_3" || file.Op != "append_section" {
		t.Errorf("unexpected author/op %+v", file.MemoryFileInfo)
	}
	if file.NextOffset != nil || file.Truncated || file.TotalBytes != 71 || file.Offset != 0 {
		t.Errorf("unexpected paging %+v", file)
	}
	if file.Section == nil || *file.Section != vectorASections[1] {
		t.Errorf("unexpected section %+v", file.Section)
	}
	if !reflect.DeepEqual(file.Sections, vectorASections) {
		t.Errorf("unexpected outline %+v", file.Sections)
	}
}

func TestMemoryFileReadQueryMapping(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t, okReply(readBody(nil)))
	if _, err := files.Read(ctx, "memory.md", WithFileVersion(3), WithFileOffset(10), WithFileMaxBytes(64)); err != nil {
		t.Fatal(err)
	}
	q := s.last(t).Query
	if q.Get("version") != "3" || q.Get("offset") != "10" || q.Get("max_bytes") != "64" || q.Has("outline") || q.Has("section") {
		t.Errorf("unexpected query %v", q)
	}
	if _, err := files.Read(ctx, "memory.md", WithFileOutline()); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; q.Get("outline") != "true" {
		t.Errorf("outline not sent: %v", q)
	}
}

func TestMemoryFileVersionFallsBackToETag(t *testing.T) {
	ctx := context.Background()
	_, files := newScopedFiles(t, fileReply{
		status: http.StatusOK, headers: map[string]string{"ETag": `"7"`},
		body: readBody(map[string]any{"version": nil}),
	})
	file, err := files.Read(ctx, "memory.md")
	if err != nil {
		t.Fatal(err)
	}
	if file.Version != 7 {
		t.Errorf("version from ETag = %d, want 7", file.Version)
	}

	_, files = newScopedFiles(t, fileReply{
		status: http.StatusOK, headers: map[string]string{"ETag": `"9"`},
		body: writeResultBody(map[string]any{"version": nil}),
	})
	result, err := files.Write(ctx, "memory.md", "x", WithExpectedVersion(8))
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != 9 {
		t.Errorf("write version from ETag = %d, want 9", result.Version)
	}
}

func TestParseETagVersion(t *testing.T) {
	cases := []struct {
		etag string
		want int
		ok   bool
	}{
		{`"7"`, 7, true},
		{`"123456"`, 123456, true},
		{`7`, 7, true},
		{`W/"7"`, 7, true},
		{`"abc"`, 0, false},
		{``, 0, false},
		{`""`, 0, false},
		{`"-1"`, 0, false},
	}
	for _, tc := range cases {
		got, ok := parseETagVersion(tc.etag)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseETagVersion(%q) = %d, %v; want %d, %v", tc.etag, got, ok, tc.want, tc.ok)
		}
	}
}

// ── Write ─────────────────────────────────────────────────────────────────

func TestMemoryFileWriteSendsRawMarkdown(t *testing.T) {
	ctx := context.Background()
	content := "# Acme memory\nIntro line.\n"
	s, files := newScopedFiles(t, fileReply{
		status: http.StatusCreated, headers: map[string]string{"ETag": `"1"`},
		body: writeResultBody(map[string]any{
			"version": 1, "op": "put", "file_created": true, "previous_version": json.RawMessage("null"),
		}),
	})
	result, err := files.Write(ctx, "memory.md", content, WithCreateOnly(), WithAuthor("scoped:ws_42:user:u_3"))
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodPut || req.Path != "/v1/memory/files/memory.md" {
		t.Errorf("write: %s %s", req.Method, req.Path)
	}
	if string(req.Body) != content {
		t.Errorf("body %q, want the raw content", req.Body)
	}
	if ct := req.Header.Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type %q", ct)
	}
	if req.Query.Get("author") != "scoped:ws_42:user:u_3" {
		t.Errorf("author %v", req.Query)
	}
	if !result.FileCreated || result.PreviousVersion != nil || result.Version != 1 || result.Unchanged {
		t.Errorf("unexpected create result %+v", result)
	}
	if !reflect.DeepEqual(result.Sections, vectorASections) {
		t.Errorf("unexpected sections %+v", result.Sections)
	}

	s, files = newScopedFiles(t, okReply(writeResultBody(map[string]any{
		"version": 7, "unchanged": true, "previous_version": 6,
	})))
	result, err = files.Write(ctx, "memory.md", content, WithExpectedVersion(7), WithAuthor(""))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Unchanged || result.Version != 7 {
		t.Errorf("unexpected unchanged result %+v", result)
	}
	if s.last(t).Query.Has("author") {
		t.Error("an empty author must not be sent")
	}
}

// ── List, versions, diff, grep, delete ────────────────────────────────────

func TestMemoryFilesListMapsOptions(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t, okReply(map[string]any{
		"files": []any{fileInfoBody(map[string]any{"author": json.RawMessage("null")})}, "next_cursor": json.RawMessage("null"),
	}))
	page, err := files.List(ctx, WithFilePrefix("notes"), WithFileLimit(50), WithFileCursor("c-1"))
	if err != nil {
		t.Fatal(err)
	}
	q := s.last(t).Query
	want := url.Values{
		"entity_id": {fileOwner}, "prefix": {"notes"}, "limit": {"50"},
		"cursor": {"c-1"}, "partition": {fileHandle},
	}
	if !reflect.DeepEqual(q, want) {
		t.Errorf("list query %v, want %v", q, want)
	}
	if len(page.Files) != 1 || page.NextCursor != nil || page.Files[0].Author != nil || page.Files[0].Version != 7 {
		t.Errorf("unexpected page %+v", page)
	}

	// Empty strings count as unset.
	if _, err := files.List(ctx, WithFilePrefix(""), WithFileCursor("")); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; q.Has("prefix") || q.Has("cursor") || q.Has("limit") {
		t.Errorf("unset options must be omitted: %v", q)
	}
}

func TestMemoryFileVersions(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t, okReply(map[string]any{
		"entity_id": fileOwner, "partition": nil, "name": "memory.md", "file_id": "3d1f",
		"current_version": 7,
		"versions": []any{
			map[string]any{"doc_id": "d7", "version": 7, "bytes": 122, "content_hash": "hash:7", "author": nil,
				"principal": "key:ak_1", "op": "append_section", "updated_at": "2026-10-10T09:12:44Z", "is_current": true},
			map[string]any{"doc_id": "d6", "version": 6, "bytes": 70, "content_hash": "hash:6", "author": "bob",
				"principal": "key:ak_1", "op": "put", "updated_at": "2026-10-09T09:12:44Z", "is_current": false},
		},
	}))
	versions, err := files.Versions(ctx, "memory.md", WithVersionsLimit(5))
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodGet || req.Path != "/v1/memory/files/memory.md/versions" || req.Query.Get("limit") != "5" {
		t.Errorf("versions request %s %s %v", req.Method, req.Path, req.Query)
	}
	if versions.CurrentVersion != 7 || versions.Partition != nil || len(versions.Versions) != 2 {
		t.Fatalf("unexpected versions %+v", versions)
	}
	v7, v6 := versions.Versions[0], versions.Versions[1]
	if !v7.IsCurrent || v7.Author != nil || v6.IsCurrent || v6.Author == nil || *v6.Author != "bob" || v6.Op != "put" {
		t.Errorf("unexpected entries %+v %+v", v7, v6)
	}
	if _, err := files.Versions(ctx, "memory.md"); err != nil {
		t.Fatal(err)
	}
	if s.last(t).Query.Has("limit") {
		t.Error("limit must be omitted when unset")
	}
}

func TestMemoryFileDiff(t *testing.T) {
	ctx := context.Background()
	diffText := "--- memory.md@v6\n+++ memory.md@v7\n@@ -4,3 +4,4 @@\n"
	s, files := newScopedFiles(t, okReply(map[string]any{
		"entity_id": fileOwner, "partition": fileHandle, "name": "memory.md", "file_id": "3d1f",
		"from_version": 6, "to_version": 7, "context": 3, "diff": diffText,
		"added_lines": 1, "removed_lines": 0, "approximate": false, "truncated": false,
	}))
	diff, err := files.Diff(ctx, "memory.md")
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Path != "/v1/memory/files/memory.md/diff" || req.Query.Get("context") != "3" ||
		req.Query.Has("from") || req.Query.Has("to") {
		t.Errorf("default diff request %s %v", req.Path, req.Query)
	}
	if diff.FromVersion != 6 || diff.ToVersion != 7 || diff.Diff != diffText || diff.AddedLines != 1 || diff.Approximate {
		t.Errorf("unexpected diff %+v", diff)
	}

	if _, err := files.Diff(ctx, "memory.md", WithFromVersion(0), WithToVersion(7), WithDiffContext(1)); err != nil {
		t.Fatal(err)
	}
	q := s.last(t).Query
	if q.Get("from") != "0" || q.Get("to") != "7" || q.Get("context") != "1" {
		t.Errorf("diff query %v", q)
	}

	sent := len(s.requests())
	if _, err := files.Diff(ctx, "memory.md", WithFromVersion(-1)); err == nil {
		t.Error("from version -1 must be rejected")
	}
	if _, err := files.Diff(ctx, "memory.md", WithToVersion(0)); err == nil {
		t.Error("to version 0 must be rejected")
	}
	if len(s.requests()) != sent {
		t.Error("no request may be sent for a client-side error")
	}
}

func TestMemoryFileGrep(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t, okReply(map[string]any{
		"matches": []any{map[string]any{
			"entity_id": fileOwner, "partition": fileHandle, "name": "memory.md",
			"version": 7, "line": 5, "section": "Agreed terms", "section_slug": "agreed-terms",
			"text":   "- 2026-09-01 — Net 30 payment terms (alice, ISS-12)",
			"before": []any{"## Agreed terms"}, "after": []any{""},
		}},
		"truncated": false, "truncated_reason": nil, "files_scanned": 1, "bytes_scanned": 122,
	}))
	result, err := files.Grep(ctx, "net")
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodPost || req.Path != "/v1/memory/files/grep" {
		t.Errorf("grep request %s %s", req.Method, req.Path)
	}
	want := map[string]any{
		"entity_id": fileOwner, "pattern": "net", "regex": false, "ignore_case": false,
		"context": float64(0), "max_matches": float64(100),
	}
	if got := req.jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("grep defaults %v, want %v", got, want)
	}
	if len(result.Matches) != 1 || result.Truncated || result.TruncatedReason != nil ||
		result.FilesScanned != 1 || result.BytesScanned != 122 {
		t.Fatalf("unexpected result %+v", result)
	}
	m := result.Matches[0]
	if m.Line != 5 || m.Version != 7 || m.Section == nil || *m.Section != "Agreed terms" ||
		m.SectionSlug == nil || *m.SectionSlug != "agreed-terms" ||
		!reflect.DeepEqual(m.Before, []string{"## Agreed terms"}) || !reflect.DeepEqual(m.After, []string{""}) {
		t.Errorf("unexpected match %+v", m)
	}

	if _, err := files.Grep(ctx, `net\s+\d+`, WithRegex(), WithIgnoreCase(), WithGrepFiles("memory.md"),
		WithGrepContext(1), WithMaxMatches(5)); err != nil {
		t.Fatal(err)
	}
	want = map[string]any{
		"entity_id": fileOwner, "pattern": `net\s+\d+`, "regex": true, "ignore_case": true,
		"files": []any{"memory.md"}, "context": float64(1), "max_matches": float64(5),
	}
	if got := s.last(t).jsonBody(t); !reflect.DeepEqual(got, want) {
		t.Errorf("grep options %v, want %v", got, want)
	}

	if _, err := files.Grep(ctx, "net", WithGrepFiles()); err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).jsonBody(t)["files"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("an explicit empty file list must be sent as []: %v", got)
	}

	sent := len(s.requests())
	if _, err := files.Grep(ctx, ""); err == nil {
		t.Error("an empty pattern must be rejected")
	}
	if len(s.requests()) != sent {
		t.Error("no request may be sent for an empty pattern")
	}
}

func TestMemoryFileDelete(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t, okReply(map[string]any{
		"entity_id": fileOwner, "partition": fileHandle, "name": "memory.md", "file_id": "3d1f",
		"version": 7, "hard": true, "versions_deleted": 7,
	}))
	result, err := files.Delete(ctx, "memory.md", WithHardDelete())
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodDelete || req.Path != "/v1/memory/files/memory.md" || req.Query.Get("hard") != "true" {
		t.Errorf("delete request %s %s %v", req.Method, req.Path, req.Query)
	}
	if !result.Hard || result.VersionsDeleted != 7 || result.Version != 7 || result.FileID != "3d1f" {
		t.Errorf("unexpected delete result %+v", result)
	}
	if _, err := files.Delete(ctx, "memory.md"); err != nil {
		t.Fatal(err)
	}
	if s.last(t).Query.Has("hard") {
		t.Error("a soft delete must not send hard")
	}
}

// ── Client-side validation ────────────────────────────────────────────────

func TestMemoryFilesContentAndSectionValidation(t *testing.T) {
	ctx := context.Background()
	s, files := newScopedFiles(t)
	cases := map[string]func() error{
		"append empty content": func() error {
			_, err := files.Append(ctx, "memory.md", "")
			return err
		},
		"append_section empty content": func() error {
			_, err := files.AppendSection(ctx, "memory.md", "Notes", "")
			return err
		},
		"append_section blank section": func() error {
			_, err := files.AppendSection(ctx, "memory.md", "  ", "x")
			return err
		},
		"replace_section blank section": func() error {
			_, err := files.ReplaceSection(ctx, "memory.md", "", "x", WithExpectedVersion(1))
			return err
		},
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Errorf("%s: expected a client-side error", name)
		}
	}
	expectNoRequests(t, s)
}

// ── Error passthrough ─────────────────────────────────────────────────────

func TestMemoryFileErrorPassthrough(t *testing.T) {
	ctx := context.Background()
	_, files := newScopedFiles(t, errorReply(http.StatusPaymentRequired, CodeCreditExhausted, nil))
	_, err := files.Write(ctx, "memory.md", "x", WithCreateOnly())
	var conflict *VersionConflictError
	if !errors.Is(err, ErrCreditExhausted) || errors.As(err, &conflict) {
		t.Errorf("402 credit_exhausted must surface as ErrCreditExhausted: %v", err)
	}

	cases := []struct {
		reply fileReply
		call  func(*MemoryFiles) error
	}{
		{errorReply(http.StatusPreconditionRequired, CodePreconditionRequired, nil), func(f *MemoryFiles) error {
			_, err := f.Append(ctx, "memory.md", "x")
			return err
		}},
		{errorReply(http.StatusConflict, CodeMemoryFileSectionAmbiguous, map[string]any{
			"candidates": []any{map[string]any{"heading": "Notes", "slug": "notes", "start_line": 16}},
		}), func(f *MemoryFiles) error {
			_, err := f.Read(ctx, "memory.md", WithFileSection("notes"))
			return err
		}},
		{errorReply(http.StatusNotFound, CodeFeatureNotEnabled, map[string]any{"feature": "memory_files"}), func(f *MemoryFiles) error {
			_, err := f.List(ctx)
			return err
		}},
		{errorReply(http.StatusNotFound, CodeMemoryFileNotFound, nil), func(f *MemoryFiles) error {
			_, err := f.Versions(ctx, "memory.md")
			return err
		}},
		{errorReply(http.StatusRequestEntityTooLarge, CodeMemoryFileTooLarge, map[string]any{"limit_bytes": 524288}), func(f *MemoryFiles) error {
			_, err := f.Append(ctx, "memory.md", "x")
			return err
		}},
	}
	for _, tc := range cases {
		_, files := newScopedFiles(t, tc.reply)
		err := tc.call(files)
		var apiErr *APIError
		wantCode := tc.reply.body.(map[string]any)["code"]
		if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.reply.status || apiErr.ErrorCode != wantCode {
			t.Errorf("%v: expected the base *APIError, got %v", wantCode, err)
		}
		if errors.As(err, &conflict) || errors.Is(err, ErrVersionConflict) {
			t.Errorf("%v must not be a version conflict", wantCode)
		}
	}
}

// ── Search hits ───────────────────────────────────────────────────────────

func TestSearchHitParsesMemoryFile(t *testing.T) {
	ctx := context.Background()
	fileHit := map[string]any{
		"doc_id": "a8c2", "score": 91, "content_type": "text/markdown", "content": agreedContent,
		"memory_file": map[string]any{
			"name": "memory.md", "version": 7, "section": "Agreed terms", "section_slug": "agreed-terms",
		},
	}
	plainHit := map[string]any{"doc_id": "d-2", "score": 80, "content_type": "text/plain", "content": "plain"}
	_, client := newFileServer(t, okReply(map[string]any{"query": "net", "results": []any{fileHit, plainHit}}))

	hits, err := client.Search(ctx, "net", 5)
	if err != nil {
		t.Fatal(err)
	}
	want := &MemoryFileHit{Name: "memory.md", Version: 7, Section: strPtr("Agreed terms"), SectionSlug: strPtr("agreed-terms")}
	if !reflect.DeepEqual(hits[0].MemoryFile, want) {
		t.Errorf("memory_file = %+v, want %+v", hits[0].MemoryFile, want)
	}
	if hits[1].MemoryFile != nil {
		t.Errorf("a non-memory-file hit must have nil MemoryFile")
	}

	results, err := client.Retrieve(ctx, "net", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(results[0].MemoryFile, want) || results[1].MemoryFile != nil {
		t.Errorf("Retrieve must carry MemoryFile: %+v / %+v", results[0].MemoryFile, results[1].MemoryFile)
	}

	_, client = newFileServer(t, okReply(map[string]any{"query": "net", "results": []any{map[string]any{
		"doc_id": "a8c2", "score": 91, "content_type": "text/markdown",
		"memory_file": map[string]any{"name": "memory.md", "version": 7, "section": nil, "section_slug": nil},
	}}}))
	hits, err = client.Search(ctx, "net", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hits[0].MemoryFile == nil || hits[0].MemoryFile.Section != nil || hits[0].MemoryFile.SectionSlug != nil {
		t.Errorf("null section must parse as nil: %+v", hits[0].MemoryFile)
	}
}
