package aether

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	if !strings.Contains(gotPath, "/documents/abc-123") {
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
	if !strings.Contains(gotPath, "/documents/abc-123/restore") {
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
		t.Errorf("expected 85, got %d", results[0].Score)
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
		if strings.HasPrefix(r.URL.Path, "/search") {
			_ = json.NewEncoder(w).Encode(searchResponse{
				Results: []SearchResult{
					{DocID: "doc-1", Score: 92, ContentType: "text/plain"},
					{DocID: "doc-2", Score: 70, ContentType: "text/plain"},
				},
			})
		} else if strings.Contains(r.URL.Path, "/documents/doc-1/download") {
			_, _ = w.Write([]byte("Content of doc 1"))
		} else if strings.Contains(r.URL.Path, "/documents/doc-2/download") {
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
		if strings.HasPrefix(r.URL.Path, "/search") {
			_ = json.NewEncoder(w).Encode(searchResponse{
				Results: []SearchResult{
					{DocID: "doc-1", Score: 92, ContentType: "text/plain"},
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
	if results[0].Score != 92 {
		t.Errorf("expected closest match (92), got %d", results[0].Score)
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
		{Filename: "a.txt", Content: "hello", Tags: []string{"x", "y"}},
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
	// Tags must be sent as a comma-joined string, not a JSON array, to match
	// the prod batch deserializer (which rejects arrays with HTTP 422).
	if !strings.Contains(gotBody, `"tags":"x,y"`) {
		t.Errorf("expected comma-joined tags string in body, got %s", gotBody)
	}
	if strings.Contains(gotBody, `"tags":[`) {
		t.Errorf("tags must not be a JSON array, got %s", gotBody)
	}
}

func TestBatchSearch(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"query": "test", "results": []map[string]any{
					{"doc_id": "a", "distance": 0.1, "content_type": "text/plain"},
				}},
			},
		})
	})

	results, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{Q: "test", K: 5, Tags: []string{"x", "y"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Query != "test" {
		t.Errorf("unexpected result: %+v", results)
	}
	// Tags must be sent as a comma-joined string, not a JSON array, to match
	// the prod batch deserializer (which rejects arrays with HTTP 422).
	if !strings.Contains(gotBody, `"tags":"x,y"`) {
		t.Errorf("expected comma-joined tags string in body, got %s", gotBody)
	}
	if strings.Contains(gotBody, `"tags":[`) {
		t.Errorf("tags must not be a JSON array, got %s", gotBody)
	}
}

// ── Async operations ─────────────────────────────────────────────

func TestInsertAsync(t *testing.T) {
	var gotPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(AsyncJobResult{JobID: "j1", Status: "pending", PollURL: "/documents/jobs/j1"})
	})

	result, err := client.InsertAsync(context.Background(), []byte("data"), "test.txt", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.JobID != "j1" {
		t.Errorf("expected j1, got %s", result.JobID)
	}
	if gotPath != "/documents/async" {
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

// ── Entity scoping ──────────────────────────────────────

func TestInsertWithEntityID(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "e1"})
	})

	_, err := client.Insert(context.Background(), []byte("data"), "test.txt", "", WithEntityID("user_8472"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=user_8472") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
}

func TestInsertStreamWithEntityID(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "es1"})
	})

	_, err := client.InsertStream(context.Background(), strings.NewReader("data"), "f.txt", "text/plain", WithEntityID("acct_4821"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=acct_4821") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
}

func TestUpdateWithEntityID(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "abc-123", Version: 2})
	})

	_, err := client.Update(context.Background(), "abc-123", []byte("x"), "f.txt", "text/plain", WithEntityID("user_dana"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=user_dana") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
}

func TestDocumentRecordDecodesEntityID(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"doc_id":"d1","entity_id":"user_dana","content_type":"text/plain"}`))
	})

	doc, err := client.Get(context.Background(), "d1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.EntityID == nil || *doc.EntityID != "user_dana" {
		t.Errorf("expected entity_id 'user_dana', got %v", doc.EntityID)
	}
}

func TestSearchResultDecodesEntityID(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"q","results":[{"doc_id":"a","distance":0.1,"entity_id":"lena","content_type":"text/plain"}]}`))
	})

	results, err := client.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].EntityID == nil || *results[0].EntityID != "lena" {
		t.Errorf("expected entity_id 'lena', got %v", results[0].EntityID)
	}
}

