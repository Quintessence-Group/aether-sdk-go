package aether

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// ── Usage-feedback capture: QueryID on hits + SendSearchFeedback ──

func strPtr(s string) *string { return &s }

func TestSearchStampsQueryIDOnEveryHit(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(searchResponse{
		Query:   "q",
		QueryID: strPtr("11111111-2222-3333-4444-555555555555"),
		Results: []SearchResult{
			{DocID: "doc-1", Score: 90, ContentType: "text/plain"},
			{DocID: "doc-2", Score: 80, ContentType: "text/plain"},
		},
	}))

	results, err := client.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range results {
		if r.QueryID == nil || *r.QueryID != "11111111-2222-3333-4444-555555555555" {
			t.Errorf("result %d: expected stamped QueryID, got %v", i, r.QueryID)
		}
	}
}

func TestSearchQueryIDNilWhenAbsent(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{
		"query": "q",
		"results": []map[string]any{
			{"doc_id": "doc-1", "score": 90, "content_type": "text/plain"},
		},
	}))

	results, err := client.Search(context.Background(), "q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].QueryID != nil {
		t.Errorf("expected nil QueryID when server omits query_id, got %q", *results[0].QueryID)
	}
}

func TestSearchByVectorStampsQueryID(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(searchResponse{
		QueryID: strPtr("qid-embed"),
		Results: []SearchResult{
			{DocID: "doc-1", Score: 90, ContentType: "text/plain"},
		},
	}))

	results, err := client.SearchByVector(context.Background(), []float32{0.1, 0.2}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].QueryID == nil || *results[0].QueryID != "qid-embed" {
		t.Errorf("expected stamped QueryID qid-embed, got %v", results[0].QueryID)
	}
}

func TestBatchSearchStampsPerQueryQueryID(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{
		"results": []map[string]any{
			{
				"query":    "a",
				"query_id": "qid-a",
				"results": []map[string]any{
					{"doc_id": "doc-1", "score": 90, "content_type": "text/plain"},
				},
			},
			{
				"query": "b",
				"results": []map[string]any{
					{"doc_id": "doc-2", "score": 80, "content_type": "text/plain"},
				},
			},
		},
	}))

	responses, err := client.BatchSearch(context.Background(), []BatchSearchQuery{
		{Q: "a"}, {Q: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 {
		t.Fatalf("expected 2 responses, got %d", len(responses))
	}
	got := responses[0].Results[0].QueryID
	if got == nil || *got != "qid-a" {
		t.Errorf("query a: expected stamped QueryID qid-a, got %v", got)
	}
	if responses[1].Results[0].QueryID != nil {
		t.Errorf("query b: expected nil QueryID, got %q", *responses[1].Results[0].QueryID)
	}
}

func TestSendSearchFeedback(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_ = json.NewEncoder(w).Encode(map[string]bool{"recorded": true})
	})

	err := client.SendSearchFeedback(context.Background(), "qid-1", "doc-1", "used")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/search/feedback" {
		t.Errorf("expected /v1/search/feedback, got %s", gotPath)
	}
	var sent searchFeedbackRequest
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("failed to decode sent body %q: %v", gotBody, err)
	}
	want := searchFeedbackRequest{QueryID: "qid-1", DocID: "doc-1", Signal: "used"}
	if sent != want {
		t.Errorf("expected body %+v, got %+v", want, sent)
	}
}

func TestSendSearchFeedback404OnUnknownQueryID(t *testing.T) {
	_, client := jsonServer(t, errorHandler(404, "unknown query_id"))

	err := client.SendSearchFeedback(context.Background(), "nope", "doc-1", "cited")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("expected 404, got %d", apiErr.StatusCode)
	}
}

func TestSendSearchFeedback400OnInvalidSignal(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid signal",
			"code":  "invalid_input",
		})
	})

	err := client.SendSearchFeedback(context.Background(), "qid-1", "doc-1", "loved")
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("expected 400, got %d", apiErr.StatusCode)
	}
	if apiErr.ErrorCode != "invalid_input" {
		t.Errorf("expected code invalid_input, got %q", apiErr.ErrorCode)
	}
}

func TestSendSearchFeedbackValidatesArguments(t *testing.T) {
	called := false
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	cases := []struct{ queryID, docID, signal string }{
		{"", "doc-1", "used"},
		{"qid-1", "", "used"},
		{"qid-1", "doc-1", ""},
	}
	for _, tc := range cases {
		err := client.SendSearchFeedback(context.Background(), tc.queryID, tc.docID, tc.signal)
		if err == nil {
			t.Errorf("expected validation error for %+v", tc)
		}
		if !strings.Contains(err.Error(), "cannot be empty") {
			t.Errorf("expected empty-argument error, got %s", err.Error())
		}
	}
	if called {
		t.Error("expected no HTTP call on client-side validation failure")
	}
}
