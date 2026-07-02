package aether

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helper: create a test server that returns JSON
func jsonServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	client := NewClient(srv.URL)
	t.Cleanup(srv.Close)
	return srv, client
}

func jsonHandler(body any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

func errorHandler(status int, msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}
}

// ── Auth ──────────────────────────────────────────────────────────

func TestAuthorizationHeader(t *testing.T) {
	var gotAuth string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(NodeStatus{})
	})
	client.apiKey = "aether_testkey123"

	_, _ = client.Status(context.Background())
	if gotAuth != "Bearer aether_testkey123" {
		t.Errorf("expected Bearer auth header, got %q", gotAuth)
	}
}

func TestNoAuthHeader(t *testing.T) {
	var gotAuth string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(NodeStatus{})
	})

	_, _ = client.Status(context.Background())
	if gotAuth != "" {
		t.Errorf("expected no auth header, got %q", gotAuth)
	}
}

// ── Error handling ────────────────────────────────────────────────

func TestAPIError401(t *testing.T) {
	_, client := jsonServer(t, errorHandler(401, "Invalid API key"))

	_, err := client.Status(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 401 {
		t.Errorf("expected 401, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "Invalid API key" {
		t.Errorf("expected 'Invalid API key', got %q", apiErr.Message)
	}
}

func TestAPIError404(t *testing.T) {
	_, client := jsonServer(t, errorHandler(404, "Document not found"))

	_, err := client.Get(context.Background(), "nonexistent")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("expected 404, got %d", apiErr.StatusCode)
	}
}

// billingErrorHandler emits the canonical Aether billing-rejection body:
// {"error": "<msg>", "code": "<machine code>", "request_id": "<id>"} with the
// given HTTP status. This mirrors the shape the engine emits for credit/plan
// rejections.
func billingErrorHandler(status int, msg, code, requestID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      msg,
			"code":       code,
			"request_id": requestID,
		})
	}
}

// The client must decode the body's `code` into APIError.ErrorCode
// so callers can branch with errors.Is against the billing sentinels, while
// preserving the human message.
func TestClientDecodesCreditExhaustedBilling(t *testing.T) {
	const msg = "Prepaid credit balance exhausted; top up to continue."
	_, client := jsonServer(t, billingErrorHandler(402, msg, "credit_exhausted", "req-123"))

	_, err := client.Insert(context.Background(), []byte("hello"), "test.txt", "text/plain")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrCreditExhausted) {
		t.Errorf("expected errors.Is(err, ErrCreditExhausted) to be true, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 402 {
		t.Errorf("expected 402, got %d", apiErr.StatusCode)
	}
	if apiErr.ErrorCode != CodeCreditExhausted {
		t.Errorf("expected ErrorCode %q, got %q", CodeCreditExhausted, apiErr.ErrorCode)
	}
	if apiErr.Message != msg {
		t.Errorf("expected message %q, got %q", msg, apiErr.Message)
	}
	// Must not bleed into the other billing sentinels.
	if errors.Is(err, ErrFreeLimitExceeded) || errors.Is(err, ErrTenantPaused) {
		t.Errorf("credit_exhausted must not match other billing sentinels")
	}
}

func TestClientDecodesTenantPausedBilling(t *testing.T) {
	const msg = "Tenant paused by operator; contact support to resume."
	_, client := jsonServer(t, billingErrorHandler(403, msg, "tenant_paused", "req-456"))

	_, err := client.Search(context.Background(), "anything", 5)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrTenantPaused) {
		t.Errorf("expected errors.Is(err, ErrTenantPaused) to be true, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 403 {
		t.Errorf("expected 403, got %d", apiErr.StatusCode)
	}
	if apiErr.ErrorCode != CodeTenantPaused {
		t.Errorf("expected ErrorCode %q, got %q", CodeTenantPaused, apiErr.ErrorCode)
	}
	if apiErr.Message != msg {
		t.Errorf("expected message %q, got %q", msg, apiErr.Message)
	}
}

func TestClientDecodesFreeLimitExceededBilling(t *testing.T) {
	const msg = "Free plan limit reached; upgrade to continue."
	_, client := jsonServer(t, billingErrorHandler(402, msg, "free_limit_exceeded", "req-789"))

	_, err := client.Insert(context.Background(), []byte("hello"), "test.txt", "text/plain")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrFreeLimitExceeded) {
		t.Errorf("expected errors.Is(err, ErrFreeLimitExceeded) to be true, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.ErrorCode != CodeFreeLimitExceeded {
		t.Errorf("expected ErrorCode %q, got %q", CodeFreeLimitExceeded, apiErr.ErrorCode)
	}
	if apiErr.Message != msg {
		t.Errorf("expected message %q, got %q", msg, apiErr.Message)
	}
}

// ── Insert ────────────────────────────────────────────────────────

func TestInsert(t *testing.T) {
	var gotPath, gotMethod string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotMethod = r.Method
		_ = json.NewEncoder(w).Encode(DocumentRecord{
			DocID: "abc-123", CID: "blake3hash", Chunks: 3, Vectors: 3, Version: 1,
		})
	})

	doc, err := client.Insert(context.Background(), []byte("hello"), "test.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if !strings.Contains(gotPath, "filename=test.txt") {
		t.Errorf("expected filename param, got %s", gotPath)
	}
	if !strings.Contains(gotPath, "content_type=text%2Fplain") {
		t.Errorf("expected content_type param, got %s", gotPath)
	}
	if doc.DocID != "abc-123" {
		t.Errorf("expected abc-123, got %s", doc.DocID)
	}
	if doc.Chunks != 3 {
		t.Errorf("expected 3 chunks, got %d", doc.Chunks)
	}
}

// TestInsertParsesSizeBytes guards against the returned DocumentRecord dropping
// SizeBytes/Title/ContentType from the insert response (the size_bytes bug). The
// Go client decodes the body straight into DocumentRecord, so the struct json
// tags carry these through — this test pins that behavior so a future refactor
// to a hand-built record can't silently regress it.
func TestInsertParsesSizeBytes(t *testing.T) {
	title := "My Document"
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DocumentRecord{
			DocID:       "sz-1",
			CID:         "blake3hash",
			Title:       &title,
			ContentType: "text/plain",
			SizeBytes:   12345,
			Chunks:      2,
			Vectors:     2,
			Version:     1,
		})
	})

	doc, err := client.Insert(context.Background(), []byte("hello world"), "doc.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if doc.SizeBytes != 12345 {
		t.Errorf("expected SizeBytes 12345, got %d", doc.SizeBytes)
	}
	if doc.Title == nil || *doc.Title != "My Document" {
		t.Errorf("expected title 'My Document', got %v", doc.Title)
	}
	if doc.ContentType != "text/plain" {
		t.Errorf("expected content_type 'text/plain', got %q", doc.ContentType)
	}
}

// TestInsertStreamParsesSizeBytes mirrors TestInsertParsesSizeBytes for the
// streaming (no-retry) decode path.
func TestInsertStreamParsesSizeBytes(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DocumentRecord{
			DocID:       "sz-stream",
			ContentType: "application/pdf",
			SizeBytes:   98765,
			Version:     1,
		})
	})

	doc, err := client.InsertStream(context.Background(), strings.NewReader("streamed"), "upload.pdf", "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	if doc.SizeBytes != 98765 {
		t.Errorf("expected SizeBytes 98765, got %d", doc.SizeBytes)
	}
	if doc.ContentType != "application/pdf" {
		t.Errorf("expected content_type 'application/pdf', got %q", doc.ContentType)
	}
}

// ── InsertStream ─────────────────────────────────────────────────

