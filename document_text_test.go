package aether

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const parsedText = "# Master Services Agreement\n\nParsed text."

func documentTextBody(overrides map[string]any) map[string]any {
	body := map[string]any{
		"doc_id":       "doc-1",
		"content_type": "application/pdf",
		"source":       "passages",
		"state":        "ready",
		"text":         parsedText,
		"offset":       0,
		"next_offset":  nil,
		"total_bytes":  len(parsedText),
		"truncated":    false,
		"section":      nil,
		"sections":     nil,
	}
	for k, v := range overrides {
		body[k] = v
	}
	return body
}

// textServer is a route-keyed fake engine that records the document reads it
// served. textStatus 0 means a ready /text response; forbidText fails the
// test on any /text call.
type textServer struct {
	mu         sync.Mutex
	search     map[string]any
	listing    map[string]any
	textStatus int
	textBody   map[string]any
	forbidText bool
	downloads  []string
	texts      []string
}

func (s *textServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/search":
			_ = json.NewEncoder(w).Encode(map[string]any{"query": "q", "results": []any{s.search}})
		case r.URL.Path == "/v1/documents":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": []any{s.listing}, "count": 1, "total": 1, "has_more": false,
			})
		case strings.HasSuffix(r.URL.Path, "/download"):
			s.downloads = append(s.downloads, r.URL.Path)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("original text"))
		case strings.HasSuffix(r.URL.Path, "/text"):
			if s.forbidText {
				t.Errorf("/text must not be called: %s", r.URL)
			}
			s.texts = append(s.texts, r.URL.RequestURI())
			status := s.textStatus
			if status == 0 {
				status = http.StatusOK
			}
			body := s.textBody
			if body == nil {
				if status == http.StatusOK {
					body = documentTextBody(nil)
				} else {
					body = map[string]any{"error": "not found", "code": "document_not_found"}
				}
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

func newTextClient(t *testing.T, s *textServer) *Client {
	t.Helper()
	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, WithMaxRetries(0), WithRetryBackoff(time.Millisecond))
}

func textHit(contentType string, extra map[string]any) map[string]any {
	hit := map[string]any{
		"doc_id": "doc-1", "score": 90, "content_type": contentType,
		"passage": "the matched passage", "tags": []string{}, "metadata": map[string]any{},
	}
	for k, v := range extra {
		hit[k] = v
	}
	return hit
}

func textRecord(contentType string, extra map[string]any) map[string]any {
	record := map[string]any{
		"doc_id": "doc-1", "cid": "", "content_type": contentType, "size_bytes": 10,
		"version": 1, "created_at": "2026-10-01T00:00:00Z", "entity_id": "acme",
	}
	for k, v := range extra {
		record[k] = v
	}
	return record
}

var (
	downloadRows = []string{
		"text/plain", "text/markdown; charset=utf-8", "TEXT/MARKDOWN",
		"text/x-python", "application/json", "application/x-yaml",
	}
	getTextRows = []string{
		"text/html", "text/csv", "application/pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/octet-stream", "",
	}
)