func TestInsertWithEmbeddingsEntityID(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "ie1"})
	})

	_, err := client.InsertWithEmbeddings(context.Background(), "content", InsertWithEmbeddingsOptions{
		Embedding: []float32{0.1, 0.2},
		EntityID:  "user_4127",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"entity_id":"user_4127"`) {
		t.Errorf("expected entity_id in body, got %s", gotBody)
	}
}

// ── Search filters ──────────────────────────────────────

func TestSearchWithEntityIDAndWindow(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5,
		WithSearchEntityID("user_42"),
		WithSince("2026-06-01T00:00:00Z"),
		WithUntil("2026-06-08T00:00:00Z"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=user_42") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "since=2026-06-01") {
		t.Errorf("expected since in query, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "until=2026-06-08") {
		t.Errorf("expected until in query, got %s", gotQuery)
	}
}

func TestSearchWithLastNDays(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.Search(context.Background(), "test", 5, WithLastNDays(7))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "last_n_days=7") {
		t.Errorf("expected last_n_days=7 in query, got %s", gotQuery)
	}
}

func TestSearchByVectorFilters(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5,
		WithSearchEntityID("user_8472"),
		WithLastNDays(30),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"entity_id":"user_8472"`) {
		t.Errorf("expected entity_id in body, got %s", gotBody)
	}
	if !strings.Contains(gotBody, `"last_n_days":30`) {
		t.Errorf("expected last_n_days in body, got %s", gotBody)
	}
}

func TestListWithEntityAndWindow(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(struct {
			Documents []DocumentRecord `json:"documents"`
			Total     int              `json:"total"`
			HasMore   bool             `json:"has_more"`
		}{})
	})

	_, err := client.List(context.Background(), &ListOptions{
		EntityID: "user_dana",
		Until:    "2026-06-08T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_id=user_dana") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "until=2026-06-08") {
		t.Errorf("expected until in query, got %s", gotQuery)
	}
}

func TestListLastNDaysPrecedence(t *testing.T) {
	var gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(struct {
			Documents []DocumentRecord `json:"documents"`
			Total     int              `json:"total"`
			HasMore   bool             `json:"has_more"`
		}{})
	})

	// LastNDays must win over an explicit Since.
	_, err := client.List(context.Background(), &ListOptions{
		Since:     "2020-01-01T00:00:00Z",
		LastNDays: 14,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "last_n_days=14") {
		t.Errorf("expected last_n_days=14 (LastNDays precedence), got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "2020-01-01") {
		t.Errorf("expected explicit Since to be overridden, got %s", gotQuery)
	}
}

func TestBatchSearchPerQueryFilters(t *testing.T) {
	var gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"query": "test", "results": []map[string]any{}},
			},
		})
	})

	_, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{Q: "test", K: 5, EntityID: "user_42", Since: "2026-06-01T00:00:00Z", Until: "2026-06-08T00:00:00Z", LastNDays: 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"entity_id":"user_42"`) {
		t.Errorf("expected entity_id in body, got %s", gotBody)
	}
	if !strings.Contains(gotBody, `"since":"2026-06-01T00:00:00Z"`) {
		t.Errorf("expected since in body, got %s", gotBody)
	}
	if !strings.Contains(gotBody, `"until":"2026-06-08T00:00:00Z"`) {
		t.Errorf("expected until in body, got %s", gotBody)
	}
}

// ── Tag→entity backfill ─────────────────────────────────

func TestBackfillEntityFromTags(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scanned":           10,
			"updated":           4,
			"skipped_existing":  2,
			"skipped_no_match":  3,
			"skipped_ambiguous": 1,
			"skipped_invalid":   0,
		})
	})

	report, err := client.BackfillEntityFromTags(context.Background(), "user:", true)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/documents/backfill-entity" {
		t.Errorf("expected /documents/backfill-entity, got %s", gotPath)
	}
	if !strings.Contains(gotBody, `"tag_prefix":"user:"`) {
		t.Errorf("expected tag_prefix in body, got %s", gotBody)
	}
	if !strings.Contains(gotBody, `"overwrite":true`) {
		t.Errorf("expected overwrite in body, got %s", gotBody)
	}
	if report.Scanned != 10 || report.Updated != 4 || report.SkippedExisting != 2 ||
		report.SkippedNoMatch != 3 || report.SkippedAmbiguous != 1 || report.SkippedInvalid != 0 {
		t.Errorf("unexpected report: %+v", report)
	}
}

func TestBackfillEntityFromTagsEmptyPrefix(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(EntityBackfillReport{}))
	_, err := client.BackfillEntityFromTags(context.Background(), "", false)
	if err == nil || !strings.Contains(err.Error(), "tagPrefix cannot be empty") {
		t.Errorf("expected validation error, got %v", err)
	}
}

// suppress unused import warning
var _ = fmt.Sprintf