func TestInsertStream(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(DocumentRecord{
			DocID: "stream-123", CID: "streamhash", Chunks: 5, Vectors: 5, Version: 1,
		})
	})

	doc, err := client.InsertStream(context.Background(), strings.NewReader("streamed data"), "upload.pdf", "application/pdf")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if !strings.Contains(gotPath, "filename=upload.pdf") {
		t.Errorf("expected filename param, got %s", gotPath)
	}
	if !strings.Contains(gotPath, "content_type=application%2Fpdf") {
		t.Errorf("expected content_type param, got %s", gotPath)
	}
	if gotBody != "streamed data" {
		t.Errorf("expected 'streamed data', got %q", gotBody)
	}
	if doc.DocID != "stream-123" {
		t.Errorf("expected stream-123, got %s", doc.DocID)
	}
	if doc.Chunks != 5 {
		t.Errorf("expected 5 chunks, got %d", doc.Chunks)
	}
}

func TestInsertStreamDefaults(t *testing.T) {
	var gotPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "def-123"})
	})

	_, err := client.InsertStream(context.Background(), strings.NewReader("data"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotPath, "filename=upload.bin") {
		t.Errorf("expected default filename, got %s", gotPath)
	}
}

func TestInsertStreamNoRetry(t *testing.T) {
	callCount := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "Service Unavailable"})
	})

	_, err := client.InsertStream(context.Background(), strings.NewReader("data"), "test.txt", "text/plain")
	if err == nil {
		t.Fatal("expected error on 503")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 503 {
		t.Errorf("expected 503, got %d", apiErr.StatusCode)
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 request (no retry), got %d", callCount)
	}
}

// ── InsertText ────────────────────────────────────────────────────

func TestInsertText(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "txt-456"})
	})

	doc, err := client.InsertText(context.Background(), "some text", "")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != "some text" {
		t.Errorf("expected 'some text', got %q", gotBody)
	}
	if doc.DocID != "txt-456" {
		t.Errorf("expected txt-456, got %s", doc.DocID)
	}
}

// ── Update ────────────────────────────────────────────────────────

func TestUpdate(t *testing.T) {
	var gotMethod, gotPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "abc-123", Version: 2})
	})

	doc, err := client.Update(context.Background(), "abc-123", []byte("updated"), "test.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PUT" {
		t.Errorf("expected PUT, got %s", gotMethod)
	}
	if !strings.Contains(gotPath, "/v1/documents/abc-123") {
		t.Errorf("expected /documents/abc-123, got %s", gotPath)
	}
	if doc.Version != 2 {
		t.Errorf("expected version 2, got %d", doc.Version)
	}
}

// ── Get ───────────────────────────────────────────────────────────

func TestGet(t *testing.T) {
	title := "Test Doc"
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DocumentRecord{
			DocID: "abc-123", Title: &title, SizeBytes: 1024,
		})
	})

	doc, err := client.Get(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Title == nil || *doc.Title != "Test Doc" {
		t.Errorf("expected title 'Test Doc', got %v", doc.Title)
	}
	if doc.SizeBytes != 1024 {
		t.Errorf("expected 1024 bytes, got %d", doc.SizeBytes)
	}
}

// ── Download ──────────────────────────────────────────────────────

func TestDownload(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{1, 2, 3, 4})
	})

	data, err := client.Download(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 4 {
		t.Errorf("expected 4 bytes, got %d", len(data))
	}
}

// ── DownloadText ──────────────────────────────────────────────────

func TestDownloadText(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Hello, document content."))
	})

	text, err := client.DownloadText(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello, document content." {
		t.Errorf("expected content, got %q", text)
	}
}

// ── List ──────────────────────────────────────────────────────────

func TestList(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(struct {
		Documents []DocumentRecord `json:"documents"`
		Count     int              `json:"count"`
		Total     int              `json:"total"`
		HasMore   bool             `json:"has_more"`
	}{
		Documents: []DocumentRecord{
			{DocID: "a", ContentType: "text/plain"},
			{DocID: "b", ContentType: "application/pdf"},
		},
		Count:   2,
		Total:   2,
		HasMore: false,
	}))

	result, err := client.List(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Documents) != 2 {
		t.Errorf("expected 2 docs, got %d", len(result.Documents))
	}
	if result.Documents[0].DocID != "a" {
		t.Errorf("expected 'a', got %s", result.Documents[0].DocID)
	}
}

// ── Delete ────────────────────────────────────────────────────────

func TestDelete(t *testing.T) {
	var gotMethod string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "tombstoned"})
	})

	err := client.Delete(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
}

// HardDelete issues DELETE with ?hard=true (irreversible purge).
func TestHardDelete(t *testing.T) {
	var gotMethod, gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "hard_deleted"})
	})

	if err := client.HardDelete(context.Background(), "abc-123"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotQuery != "hard=true" {
		t.Errorf("expected query hard=true, got %q", gotQuery)
	}
}

// ── Restore ───────────────────────────────────────────────────────

func TestRestore(t *testing.T) {
	var gotMethod, gotPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "restored"})
	})

	err := client.Restore(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if !strings.Contains(gotPath, "/v1/documents/abc-123/restore") {
		t.Errorf("expected restore path, got %s", gotPath)
	}
}

// ── Search ────────────────────────────────────────────────────────

func TestSearch(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{
			Query: "machine learning",
			Results: []SearchResult{
				{DocID: "abc", Score: 85, ContentType: "text/plain"},
			},
		})
	})

	results, err := client.Search(context.Background(), "machine learning", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "q=machine+learning") {
		t.Errorf("expected query param, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "k=5") {
		t.Errorf("expected k param, got %s", gotQuery)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Score != 85 {
		t.Errorf("expected score 85, got %d", results[0].Score)
	}
}

func TestSearchInvalidK(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(searchResponse{Results: []SearchResult{}}))

	_, err := client.Search(context.Background(), "test", 0)
	if err == nil {
		t.Fatal("expected error for k=0")
	}
	if !strings.Contains(err.Error(), "k must be at least 1") {
		t.Errorf("expected validation error, got %s", err.Error())
	}
}

// ── Retrieve ──────────────────────────────────────────────────────

func TestRetrieve(t *testing.T) {
	callCount := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if strings.HasPrefix(r.URL.Path, "/v1/search") {
			_ = json.NewEncoder(w).Encode(searchResponse{
				Results: []SearchResult{
					{DocID: "doc-1", Score: 90, ContentType: "text/plain"},
					{DocID: "doc-2", Score: 70, ContentType: "text/plain"},
				},
			})
		} else if strings.Contains(r.URL.Path, "/v1/documents/doc-1/download") {
			_, _ = w.Write([]byte("Content of doc 1"))
		} else if strings.Contains(r.URL.Path, "/v1/documents/doc-2/download") {
			_, _ = w.Write([]byte("Content of doc 2"))
		}
	})

	results, err := client.Retrieve(context.Background(), "test query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].DocID != "doc-1" || results[0].Content != "Content of doc 1" {
		t.Errorf("unexpected result[0]: %+v", results[0])
	}
	if results[1].Content != "Content of doc 2" {
		t.Errorf("unexpected result[1] content: %q", results[1].Content)
	}
}

func TestRetrieveDeduplicates(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/search") {
			_ = json.NewEncoder(w).Encode(searchResponse{
				Results: []SearchResult{
					{DocID: "doc-1", Score: 90, ContentType: "text/plain"},
					{DocID: "doc-1", Score: 80, ContentType: "text/plain"},
					{DocID: "doc-2", Score: 70, ContentType: "text/plain"},
				},
			})
		} else {
			_, _ = w.Write([]byte("content"))
		}
	})

	results, err := client.Retrieve(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 deduplicated results, got %d", len(results))
	}
	if results[0].Score != 90 {
		t.Errorf("expected best match (score 90), got %d", results[0].Score)
	}
}

// ── Status ────────────────────────────────────────────────────────

func TestStatus(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(NodeStatus{
		Documents: 42, Vectors: 100,
	}))

	s, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Documents != 42 {
		t.Errorf("expected 42 documents, got %d", s.Documents)
	}
	if s.Vectors != 100 {
		t.Errorf("expected 100 vectors, got %d", s.Vectors)
	}
}

// ── Base URL handling ─────────────────────────────────────────────