func TestGetTextMapsOptionsAndParses(t *testing.T) {
	s := &textServer{textBody: documentTextBody(map[string]any{
		"text": "## Agreed terms\n- Net 30\n", "next_offset": 25, "truncated": true,
		"section": map[string]any{
			"heading": "Agreed terms", "level": 2, "slug": "agreed-terms",
			"start_line": 4, "end_line": 6, "bytes": 71,
		},
	})}
	client, err := newTextClient(t, s).Partition("client-a")
	if err != nil {
		t.Fatal(err)
	}
	text, err := client.GetText(context.Background(), "doc 1",
		WithTextSection("Agreed terms"), WithOffset(0), WithMaxBytes(25))
	if err != nil {
		t.Fatal(err)
	}
	got := s.texts[0]
	for _, want := range []string{
		"/v1/documents/doc%201/text?", "section=Agreed+terms", "offset=0",
		"max_bytes=25", "partition=client-a",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("request %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "outline") {
		t.Errorf("outline must be omitted: %q", got)
	}
	if text.Source == nil || *text.Source != "passages" || text.State != "ready" {
		t.Errorf("unexpected source/state: %+v", text)
	}
	if text.NextOffset == nil || *text.NextOffset != 25 || !text.Truncated {
		t.Errorf("unexpected paging: %+v", text)
	}
	if text.Section == nil || text.Section.Slug != "agreed-terms" || text.Section.StartLine != 4 {
		t.Errorf("unexpected section: %+v", text.Section)
	}
	if text.Sections != nil {
		t.Errorf("sections must be nil without WithOutline")
	}
}

func TestGetTextOutlineAndNullNextOffset(t *testing.T) {
	s := &textServer{textBody: documentTextBody(map[string]any{
		"text": "",
		"sections": []any{map[string]any{
			"heading": "Acme memory", "level": 1, "slug": "acme-memory",
			"start_line": 1, "end_line": 9, "bytes": 122,
		}},
	})}
	text, err := newTextClient(t, s).GetText(context.Background(), "doc-1", WithOutline())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.texts[0], "outline=true") {
		t.Errorf("outline not sent: %q", s.texts[0])
	}
	if text.NextOffset != nil {
		t.Errorf("next_offset must parse as nil")
	}
	if len(text.Sections) != 1 || text.Sections[0].Bytes != 122 {
		t.Errorf("unexpected sections: %+v", text.Sections)
	}
}

func TestGetTextRejectsEmptyDocIDWithoutHTTP(t *testing.T) {
	s := &textServer{}
	if _, err := newTextClient(t, s).GetText(context.Background(), ""); err == nil {
		t.Fatal("expected a client-side error")
	}
	if len(s.texts) != 0 {
		t.Fatal("no request may be sent")
	}
}

func TestGetTextSurfacesSectionErrors(t *testing.T) {
	s := &textServer{textStatus: http.StatusConflict, textBody: map[string]any{
		"error": "ambiguous", "code": "document_section_ambiguous",
	}}
	_, err := newTextClient(t, s).GetText(context.Background(), "doc-1", WithTextSection("notes"))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 || apiErr.ErrorCode != "document_section_ambiguous" {
		t.Fatalf("expected a 409 APIError, got %v", err)
	}
}

func TestIsTextLikeContentTypeGoldenTable(t *testing.T) {
	for _, ct := range downloadRows {
		if !isTextLikeContentType(ct) {
			t.Errorf("%q must be text-like", ct)
		}
	}
	for _, ct := range getTextRows {
		if isTextLikeContentType(ct) {
			t.Errorf("%q must not be text-like", ct)
		}
	}
}

func TestRetrieveHydrationGoldenTable(t *testing.T) {
	for _, ct := range downloadRows {
		s := &textServer{search: textHit(ct, nil), forbidText: true}
		results, err := newTextClient(t, s).Retrieve(context.Background(), "q", 5)
		if err != nil {
			t.Fatalf("%q: %v", ct, err)
		}
		if results[0].Content != "original text" || len(s.downloads) != 1 {
			t.Errorf("%q must be downloaded: %+v / %v", ct, results[0].Content, s.downloads)
		}
	}
	for _, ct := range getTextRows {
		s := &textServer{search: textHit(ct, nil)}
		results, err := newTextClient(t, s).Retrieve(context.Background(), "q", 5)
		if err != nil {
			t.Fatalf("%q: %v", ct, err)
		}
		if results[0].Content != parsedText {
			t.Errorf("%q: content %q", ct, results[0].Content)
		}
		if len(s.downloads) != 0 {
			t.Errorf("%q must never be downloaded", ct)
		}
		if len(s.texts) != 1 || !strings.Contains(s.texts[0], "max_bytes=1048576") {
			t.Errorf("%q: /text calls %v", ct, s.texts)
		}
	}
}