func TestStripsTrailingSlashes(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(NodeStatus{})
	}))
	defer srv.Close()

	client := NewClient(srv.URL + "///")
	_, _ = client.Status(context.Background())

	if gotPath != "/status" {
		t.Errorf("expected /status, got %s", gotPath)
	}
}

// ── Functional options ────────────────────────────────────────────

func TestWithAPIKey(t *testing.T) {
	client := NewClient("http://localhost:9000", WithAPIKey("aether_key"))
	if client.apiKey != "aether_key" {
		t.Errorf("expected api key to be set")
	}
}

func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 5 * time.Second}
	client := NewClient("http://localhost:9000", WithHTTPClient(custom))
	if client.httpClient != custom {
		t.Error("expected custom http client")
	}
}

// ── Retry logic ──────────────────────────────────────────────────

func TestRetryOnTransientError(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(NodeStatus{Documents: 1})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))

	s, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Documents != 1 {
		t.Errorf("expected 1 document, got %d", s.Documents)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestRetryExhaustion(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(503)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unavailable"})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, WithMaxRetries(1), WithRetryBackoff(time.Millisecond))

	_, err := client.Status(context.Background())
	if err == nil {
		t.Fatal("expected error after retry exhaustion")
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestNoRetryOn404(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}))
	defer srv.Close()
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))

	_, err := client.Get(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("expected 1 attempt (no retry), got %d", attempts)
	}
}

// ── Batch operations ─────────────────────────────────────────────

func TestBatchInsert(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []DocumentRecord{{DocID: "b1", Chunks: 1}},
		})
	})

	docs, err := client.BatchInsert(context.Background(), []BatchInsertItem{
		{Filename: "a.txt", Content: "hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].DocID != "b1" {
		t.Errorf("unexpected result: %+v", docs)
	}
	if !strings.Contains(gotBody, `"filename":"a.txt"`) {
		t.Errorf("expected filename in body, got %s", gotBody)
	}
}

func TestBatchSearch(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{
		"results": []map[string]any{
			{"query": "test", "results": []map[string]any{
				{"doc_id": "a", "distance": 0.1, "content_type": "text/plain"},
			}},
		},
	}))

	results, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{Q: "test", K: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Query != "test" {
		t.Errorf("unexpected result: %+v", results)
	}
}

// ── Async operations ─────────────────────────────────────────────

func TestInsertAsync(t *testing.T) {
	var gotPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(AsyncJobResult{JobID: "j1", Status: "pending", PollURL: "/v1/documents/jobs/j1"})
	})

	result, err := client.InsertAsync(context.Background(), []byte("data"), "test.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.JobID != "j1" {
		t.Errorf("expected j1, got %s", result.JobID)
	}
	if gotPath != "/v1/documents/async" {
		t.Errorf("expected /documents/async, got %s", gotPath)
	}
}

func TestWaitForJobCompleted(t *testing.T) {
	calls := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_ = json.NewEncoder(w).Encode(JobStatus{Status: "pending"})
			return
		}
		docID := "d1"
		_ = json.NewEncoder(w).Encode(JobStatus{Status: "completed", DocID: &docID})
	})

	result, err := client.WaitForJob(context.Background(), "j1", 5*time.Second, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Errorf("expected completed, got %s", result.Status)
	}
	if result.DocID == nil || *result.DocID != "d1" {
		t.Error("expected doc_id d1")
	}
}

// ── Functional options ───────────────────────────────────────────

func TestInsertWithTags(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "t1"})
	})

	_, err := client.Insert(context.Background(), []byte("data"), "test.txt", "", WithTags([]string{"a", "b"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "tags=a%2Cb") {
		t.Errorf("expected tags in query, got %s", gotQuery)
	}
}

func TestSearchWithTags(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5, WithSearchTags([]string{"tag1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "tags=tag1") {
		t.Errorf("expected tags in query, got %s", gotQuery)
	}
}

// ── Entity & time-window filters ─────────────────────────────────

func TestInsertWithEntityID(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "e1"})
	})

	_, err := client.Insert(context.Background(), []byte("data"), "test.txt", "", WithEntityID("user-123"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=user-123") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
}

func TestInsertStreamWithEntityID(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "e2"})
	})

	_, err := client.InsertStream(context.Background(), strings.NewReader("data"), "test.txt", "text/plain", WithEntityID("user-123"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=user-123") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
}

func TestInsertAsyncWithEntityID(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(AsyncJobResult{JobID: "j1", Status: "pending"})
	})

	_, err := client.InsertAsync(context.Background(), []byte("data"), "test.txt", "", WithEntityID("user-123"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=user-123") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
}

func TestInsertWithEmbeddingsEntityID(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "e3"})
	})

	_, err := client.InsertWithEmbeddings(context.Background(), "content", InsertWithEmbeddingsOptions{
		Embedding: []float32{0.1, 0.2},
		EntityID:  "user-7",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"entity_id":"user-7"`) {
		t.Errorf("expected entity_id in body, got %s", gotBody)
	}

	_, err = client.InsertWithEmbeddings(context.Background(), "content", InsertWithEmbeddingsOptions{
		Embedding: []float32{0.1, 0.2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, `"entity_id"`) {
		t.Errorf("expected entity_id omitted when unset, got %s", gotBody)
	}
}

func TestBatchInsertItemEntityID(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []DocumentRecord{{DocID: "b1"}, {DocID: "b2"}},
		})
	})

	_, err := client.BatchInsert(context.Background(), []BatchInsertItem{
		{Filename: "a.txt", Content: "hello", EntityID: "user-1"},
		{Filename: "b.txt", Content: "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"entity_id":"user-1"`) {
		t.Errorf("expected entity_id in body, got %s", gotBody)
	}
	if strings.Count(gotBody, `"entity_id"`) != 1 {
		t.Errorf("expected entity_id omitted for the item without one, got %s", gotBody)
	}
}

func TestSearchWithFilters(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5,
		WithSearchEntityID("user-1"),
		WithSince("2026-01-01T00:00:00Z"),
		WithUntil("2026-06-01T00:00:00Z"),
		WithMaxDistance(0.5),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"entity_id=user-1",
		"since=2026-01-01T00%3A00%3A00Z",
		"until=2026-06-01T00%3A00%3A00Z",
		"max_distance=0.5",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
}

func TestSearchWithLastNDays(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5, WithLastNDays(30))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "last_n_days=30") {
		t.Errorf("expected last_n_days in query, got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "since=") {
		t.Errorf("expected no since param, got %s", gotQuery)
	}
}

func TestSearchOmitsUnsetFilters(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"entity_id=", "since=", "until=", "last_n_days=", "max_distance="} {
		if strings.Contains(gotQuery, param) {
			t.Errorf("expected %s omitted, got %s", param, gotQuery)
		}
	}
}

func TestRetrieveForwardsFilters(t *testing.T) {
	var gotQuery string
	content := "inline content"
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{
			Results: []SearchResult{
				{DocID: "d1", Score: 90, ContentType: "text/plain", Content: &content},
			},
		})
	})

	results, err := client.Retrieve(context.Background(), "test", 3,
		WithSearchEntityID("user-1"), WithLastNDays(7))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Content != "inline content" {
		t.Errorf("unexpected results: %+v", results)
	}
	for _, want := range []string{"entity_id=user-1", "last_n_days=7", "include_content=true"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
}

func TestSearchByVectorWithFilters(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5,
		WithSearchEntityID("user-1"),
		WithSince("2026-01-01T00:00:00Z"),
		WithUntil("2026-06-01T00:00:00Z"),
		WithMaxDistance(0.5),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"entity_id":"user-1"`,
		`"since":"2026-01-01T00:00:00Z"`,
		`"until":"2026-06-01T00:00:00Z"`,
		`"max_distance":0.5`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}

	_, err = client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5, WithLastNDays(14))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"last_n_days":14`) {
		t.Errorf("expected last_n_days in body, got %s", gotBody)
	}

	_, err = client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"entity_id"`, `"since"`, `"until"`, `"last_n_days"`, `"max_distance"`} {
		if strings.Contains(gotBody, field) {
			t.Errorf("expected %s omitted when unset, got %s", field, gotBody)
		}
	}
}

func TestBatchSearchQueryFilters(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})

	_, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{
			Q:           "a",
			K:           3,
			EntityID:    "user-1",
			Since:       "2026-01-01T00:00:00Z",
			Until:       "2026-06-01T00:00:00Z",
			MaxDistance: 0.4,
		},
		{Q: "b", LastNDays: 7},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"entity_id":"user-1"`,
		`"since":"2026-01-01T00:00:00Z"`,
		`"until":"2026-06-01T00:00:00Z"`,
		`"max_distance":0.4`,
		`"last_n_days":7`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}
	if strings.Count(gotBody, `"entity_id"`) != 1 {
		t.Errorf("expected entity_id omitted for the query without one, got %s", gotBody)
	}
}

func TestListWithFilters(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documents": []DocumentRecord{}, "count": 0, "total": 0, "has_more": false,
		})
	})

	_, err := client.List(context.Background(), &ListOptions{
		EntityID: "user-1",
		Since:    "2026-03-01T00:00:00Z",
		Until:    "2026-06-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"entity_id=user-1",
		"since=2026-03-01T00%3A00%3A00Z",
		"until=2026-06-01T00%3A00%3A00Z",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
}

func TestListWithLastNDays(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documents": []DocumentRecord{}, "count": 0, "total": 0, "has_more": false,
		})
	})

	_, err := client.List(context.Background(), &ListOptions{LastNDays: 30, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "last_n_days=30") {
		t.Errorf("expected last_n_days in query, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "limit=10") {
		t.Errorf("expected limit in query, got %s", gotQuery)
	}
}

func TestListOmitsUnsetFilters(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documents": []DocumentRecord{}, "count": 0, "total": 0, "has_more": false,
		})
	})

	_, err := client.List(context.Background(), &ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"entity_id=", "since=", "until=", "last_n_days="} {
		if strings.Contains(gotQuery, param) {
			t.Errorf("expected %s omitted, got %s", param, gotQuery)
		}
	}
}

func TestDocumentRecordEntityIDRoundTrip(t *testing.T) {
	calls := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			entityID := "user-9"
			_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "abc-123", EntityID: &entityID})
			return
		}
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "abc-123"})
	})

	doc, err := client.Get(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if doc.EntityID == nil || *doc.EntityID != "user-9" {
		t.Errorf("expected entity_id user-9, got %v", doc.EntityID)
	}

	doc, err = client.Get(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if doc.EntityID != nil {
		t.Errorf("expected nil entity_id, got %q", *doc.EntityID)
	}
}

// ── Source insert + metadata-facet filters ───────────────────────

func TestInsertWithSource(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "s1"})
	})

	_, err := client.Insert(context.Background(), []byte("data"), "test.txt", "", WithSource("slack"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "source=slack") {
		t.Errorf("expected source in query, got %s", gotQuery)
	}
}

func TestInsertStreamWithSource(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "s2"})
	})

	_, err := client.InsertStream(context.Background(), strings.NewReader("data"), "test.txt", "text/plain", WithSource("upload"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "source=upload") {
		t.Errorf("expected source in query, got %s", gotQuery)
	}
}

func TestInsertAsyncWithSource(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(AsyncJobResult{JobID: "j1", Status: "pending"})
	})

	_, err := client.InsertAsync(context.Background(), []byte("data"), "test.txt", "", WithSource("crawler"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "source=crawler") {
		t.Errorf("expected source in query, got %s", gotQuery)
	}
}

func TestUpdateWithSource(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "s3"})
	})

	_, err := client.Update(context.Background(), "s3", []byte("data"), "test.txt", "", WithSource("slack"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "source=slack") {
		t.Errorf("expected source in query, got %s", gotQuery)
	}
}

func TestInsertWithEmbeddingsSource(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "s4"})
	})

	_, err := client.InsertWithEmbeddings(context.Background(), "content", InsertWithEmbeddingsOptions{
		Embedding: []float32{0.1, 0.2},
		Source:    "slack",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"source":"slack"`) {
		t.Errorf("expected source in body, got %s", gotBody)
	}

	_, err = client.InsertWithEmbeddings(context.Background(), "content", InsertWithEmbeddingsOptions{
		Embedding: []float32{0.1, 0.2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, `"source"`) {
		t.Errorf("expected source omitted when unset, got %s", gotBody)
	}
}

func TestBatchInsertItemSource(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []DocumentRecord{{DocID: "b1"}, {DocID: "b2"}},
		})
	})

	_, err := client.BatchInsert(context.Background(), []BatchInsertItem{
		{Filename: "a.txt", Content: "hello", Source: "slack"},
		{Filename: "b.txt", Content: "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"source":"slack"`) {
		t.Errorf("expected source in body, got %s", gotBody)
	}
	if strings.Count(gotBody, `"source"`) != 1 {
		t.Errorf("expected source omitted for the item without one, got %s", gotBody)
	}
}

func TestBatchInsertItemTagsCSV(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []DocumentRecord{{DocID: "b1"}, {DocID: "b2"}},
		})
	})

	_, err := client.BatchInsert(context.Background(), []BatchInsertItem{
		{Filename: "a.txt", Content: "hello", Tags: []string{"alpha", "beta"}},
		{Filename: "b.txt", Content: "world"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Per-item tags are sent as a comma-separated string, not a JSON array.
	if !strings.Contains(gotBody, `"tags":"alpha,beta"`) {
		t.Errorf("expected CSV tags in body, got %s", gotBody)
	}
	if strings.Contains(gotBody, `"tags":[`) {
		t.Errorf("expected tags not emitted as a JSON array, got %s", gotBody)
	}
	if strings.Count(gotBody, `"tags"`) != 1 {
		t.Errorf("expected tags omitted for the item without any, got %s", gotBody)
	}
}

func TestSearchWithMetadataFacets(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5,
		WithAnyTags("a", "b"),
		WithContentTypes("application/pdf", "text/markdown"),
		WithSources("slack", "upload"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"any_tags=a%2Cb",
		"content_type=application%2Fpdf%2Ctext%2Fmarkdown",
		"source=slack%2Cupload",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
}

func TestSearchOmitsUnsetMetadataFacets(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"any_tags=", "content_type=", "source="} {
		if strings.Contains(gotQuery, param) {
			t.Errorf("expected %s omitted, got %s", param, gotQuery)
		}
	}
}

func TestSearchByVectorWithMetadataFacets(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5,
		WithAnyTags("a", "b"),
		WithContentTypes("application/pdf"),
		WithSources("slack"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"any_tags":["a","b"]`,
		`"content_type":["application/pdf"]`,
		`"source":["slack"]`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}

	_, err = client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"any_tags"`, `"content_type"`, `"source"`} {
		if strings.Contains(gotBody, field) {
			t.Errorf("expected %s omitted when unset, got %s", field, gotBody)
		}
	}
}

func TestBatchSearchQueryMetadataFacets(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})

	_, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{
			Q:            "a",
			K:            3,
			Tags:         []string{"t1", "t2"},
			AnyTags:      []string{"x", "y"},
			ContentTypes: []string{"application/pdf"},
			Sources:      []string{"slack", "upload"},
		},
		{Q: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Batch facets are comma-joined CSV string fields, matching the engine —
	// tags (AND) included, not JSON arrays.
	for _, want := range []string{
		`"tags":"t1,t2"`,
		`"any_tags":"x,y"`,
		`"content_type":"application/pdf"`,
		`"source":"slack,upload"`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}
	// Facets must never be emitted as JSON arrays.
	for _, bad := range []string{`"tags":[`, `"any_tags":[`, `"content_type":[`, `"source":[`} {
		if strings.Contains(gotBody, bad) {
			t.Errorf("expected no JSON-array facet %s, got %s", bad, gotBody)
		}
	}
	if strings.Count(gotBody, `"any_tags"`) != 1 {
		t.Errorf("expected any_tags omitted for the query without one, got %s", gotBody)
	}
}

func TestListWithMetadataFacets(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documents": []DocumentRecord{}, "count": 0, "total": 0, "has_more": false,
		})
	})

	_, err := client.List(context.Background(), &ListOptions{
		Tags:         []string{"t1", "t2"},
		AnyTags:      []string{"a", "b"},
		ContentTypes: []string{"text/markdown"},
		Sources:      []string{"slack"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tags=t1%2Ct2",
		"any_tags=a%2Cb",
		"content_type=text%2Fmarkdown",
		"source=slack",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
}

func TestListOmitsUnsetMetadataFacets(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documents": []DocumentRecord{}, "count": 0, "total": 0, "has_more": false,
		})
	})

	_, err := client.List(context.Background(), &ListOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"tags=", "any_tags=", "content_type=", "source="} {
		if strings.Contains(gotQuery, param) {
			t.Errorf("expected %s omitted, got %s", param, gotQuery)
		}
	}
}