func TestMemoryListHydrationGoldenTable(t *testing.T) {
	for _, ct := range append(append([]string{}, downloadRows...), getTextRows...) {
		s := &textServer{listing: textRecord(ct, nil), forbidText: isTextLikeContentType(ct)}
		mem, err := NewMemoryWithClient("acme", newTextClient(t, s))
		if err != nil {
			t.Fatal(err)
		}
		items, err := mem.List(context.Background())
		if err != nil {
			t.Fatalf("%q: %v", ct, err)
		}
		want := parsedText
		if isTextLikeContentType(ct) {
			want = "original text"
		}
		if items[0].Text != want {
			t.Errorf("%q: text %q, want %q", ct, items[0].Text, want)
		}
		if !isTextLikeContentType(ct) && len(s.downloads) != 0 {
			t.Errorf("%q must never be downloaded", ct)
		}
	}
}

func TestHydrationFallbacks(t *testing.T) {
	ctx := context.Background()
	unsupported := documentTextBody(map[string]any{"state": "unsupported", "source": nil, "text": ""})

	// Retrieve: unsupported → passage; 404 → passage; no passage → "".
	s := &textServer{search: textHit("application/pdf", nil), textBody: unsupported}
	results, err := newTextClient(t, s).Retrieve(ctx, "q", 5)
	if err != nil || results[0].Content != "the matched passage" {
		t.Errorf("unsupported → passage: %v %+v", err, results)
	}
	s = &textServer{search: textHit("application/pdf", nil), textStatus: http.StatusNotFound}
	results, err = newTextClient(t, s).Retrieve(ctx, "q", 5)
	if err != nil || results[0].Content != "the matched passage" || len(s.downloads) != 0 {
		t.Errorf("404 → passage: %v %+v", err, results)
	}
	s = &textServer{search: textHit("application/pdf", map[string]any{"passage": nil}), textStatus: http.StatusNotFound}
	results, err = newTextClient(t, s).Retrieve(ctx, "q", 5)
	if err != nil || results[0].Content != "" {
		t.Errorf("missing passage → \"\": %v %+v", err, results)
	}

	// Recall uses the hydrated content.
	s = &textServer{search: textHit("application/pdf", nil)}
	mem, _ := NewMemoryWithClient("acme", newTextClient(t, s))
	items, err := mem.Recall(ctx, "terms")
	if err != nil || items[0].Text != parsedText || len(s.downloads) != 0 {
		t.Errorf("recall: %v %+v", err, items)
	}

	// List: unsupported / 404 → "".
	for _, srv := range []*textServer{
		{listing: textRecord("application/pdf", nil), textBody: unsupported},
		{listing: textRecord("application/pdf", nil), textStatus: http.StatusNotFound},
	} {
		mem, _ := NewMemoryWithClient("acme", newTextClient(t, srv))
		items, err := mem.List(ctx)
		if err != nil || items[0].Text != "" {
			t.Errorf("list fallback: %v %+v", err, items)
		}
	}

	// Other errors propagate.
	s = &textServer{search: textHit("application/pdf", nil), textStatus: http.StatusInternalServerError,
		textBody: map[string]any{"error": "boom"}}
	_, err = newTextClient(t, s).Retrieve(ctx, "q", 5)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 {
		t.Errorf("a 500 must propagate, got %v", err)
	}
}

func TestMediaIsNeverRead(t *testing.T) {
	ctx := context.Background()
	for _, row := range [][2]string{{"image/png", "image"}, {"audio/mpeg", "audio"}} {
		s := &textServer{search: textHit(row[0], map[string]any{"modality": row[1]}), forbidText: true}
		results, err := newTextClient(t, s).Retrieve(ctx, "q", 5)
		if err != nil || results[0].Content != "the matched passage" || len(s.downloads) != 0 {
			t.Errorf("%s retrieve: %v %+v", row[0], err, results)
		}
		s = &textServer{
			listing:    textRecord(row[0], map[string]any{"modality": row[1], "derived_text": "a caption"}),
			forbidText: true,
		}
		mem, _ := NewMemoryWithClient("acme", newTextClient(t, s))
		items, err := mem.List(ctx)
		if err != nil || items[0].Text != "a caption" || len(s.downloads) != 0 {
			t.Errorf("%s list: %v %+v", row[0], err, items)
		}
	}
}