func TestDocumentRecordTagsSourceRoundTrip(t *testing.T) {
	calls := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			source := "slack"
			created := "2026-06-01T00:00:00Z"
			_ = json.NewEncoder(w).Encode(DocumentRecord{
				DocID:     "abc-123",
				Tags:      []string{"alpha", "beta"},
				Source:    &source,
				CreatedAt: &created,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "abc-123"})
	})

	doc, err := client.Get(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Tags) != 2 || doc.Tags[0] != "alpha" || doc.Tags[1] != "beta" {
		t.Errorf("expected tags [alpha beta], got %v", doc.Tags)
	}
	if doc.Source == nil || *doc.Source != "slack" {
		t.Errorf("expected source slack, got %v", doc.Source)
	}
	if doc.CreatedAt == nil || *doc.CreatedAt != "2026-06-01T00:00:00Z" {
		t.Errorf("expected created_at, got %v", doc.CreatedAt)
	}

	doc, err = client.Get(context.Background(), "abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Tags != nil {
		t.Errorf("expected nil tags, got %v", doc.Tags)
	}
	if doc.Source != nil {
		t.Errorf("expected nil source, got %q", *doc.Source)
	}
}

func TestSearchResultTagsSourceCreatedAtRoundTrip(t *testing.T) {
	source := "upload"
	created := "2026-06-02T12:00:00Z"
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{
			{
				DocID:       "d1",
				Score:       90,
				ContentType: "text/plain",
				Tags:        []string{"x", "y"},
				Source:      &source,
				CreatedAt:   &created,
			},
		}})
	})

	results, err := client.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if len(r.Tags) != 2 || r.Tags[0] != "x" || r.Tags[1] != "y" {
		t.Errorf("expected tags [x y], got %v", r.Tags)
	}
	if r.Source == nil || *r.Source != "upload" {
		t.Errorf("expected source upload, got %v", r.Source)
	}
	if r.CreatedAt == nil || *r.CreatedAt != "2026-06-02T12:00:00Z" {
		t.Errorf("expected created_at, got %v", r.CreatedAt)
	}
}

// ── Score + updated_at parsing ────────────────────────────────────

// TestSearchResultScoreAndTimestamps verifies a modern payload: the calibrated
// integer score (0–100, higher = better) parses as-is, and both created_at and
// updated_at are read back as raw strings.
func TestSearchResultScoreAndTimestamps(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"query":"q","results":[
			{"doc_id":"d1","score":80,"content_type":"text/plain",
			 "created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-05T09:30:00Z"}
		]}`)
	})

	results, err := client.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Score != 80 {
		t.Errorf("expected score 80, got %v", r.Score)
	}
	if r.CreatedAt == nil || *r.CreatedAt != "2026-06-01T00:00:00Z" {
		t.Errorf("expected created_at, got %v", r.CreatedAt)
	}
	if r.UpdatedAt == nil || *r.UpdatedAt != "2026-06-05T09:30:00Z" {
		t.Errorf("expected updated_at, got %v", r.UpdatedAt)
	}
}

// TestSearchResultScoreFullRange verifies both extremes of the score range
// parse as-is.
func TestSearchResultScoreFullRange(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"query":"q","results":[
			{"doc_id":"hi","score":100,"content_type":"text/plain"},
			{"doc_id":"lo","score":0,"content_type":"text/plain"}
		]}`)
	})

	results, err := client.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Score != 100 {
		t.Errorf("expected score 100, got %v", results[0].Score)
	}
	if results[1].Score != 0 {
		t.Errorf("expected score 0, got %v", results[1].Score)
	}
}

// TestSearchResultEntityIDEcho verifies the hit echoes entity_id when the
// matched document was written under an entity, and leaves it nil otherwise.
func TestSearchResultEntityIDEcho(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"query":"q","results":[
			{"doc_id":"d1","score":90,"content_type":"text/plain","entity_id":"acct/42"},
			{"doc_id":"d2","score":70,"content_type":"text/plain"}
		]}`)
	})

	results, err := client.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].EntityID == nil || *results[0].EntityID != "acct/42" {
		t.Errorf("expected entity_id acct/42, got %v", results[0].EntityID)
	}
	if results[1].EntityID != nil {
		t.Errorf("expected nil entity_id for unscoped doc, got %v", *results[1].EntityID)
	}
}

// ── Recency options on the wire ───────────────────────────────────

func TestSearchWithRecency(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5, WithRecency(0.4, 14))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"recency_weight=0.4", "half_life_days=14"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}

	// WithHalfLifeDays alone forwards half_life_days without a weight.
	_, err = client.Search(context.Background(), "test", 5, WithHalfLifeDays(7))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "half_life_days=7") {
		t.Errorf("expected half_life_days in query, got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "recency_weight=") {
		t.Errorf("expected no recency_weight when only half-life set, got %s", gotQuery)
	}

	// Unset recency forwards neither param.
	_, err = client.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"recency_weight=", "half_life_days="} {
		if strings.Contains(gotQuery, param) {
			t.Errorf("expected %s omitted when unset, got %s", param, gotQuery)
		}
	}
}

func TestSearchByVectorWithRecency(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5,
		WithRecency(0.6, 30))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"recency_weight":0.6`, `"half_life_days":30`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}

	_, err = client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"recency_weight"`, `"half_life_days"`} {
		if strings.Contains(gotBody, field) {
			t.Errorf("expected %s omitted when unset, got %s", field, gotBody)
		}
	}
}

func TestBatchSearchWithRecency(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})

	_, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{Q: "a", K: 3, RecencyWeight: 0.5, HalfLifeDays: 21},
		{Q: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"recency_weight":0.5`, `"half_life_days":21`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}
	// The second query sets neither, so each recency field appears exactly once.
	if strings.Count(gotBody, `"recency_weight"`) != 1 {
		t.Errorf("expected recency_weight once, got %s", gotBody)
	}
}

func TestRetrieveForwardsRecencyAndTimestamps(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"query":"q","results":[
			{"doc_id":"d1","score":90,"content_type":"text/plain","content":"inline",
			 "created_at":"2026-06-01T00:00:00Z","updated_at":"2026-06-07T00:00:00Z"}
		]}`)
	})

	results, err := client.Retrieve(context.Background(), "test", 3, WithRecency(0.3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.Content != "inline" {
		t.Errorf("expected inline content, got %q", r.Content)
	}
	if r.Score != 90 {
		t.Errorf("expected score 90, got %v", r.Score)
	}
	if r.CreatedAt == nil || *r.CreatedAt != "2026-06-01T00:00:00Z" {
		t.Errorf("expected created_at on retrieval result, got %v", r.CreatedAt)
	}
	if r.UpdatedAt == nil || *r.UpdatedAt != "2026-06-07T00:00:00Z" {
		t.Errorf("expected updated_at on retrieval result, got %v", r.UpdatedAt)
	}
	// recency_weight forwarded; half_life_days omitted (0 -> server default).
	if !strings.Contains(gotQuery, "recency_weight=0.3") {
		t.Errorf("expected recency_weight in query, got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "half_life_days=") {
		t.Errorf("expected half_life_days omitted, got %s", gotQuery)
	}
}

// ── Freshness options on the wire ────────────────────────────────

func TestSearchWithFreshness(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5, WithFreshness(0.4, 7))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"freshness_weight=0.4", "freshness_half_life_days=7"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}

	// WithFreshnessHalfLifeDays alone forwards freshness_half_life_days
	// without a weight.
	_, err = client.Search(context.Background(), "test", 5, WithFreshnessHalfLifeDays(3))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "freshness_half_life_days=3") {
		t.Errorf("expected freshness_half_life_days in query, got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "freshness_weight=") {
		t.Errorf("expected no freshness_weight when only half-life set, got %s", gotQuery)
	}

	// Unset freshness forwards neither param.
	_, err = client.Search(context.Background(), "test", 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, param := range []string{"freshness_weight=", "freshness_half_life_days="} {
		if strings.Contains(gotQuery, param) {
			t.Errorf("expected %s omitted when unset, got %s", param, gotQuery)
		}
	}
}

func TestSearchByVectorWithFreshness(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5,
		WithFreshness(0.6, 21))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"freshness_weight":0.6`, `"freshness_half_life_days":21`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}

	_, err = client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"freshness_weight"`, `"freshness_half_life_days"`} {
		if strings.Contains(gotBody, field) {
			t.Errorf("expected %s omitted when unset, got %s", field, gotBody)
		}
	}
}

func TestBatchSearchWithFreshness(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	})

	_, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{Q: "a", K: 3, FreshnessWeight: 0.5, FreshnessHalfLifeDays: 9},
		{Q: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"freshness_weight":0.5`, `"freshness_half_life_days":9`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("expected %s in body, got %s", want, gotBody)
		}
	}
	// The second query sets neither, so each freshness field appears exactly once.
	if strings.Count(gotBody, `"freshness_weight"`) != 1 {
		t.Errorf("expected freshness_weight once, got %s", gotBody)
	}
}

func TestRetrieveForwardsFreshness(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.WriteString(w, `{"query":"q","results":[
			{"doc_id":"d1","score":90,"content_type":"text/plain","content":"inline"}
		]}`)
	})

	_, err := client.Retrieve(context.Background(), "test", 3, WithFreshness(0.3, 0))
	if err != nil {
		t.Fatal(err)
	}
	// freshness_weight forwarded; freshness_half_life_days omitted
	// (0 -> server default).
	if !strings.Contains(gotQuery, "freshness_weight=0.3") {
		t.Errorf("expected freshness_weight in query, got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "freshness_half_life_days=") {
		t.Errorf("expected freshness_half_life_days omitted, got %s", gotQuery)
	}
}

// ── Backfill entity from tags ────────────────────────────────────

func TestBackfillEntityFromTags(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scanned":           10,
			"updated":           6,
			"skipped_existing":  2,
			"skipped_no_match":  1,
			"skipped_ambiguous": 1,
			"skipped_invalid":   0,
		})
	})

	report, err := client.BackfillEntityFromTags(context.Background(), "patient:", false)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/documents/backfill-entity" {
		t.Errorf("expected /documents/backfill-entity, got %s", gotPath)
	}
	if !strings.Contains(gotBody, `"tag_prefix":"patient:"`) {
		t.Errorf("expected tag_prefix in body, got %s", gotBody)
	}
	if !strings.Contains(gotBody, `"overwrite":false`) {
		t.Errorf("expected overwrite=false in body, got %s", gotBody)
	}
	if report.Scanned != 10 {
		t.Errorf("expected scanned 10, got %d", report.Scanned)
	}
	if report.Updated != 6 {
		t.Errorf("expected updated 6, got %d", report.Updated)
	}
	if report.SkippedExisting != 2 {
		t.Errorf("expected skipped_existing 2, got %d", report.SkippedExisting)
	}
	if report.SkippedNoMatch != 1 {
		t.Errorf("expected skipped_no_match 1, got %d", report.SkippedNoMatch)
	}
	if report.SkippedAmbiguous != 1 {
		t.Errorf("expected skipped_ambiguous 1, got %d", report.SkippedAmbiguous)
	}
	if report.SkippedInvalid != 0 {
		t.Errorf("expected skipped_invalid 0, got %d", report.SkippedInvalid)
	}
}

func TestBackfillEntityFromTagsOverwrite(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(EntityBackfillReport{Scanned: 3, Updated: 3})
	})

	report, err := client.BackfillEntityFromTags(context.Background(), "patient:", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"overwrite":true`) {
		t.Errorf("expected overwrite=true in body, got %s", gotBody)
	}
	if report.Updated != 3 {
		t.Errorf("expected updated 3, got %d", report.Updated)
	}
}

func TestBackfillEntityFromTagsEmptyPrefix(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(EntityBackfillReport{}))
	_, err := client.BackfillEntityFromTags(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "tagPrefix cannot be empty") {
		t.Errorf("expected validation error, got %v", err)
	}
}

func TestBackfillEntityFromTagsAPIError(t *testing.T) {
	_, client := jsonServer(t, errorHandler(400, "tag_prefix must be non-empty"))
	_, err := client.BackfillEntityFromTags(context.Background(), "patient:", false)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("expected 400, got %d", apiErr.StatusCode)
	}
	if apiErr.Message != "tag_prefix must be non-empty" {
		t.Errorf("expected house error message, got %q", apiErr.Message)
	}
}

// ── Input validation ─────────────────────────────────────────────

func TestValidationEmptyDocID(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(DocumentRecord{}))
	_, err := client.Get(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "docID cannot be empty") {
		t.Errorf("expected validation error, got %v", err)
	}
}

func TestValidationEmptyQuery(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(searchResponse{}))
	_, err := client.Search(context.Background(), "", 5)
	if err == nil || !strings.Contains(err.Error(), "query cannot be empty") {
		t.Errorf("expected validation error, got %v", err)
	}
}

func TestValidationEmptyBatchDocuments(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{}))
	_, err := client.BatchInsert(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "documents cannot be empty") {
		t.Errorf("expected validation error, got %v", err)
	}
}

// ── Partition scoping ───────────────────────────────────

// TestPartitionReturnsDistinctScopedClient verifies that Partition returns a new
// scoped object while the original client stays unscoped (sends no partition).
func TestPartitionReturnsDistinctScopedClient(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	scoped, err := client.Partition("tenant-x")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	if scoped == client {
		t.Fatal("expected a distinct scoped client, got the same pointer")
	}

	// The original client sends no partition.
	if _, err := client.Search(context.Background(), "q", 3); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotQuery, "partition") {
		t.Errorf("expected original client to send no partition, got %s", gotQuery)
	}

	// The scoped client sends the partition.
	if _, err := scoped.Search(context.Background(), "q", 3); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "partition=tenant-x") {
		t.Errorf("expected partition=tenant-x in query, got %s", gotQuery)
	}
}

// TestPartitionSharesTransportAndConfig verifies the scoped clone shares the
// underlying transport and configuration (api key, base url) with the parent.
func TestPartitionSharesTransportAndConfig(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	}))
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, WithAPIKey("ak_secret"))
	scoped, err := client.Partition("tenant-x")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	if scoped.httpClient != client.httpClient {
		t.Error("expected scoped client to share the parent's *http.Client")
	}
	if scoped.baseURL != client.baseURL {
		t.Error("expected scoped client to share the parent's base url")
	}
	if _, err := scoped.Search(context.Background(), "q", 1); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ak_secret" {
		t.Errorf("expected inherited auth header, got %q", gotAuth)
	}
}

// TestPartitionRescopeLastWins verifies that re-scoping returns a handle scoped
// to the last partition.
func TestPartitionRescopeLastWins(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	a, err := client.Partition("a")
	if err != nil {
		t.Fatalf("Partition(a): %v", err)
	}
	b, err := a.Partition("b")
	if err != nil {
		t.Fatalf("Partition(b): %v", err)
	}
	if _, err := b.Search(context.Background(), "q", 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "partition=b") {
		t.Errorf("expected partition=b (last wins), got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "partition=a") {
		t.Errorf("did not expect partition=a, got %s", gotQuery)
	}
}

// TestPartitionScopedQueryRoutes verifies the partition is sent as a query param
// for the partition-aware query routes.
func TestPartitionScopedQueryRoutes(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/search"):
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
		case r.URL.Path == "/v1/documents" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"documents": []DocumentRecord{}, "total": 0})
		case r.URL.Path == "/v1/documents/async":
			_ = json.NewEncoder(w).Encode(AsyncJobResult{JobID: "j1"})
		default:
			_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d1"})
		}
	})
	scoped, err := client.Partition("tenant-x")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"search", func() error { _, e := scoped.Search(ctx, "q", 1); return e }},
		{"insert_text", func() error { _, e := scoped.InsertText(ctx, "hi", "n.txt"); return e }},
		{"insert", func() error { _, e := scoped.Insert(ctx, []byte("x"), "n.txt", ""); return e }},
		{"insert_stream", func() error {
			_, e := scoped.InsertStream(ctx, strings.NewReader("x"), "n.txt", "text/plain")
			return e
		}},
		{"insert_async", func() error { _, e := scoped.InsertAsync(ctx, []byte("x"), "n.txt", ""); return e }},
		{"update", func() error { _, e := scoped.Update(ctx, "doc-1", []byte("x"), "n.txt", ""); return e }},
		{"list", func() error { _, e := scoped.List(ctx, nil); return e }},
		{"retrieve", func() error { _, e := scoped.Retrieve(ctx, "q", 1); return e }},
	}
	for _, tc := range cases {
		gotQuery = ""
		if err := tc.call(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(gotQuery, "partition=tenant-x") {
			t.Errorf("%s: expected partition=tenant-x in query, got %s", tc.name, gotQuery)
		}
	}
}

// TestPartitionScopedBodyRoutes verifies the partition is sent in the JSON body
// for the partition-aware body routes, including per-item / per-query placement.
func TestPartitionScopedBodyRoutes(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch r.URL.Path {
		case "/v1/search/embed":
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
		case "/v1/documents/embed":
			_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d1"})
		case "/v1/documents/batch":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []DocumentRecord{{DocID: "d1"}, {DocID: "d2"}}})
		case "/v1/search/batch":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []BatchSearchResponse{}})
		}
	})
	scoped, err := client.Partition("tenant-x")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	ctx := context.Background()

	// search_by_vector → body field.
	gotBody = ""
	if _, err := scoped.SearchByVector(ctx, []float32{0.1, 0.2}, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"partition":"tenant-x"`) {
		t.Errorf("search_by_vector: expected partition in body, got %s", gotBody)
	}

	// insert_with_embeddings → body field.
	gotBody = ""
	if _, err := scoped.InsertWithEmbeddings(ctx, "content", InsertWithEmbeddingsOptions{Embedding: []float32{0.1}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"partition":"tenant-x"`) {
		t.Errorf("insert_with_embeddings: expected partition in body, got %s", gotBody)
	}

	// batch_insert → per-item body field, same partition on every item.
	gotBody = ""
	if _, err := scoped.BatchInsert(ctx, []BatchInsertItem{
		{Filename: "a.txt", Content: "a"},
		{Filename: "b.txt", Content: "b"},
	}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(gotBody, `"partition":"tenant-x"`); n != 2 {
		t.Errorf("batch_insert: expected partition on every item (2), got %d in %s", n, gotBody)
	}

	// batch_search → per-query body field, same partition on every query.
	gotBody = ""
	if _, err := scoped.BatchSearch(ctx, []BatchSearchQuery{
		{Q: "one", K: 1},
		{Q: "two", K: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(gotBody, `"partition":"tenant-x"`); n != 2 {
		t.Errorf("batch_search: expected partition on every query (2), got %d in %s", n, gotBody)
	}
}

// TestUnscopedClientOmitsPartition verifies the default client sends no partition
// in either query or body locations (byte-identical to the unscoped default behavior).
func TestUnscopedClientOmitsPartition(t *testing.T) {
	var gotQuery, gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		switch r.URL.Path {
		case "/v1/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
		case "/v1/documents/batch":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []DocumentRecord{{DocID: "d1"}}})
		default:
			_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d1"})
		}
	})
	ctx := context.Background()

	if _, err := client.Search(ctx, "q", 1); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotQuery, "partition") {
		t.Errorf("unscoped search: expected no partition, got %s", gotQuery)
	}

	if _, err := client.BatchInsert(ctx, []BatchInsertItem{{Filename: "a.txt", Content: "a"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotBody, "partition") {
		t.Errorf("unscoped batch_insert: expected no partition, got %s", gotBody)
	}
}

// TestDocIDAddressedMethodsSendNoPartition verifies that doc_id-addressed methods
// send no partition even from a scoped handle.
func TestDocIDAddressedMethodsSendNoPartition(t *testing.T) {
	var gotURL string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "doc-1"})
	})
	scoped, err := client.Partition("tenant-x")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	ctx := context.Background()

	gotURL = ""
	if _, err := scoped.Get(ctx, "doc-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotURL, "partition") {
		t.Errorf("get: expected no partition, got %s", gotURL)
	}

	gotURL = ""
	if err := scoped.Delete(ctx, "doc-1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotURL, "partition") {
		t.Errorf("delete: expected no partition, got %s", gotURL)
	}
}

// TestPartitionValidation verifies that empty/whitespace and >256-char partition
// ids are rejected with an argument error and make no HTTP call.
func TestPartitionValidation(t *testing.T) {
	var called bool
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(DocumentRecord{})
	})

	for _, bad := range []string{"", "   ", "\t\n", strings.Repeat("x", 257)} {
		called = false
		scoped, err := client.Partition(bad)
		if err == nil {
			t.Errorf("expected error for partition %q, got nil", bad)
		}
		if scoped != nil {
			t.Errorf("expected nil client for invalid partition %q", bad)
		}
		if called {
			t.Errorf("expected no HTTP call for invalid partition %q", bad)
		}
	}

	// Exactly 256 chars is accepted.
	maxOK := strings.Repeat("x", 256)
	if _, err := client.Partition(maxOK); err != nil {
		t.Errorf("expected 256-char partition to be accepted, got %v", err)
	}
}

// ── Ingestion ─────────────────────────────────────────────────────

// writeTempFile creates a file with the given content under dir and returns its
// full path.
func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveIngestContentType pins the extension → content-type resolution,
// including the .md → text/markdown explicit map entry and the octet-stream
// fallback for an unknown extension.
func TestResolveIngestContentType(t *testing.T) {
	cases := map[string]string{
		"notes.md":        "text/markdown",
		"readme.markdown": "text/markdown",
		"a.txt":           "text/plain",
		"a.text":          "text/plain",
		"doc.pdf":         "application/pdf",
		"data.csv":        "text/csv",
		"x.json":          "application/json",
		"page.html":       "text/html",
		"page.htm":        "text/html",
		"weird.zzznope":   "application/octet-stream",
	}
	for name, want := range cases {
		if got := resolveIngestContentType(name); got != want {
			t.Errorf("resolveIngestContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestIngestFilesMixedBatch is the core graceful-degradation case: the test
// server returns 422 for one specific file, and IngestFiles reports it as
// "skipped" while the rest are "ingested" — without returning a function error.
func TestIngestFilesMixedBatch(t *testing.T) {
	dir := t.TempDir()
	good1 := writeTempFile(t, dir, "good1.md", "# hello")
	bad := writeTempFile(t, dir, "bad.bin", "\x00\x01\x02binary")
	good2 := writeTempFile(t, dir, "good2.txt", "plain text")

	var inserts int
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		// The engine rejects the binary file with 422 (unprocessable).
		if strings.Contains(r.URL.RawQuery, "bad.bin") {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unsupported or binary content"})
			return
		}
		inserts++
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: fmt.Sprintf("doc-%d", inserts)})
	})

	results, err := client.IngestFiles(context.Background(), []string{good1, bad, good2})
	if err != nil {
		t.Fatalf("IngestFiles returned a function error for a per-file rejection: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Results are returned in input order.
	if results[0].Path != good1 || results[0].Status != "ingested" || results[0].DocID == "" {
		t.Errorf("result[0] = %+v, want ingested good1 with a doc id", results[0])
	}
	if results[0].ContentType != "text/markdown" {
		t.Errorf("result[0].ContentType = %q, want text/markdown", results[0].ContentType)
	}
	if results[1].Path != bad || results[1].Status != "skipped" {
		t.Errorf("result[1] = %+v, want skipped bad", results[1])
	}
	if results[1].DocID != "" {
		t.Errorf("skipped result must not carry a doc id, got %q", results[1].DocID)
	}
	if !strings.Contains(results[1].Error, "unsupported or binary content") {
		t.Errorf("result[1].Error = %q, want the server message", results[1].Error)
	}
	if results[2].Path != good2 || results[2].Status != "ingested" || results[2].DocID == "" {
		t.Errorf("result[2] = %+v, want ingested good2 with a doc id", results[2])
	}
}

// TestIngestFilesSkipStatuses confirms that 413/415/422 are all classified as
// "skipped" while other API failures (e.g. 500) surface as "error".
func TestIngestFilesSkipStatuses(t *testing.T) {
	cases := []struct {
		status     int
		wantStatus string
	}{
		{http.StatusRequestEntityTooLarge, "skipped"}, // 413
		{http.StatusUnsupportedMediaType, "skipped"},  // 415
		{http.StatusUnprocessableEntity, "skipped"},   // 422
		{http.StatusInternalServerError, "error"},     // 500
	}
	for _, tc := range cases {
		dir := t.TempDir()
		f := writeTempFile(t, dir, "f.txt", "data")
		_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "nope"})
		})
		// 413/415/422/500 are all non-retryable, so each resolves on the first
		// attempt regardless of the client's retry setting.
		results, err := client.IngestFiles(context.Background(), []string{f})
		if err != nil {
			t.Fatalf("status %d: unexpected function error: %v", tc.status, err)
		}
		if len(results) != 1 || results[0].Status != tc.wantStatus {
			t.Errorf("status %d: got %+v, want Status %q", tc.status, results, tc.wantStatus)
		}
	}
}

// TestIngestFilesReadError reports a missing/unreadable file as "error" (not
// "skipped") and never hits the network.
func TestIngestFilesReadError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.txt")
	var hit bool
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "x"})
	})

	results, err := client.IngestFiles(context.Background(), []string{missing})
	if err != nil {
		t.Fatalf("unexpected function error: %v", err)
	}
	if len(results) != 1 || results[0].Status != "error" {
		t.Fatalf("expected one error result, got %+v", results)
	}
	if results[0].Error == "" {
		t.Errorf("expected an error message for an unreadable file")
	}
	if hit {
		t.Errorf("unreadable file should not be sent to the server")
	}
}

// TestIngestFilesForwardsInsertOptions confirms InsertOptions (tags/chunking)
// pass through to each insert.
func TestIngestFilesForwardsInsertOptions(t *testing.T) {
	dir := t.TempDir()
	f := writeTempFile(t, dir, "f.md", "# doc")
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d1"})
	})

	_, err := client.IngestFiles(context.Background(), []string{f},
		WithTags([]string{"x", "y"}), WithChunking(800, 100))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "tags=x%2Cy") {
		t.Errorf("expected tags in query, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "chunk_size=800") || !strings.Contains(gotQuery, "overlap=100") {
		t.Errorf("expected chunking params in query, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "content_type=text%2Fmarkdown") {
		t.Errorf("expected .md → text/markdown content_type, got %s", gotQuery)
	}
}

// TestIngestDirectoryRecursive walks nested directories by default and ingests
// every regular file.
func TestIngestDirectoryRecursive(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "top.md", "# top")
	writeTempFile(t, dir, "sub/nested.txt", "nested")
	writeTempFile(t, dir, "sub/deep/deeper.md", "deeper")

	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d"})
	})

	results, err := client.IngestDirectory(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 files ingested recursively, got %d: %+v", len(results), results)
	}
	// Lexical path order is deterministic.
	if !sortedAscending(results) {
		t.Errorf("expected results in lexical path order, got %+v", results)
	}
}

// TestIngestDirectoryNonRecursive stays at the top level when WithRecursive is
// false.
func TestIngestDirectoryNonRecursive(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "top1.md", "1")
	writeTempFile(t, dir, "top2.md", "2")
	writeTempFile(t, dir, "sub/nested.md", "n")

	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d"})
	})

	results, err := client.IngestDirectory(context.Background(), dir, WithRecursive(false))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 top-level files, got %d: %+v", len(results), results)
	}
	for _, r := range results {
		if strings.Contains(r.Path, "sub"+string(filepath.Separator)) {
			t.Errorf("non-recursive walk picked up a nested file: %s", r.Path)
		}
	}
}

// TestIngestDirectoryExtensionFilter ingests only files whose extension matches
// the WithExtensions filter (leading dot and case are optional).
func TestIngestDirectoryExtensionFilter(t *testing.T) {
	dir := t.TempDir()
	mdFile := writeTempFile(t, dir, "keep.md", "# keep")
	writeTempFile(t, dir, "skip.txt", "skip")
	writeTempFile(t, dir, "skip.bin", "skip")
	writeTempFile(t, dir, "sub/also.MD", "# also")

	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d"})
	})

	// "md" without a leading dot, exercising the normalization.
	results, err := client.IngestDirectory(context.Background(), dir, WithExtensions("md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 .md files, got %d: %+v", len(results), results)
	}
	for _, r := range results {
		if strings.ToLower(filepath.Ext(r.Path)) != ".md" {
			t.Errorf("extension filter let through %s", r.Path)
		}
		if r.ContentType != "text/markdown" {
			t.Errorf("expected text/markdown, got %q for %s", r.ContentType, r.Path)
		}
	}
	_ = mdFile
}

// TestIngestDirectoryForwardsInsertOptions confirms WithInsertOptions flows
// InsertOptions to each insert in the directory walk.
func TestIngestDirectoryForwardsInsertOptions(t *testing.T) {
	dir := t.TempDir()
	writeTempFile(t, dir, "a.md", "# a")
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d"})
	})

	_, err := client.IngestDirectory(context.Background(), dir,
		WithInsertOptions(WithTags([]string{"docs"}), WithSource("upload")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "tags=docs") {
		t.Errorf("expected forwarded tags, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "source=upload") {
		t.Errorf("expected forwarded source, got %s", gotQuery)
	}
}

// TestIngestDirectoryNotADirectory returns a function error (not a result set)
// when the input is a file or does not exist.
func TestIngestDirectoryNotADirectory(t *testing.T) {
	dir := t.TempDir()
	f := writeTempFile(t, dir, "file.txt", "data")
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d"})
	})

	if _, err := client.IngestDirectory(context.Background(), f); err == nil {
		t.Errorf("expected an error for a non-directory input, got nil")
	}
	if _, err := client.IngestDirectory(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Errorf("expected an error for a missing directory, got nil")
	}
}

// sortedAscending reports whether the result paths are in non-decreasing order.
func sortedAscending(results []IngestResult) bool {
	for i := 1; i < len(results); i++ {
		if results[i-1].Path > results[i].Path {
			return false
		}
	}
	return true
}

// suppress unused import warning
var _ = fmt.Sprintf
