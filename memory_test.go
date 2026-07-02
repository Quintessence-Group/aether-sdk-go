package aether

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memoryServer spins up an httptest.Server with the given handler and returns a
// Memory wired to a Client pointed at it via the dependency-injection path
// (NewMemoryWithClient). This is the same transport layer the raw client tests
// mock — the real Client runs against the mock HTTP server.
func memoryServer(t *testing.T, entityID string, handler http.HandlerFunc, opts ...MemoryOption) (*httptest.Server, *Memory) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, err := NewMemoryWithClient(entityID, client, opts...)
	if err != nil {
		t.Fatalf("NewMemoryWithClient: %v", err)
	}
	return srv, mem
}

func fixedClock(ts string) func() time.Time {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return t.UTC() }
}

// ── §8 case 1 + 2 + 3: remember scoping, round-trip, metadata → tags ──────

func TestMemoryRememberScopingAndRoundTrip(t *testing.T) {
	var gotQuery, gotBody string
	createdAt := "2026-06-15T12:00:00Z"
	_, mem := memoryServer(t, "patient-john", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1", CreatedAt: &createdAt})
	})

	item, err := mem.Remember(context.Background(), "Anxious about flying", nil)
	if err != nil {
		t.Fatal(err)
	}
	// case 1: entity_id sent as the first-class field (query param on the insert).
	if !strings.Contains(gotQuery, "entity_id=patient-john") {
		t.Errorf("expected entity_id in query, got %s", gotQuery)
	}
	if gotBody != "Anxious about flying" {
		t.Errorf("expected body echo, got %q", gotBody)
	}
	// case 2: returns MemoryItem with id + created_at from the response.
	if item.ID != "mem-1" {
		t.Errorf("expected id mem-1, got %s", item.ID)
	}
	if item.CreatedAt == nil || *item.CreatedAt != createdAt {
		t.Errorf("expected created_at %q, got %v", createdAt, item.CreatedAt)
	}
	if item.EntityID == nil || *item.EntityID != "patient-john" {
		t.Errorf("expected entity_id patient-john, got %v", item.EntityID)
	}
	if item.Text != "Anxious about flying" {
		t.Errorf("expected text echo, got %q", item.Text)
	}
	if item.Score != nil {
		t.Errorf("expected nil score for remember, got %v", *item.Score)
	}
}

func TestMemoryRememberMetadataToTags(t *testing.T) {
	var gotQuery string
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{"topic": "anxiety"})
	if err != nil {
		t.Fatal(err)
	}
	// case 3: metadata encoded as key:value tag. url-encoded ':' is %3A.
	if !strings.Contains(gotQuery, "tags=topic%3Aanxiety") {
		t.Errorf("expected tag topic:anxiety in query, got %s", gotQuery)
	}
}

func TestMemoryRememberMultipleMetadataDeterministic(t *testing.T) {
	var gotTags string
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		gotTags = r.URL.Query().Get("tags")
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{
		"topic":    "anxiety",
		"severity": "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Keys are sorted for determinism: severity before topic.
	if gotTags != "severity:high,topic:anxiety" {
		t.Errorf("expected sorted comma-joined tags, got %q", gotTags)
	}
}

func TestMemoryRememberMetadataPrefixKeysSortedByKey(t *testing.T) {
	// Regression: sort KEYS, not the assembled "k:v" tag strings. With a prefix
	// key, key-sort gives "a:v,a0:w"; a tag-string sort would give "a0:w,a:v"
	// since '0' (0x30) < ':' (0x3A). Must match py/ts/.NET byte-for-byte.
	var gotTags string
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		gotTags = r.URL.Query().Get("tags")
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{
		"a0": "w",
		"a":  "v",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotTags != "a:v,a0:w" {
		t.Errorf("expected key-sorted tags a:v,a0:w, got %q", gotTags)
	}
}

func TestMemoryRememberMetadataCommaRejected(t *testing.T) {
	called := false
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{"topic": "a,b"})
	if err == nil {
		t.Fatal("expected client-side argument error for comma in value")
	}
	if !strings.Contains(err.Error(), "comma") {
		t.Errorf("expected comma error, got %v", err)
	}
	if called {
		t.Error("expected no HTTP call when metadata is invalid")
	}
}

func TestMemoryRememberMetadataEmptyKeyRejected(t *testing.T) {
	called := false
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{"": "value"})
	if err == nil {
		t.Fatal("expected client-side argument error for empty metadata key")
	}
	if called {
		t.Error("expected no HTTP call when metadata key is empty")
	}
}

func TestMemoryRememberMetadataCommaInKeyRejected(t *testing.T) {
	called := false
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{"a,b": "value"})
	if err == nil {
		t.Fatal("expected client-side argument error for comma in key")
	}
	if !strings.Contains(err.Error(), "comma") {
		t.Errorf("expected comma error, got %v", err)
	}
	if called {
		t.Error("expected no HTTP call when metadata key has a comma")
	}
}

func TestMemoryRememberMetadataColonInKeyRejected(t *testing.T) {
	called := false
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{"a:b": "value"})
	if err == nil {
		t.Fatal("expected client-side argument error for colon in key")
	}
	if !strings.Contains(err.Error(), "colon") {
		t.Errorf("expected colon error, got %v", err)
	}
	if called {
		t.Error("expected no HTTP call when metadata key has a colon")
	}
}

func TestMemoryRememberMetadataColonInValueAllowed(t *testing.T) {
	// A value may contain ':'; only the FIRST colon separates key from value.
	var gotTags string
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		gotTags = r.URL.Query().Get("tags")
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "text", map[string]string{"url": "https://x.io"})
	if err != nil {
		t.Fatal(err)
	}
	if gotTags != "url:https://x.io" {
		t.Errorf("expected value to retain colons, got %q", gotTags)
	}
}

func TestMemoryRememberEmptyTextRejected(t *testing.T) {
	called := false
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	_, err := mem.Remember(context.Background(), "   ", nil)
	if err == nil {
		t.Fatal("expected error for whitespace-only text")
	}
	if called {
		t.Error("expected no HTTP call for empty text")
	}
}

// ── §8 case 4: recall default (recency_weight = 0) ───────────────────────

func TestMemoryRecallDefault(t *testing.T) {
	var searchCalls, getCalls int
	var gotQuery string
	c1 := "doc 1 content"
	c2 := "doc 2 content"
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/search") {
			searchCalls++
			gotQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(searchResponse{
				Results: []SearchResult{
					{DocID: "d1", Score: 90, ContentType: "text/plain", Content: &c1},
					{DocID: "d2", Score: 60, ContentType: "text/plain", Content: &c2},
				},
			})
			return
		}
		getCalls++
		w.WriteHeader(500)
	})

	items, err := mem.Recall(context.Background(), "anxiety coping")
	if err != nil {
		t.Fatal(err)
	}
	// exactly one retrieve call, no get calls.
	if searchCalls != 1 {
		t.Errorf("expected exactly 1 search call, got %d", searchCalls)
	}
	if getCalls != 0 {
		t.Errorf("expected 0 get calls in default mode, got %d", getCalls)
	}
	// entity_id sent as the filter; default k=5 is forwarded.
	if !strings.Contains(gotQuery, "entity_id=user-1") {
		t.Errorf("expected entity_id filter, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "k=5") {
		t.Errorf("expected k=5, got %s", gotQuery)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// server order preserved; created_at is nil; score = wire score / 100.
	if items[0].ID != "d1" || items[1].ID != "d2" {
		t.Errorf("expected server order [d1 d2], got [%s %s]", items[0].ID, items[1].ID)
	}
	if items[0].CreatedAt != nil {
		t.Errorf("expected nil created_at in default mode, got %v", *items[0].CreatedAt)
	}
	if items[0].Score == nil {
		t.Fatal("expected score in default mode")
	}
	wantScore := 0.90
	if diff := *items[0].Score - wantScore; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected score %.6f, got %.6f", wantScore, *items[0].Score)
	}
	if items[0].Text != "doc 1 content" {
		t.Errorf("expected text from retrieve content, got %q", items[0].Text)
	}
}

func TestMemoryRecallForwardsTimeFilters(t *testing.T) {
	var gotQuery string
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})

	_, err := mem.Recall(context.Background(), "q",
		WithRecallSince("2026-01-01T00:00:00Z"),
		WithRecallUntil("2026-06-01T00:00:00Z"),
		WithRecallK(3),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"since=2026-01-01T00%3A00%3A00Z", "until=2026-06-01T00%3A00%3A00Z", "k=3"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
}

func TestMemoryRecallEmptyQueryRejected(t *testing.T) {
	for _, q := range []string{"", "   ", "\t\n"} {
		called := false
		_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
			called = true
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
		})
		_, err := mem.Recall(context.Background(), q)
		if err == nil {
			t.Errorf("expected error for empty/whitespace query %q", q)
		}
		if called {
			t.Errorf("expected no HTTP call for empty query %q", q)
		}
	}
}

func TestMemoryRecallKBelowOneRejected(t *testing.T) {
	for _, k := range []int{0, -1} {
		called := false
		_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
			called = true
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
		})
		_, err := mem.Recall(context.Background(), "q", WithRecallK(k))
		if err == nil {
			t.Errorf("expected error for k=%d", k)
		}
		if !strings.Contains(err.Error(), "k must be") {
			t.Errorf("expected k validation error for k=%d, got %v", k, err)
		}
		if called {
			t.Errorf("expected no HTTP call for k=%d", k)
		}
	}
}

// ── §8 case 5: recall recency (golden ordering) ──────────────────────────

func TestMemoryRecallRecencyGoldenOrdering(t *testing.T) {
	// Canonical recency golden vector — these exact inputs and this exact
	// asserted order are shared across all four SDKs so a per-language formula
	// regression is caught by one vector. now is fixed at 2026-06-15T00:00:00Z;
	// half-life is the default 30d; recency_weight = 0.5.
	//
	//   blended = 0.5*(score/100) + 0.5*0.5^(age_days/30)
	//   server order (descending score) -> [doc_id, score, created_at]:
	//     doc-e 95 null               -> blended 0.475000 (best score, but
	//                                     null created_at => recency 0 sinks it)
	//     doc-a 90 2026-01-01 (165d)  -> blended 0.461049
	//     doc-b 80 2026-06-14 (1d)    -> blended 0.888580 (freshest wins)
	//     doc-c 70 2026-06-10 (5d)    -> blended 0.795449
	//     doc-d 60 2026-05-16 (30d=1 half-life) -> blended 0.550000 (recency 0.5)
	//
	// Golden sorted order: [doc-b, doc-c, doc-d, doc-e, doc-a].
	contents := map[string]string{
		"doc-a": "doc a",
		"doc-b": "doc b",
		"doc-c": "doc c",
		"doc-d": "doc d",
		"doc-e": "doc e",
	}
	created := map[string]*string{
		"doc-e": nil,
		"doc-a": ptr("2026-01-01T00:00:00Z"),
		"doc-b": ptr("2026-06-14T00:00:00Z"),
		"doc-c": ptr("2026-06-10T00:00:00Z"),
		"doc-d": ptr("2026-05-16T00:00:00Z"),
	}

	var searchCalls int
	var mu sync.Mutex
	getCalls := map[string]int{}

	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/search") {
			mu.Lock()
			searchCalls++
			mu.Unlock()
			// Server order: descending score.
			results := []SearchResult{
				{DocID: "doc-e", Score: 95, ContentType: "text/plain", Content: ptr(contents["doc-e"])},
				{DocID: "doc-a", Score: 90, ContentType: "text/plain", Content: ptr(contents["doc-a"])},
				{DocID: "doc-b", Score: 80, ContentType: "text/plain", Content: ptr(contents["doc-b"])},
				{DocID: "doc-c", Score: 70, ContentType: "text/plain", Content: ptr(contents["doc-c"])},
				{DocID: "doc-d", Score: 60, ContentType: "text/plain", Content: ptr(contents["doc-d"])},
			}
			_ = json.NewEncoder(w).Encode(searchResponse{Results: results})
			return
		}
		// GET /documents/{id}
		id := strings.TrimPrefix(r.URL.Path, "/v1/documents/")
		mu.Lock()
		getCalls[id]++
		mu.Unlock()
		rec := DocumentRecord{DocID: id, ContentType: "text/plain", CreatedAt: created[id]}
		_ = json.NewEncoder(w).Encode(rec)
	}, WithClock(fixedClock("2026-06-15T00:00:00Z")))

	items, err := mem.Recall(context.Background(), "q",
		WithRecallK(5),
		WithRecencyWeight(0.5),
	)
	if err != nil {
		t.Fatal(err)
	}

	if searchCalls != 1 {
		t.Errorf("expected 1 search call, got %d", searchCalls)
	}
	// One Get per unique candidate doc id (N+1 calls).
	if len(getCalls) != 5 {
		t.Errorf("expected 5 unique Get calls, got %d (%v)", len(getCalls), getCalls)
	}

	// Asserted order: [doc-b, doc-c, doc-d, doc-e, doc-a].
	wantOrder := []string{"doc-b", "doc-c", "doc-d", "doc-e", "doc-a"}
	if len(items) != len(wantOrder) {
		t.Fatalf("expected %d items, got %d", len(wantOrder), len(items))
	}
	for i, want := range wantOrder {
		if items[i].ID != want {
			gotIDs := make([]string, len(items))
			for j, it := range items {
				gotIDs[j] = it.ID
			}
			t.Fatalf("expected order %v, got %v", wantOrder, gotIDs)
		}
	}

	// Blended scores, asserted within 1e-6 (golden vector).
	wantBlended := map[string]float64{
		"doc-b": 0.888580,
		"doc-c": 0.795449,
		"doc-d": 0.550000,
		"doc-e": 0.475000,
		"doc-a": 0.461049,
	}
	for _, it := range items {
		if it.Score == nil {
			t.Fatalf("expected blended score for %s", it.ID)
		}
		if diff := *it.Score - wantBlended[it.ID]; diff > 1e-6 || diff < -1e-6 {
			t.Errorf("%s: expected blended %.6f, got %.6f", it.ID, wantBlended[it.ID], *it.Score)
		}
	}

	// created_at is populated in recency mode (doc-e is null).
	if items[0].CreatedAt == nil || *items[0].CreatedAt != "2026-06-14T00:00:00Z" {
		t.Errorf("expected created_at on top result doc-b, got %v", items[0].CreatedAt)
	}
	if items[3].ID == "doc-e" && items[3].CreatedAt != nil {
		t.Errorf("expected nil created_at for doc-e, got %v", *items[3].CreatedAt)
	}
}

func TestMemoryRecallRecencyWeightClamped(t *testing.T) {
	// recency_weight > 1 clamps to 1 (pure recency). Newest wins regardless of
	// relevance score.
	created := map[string]*string{
		"a": ptr("2026-01-01T00:00:00Z"),
		"b": ptr("2026-06-14T00:00:00Z"),
	}
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/search") {
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{
				{DocID: "a", Score: 99, ContentType: "text/plain", Content: ptr("a")},
				{DocID: "b", Score: 10, ContentType: "text/plain", Content: ptr("b")},
			}})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/documents/")
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: id, CreatedAt: created[id]})
	}, WithClock(fixedClock("2026-06-15T00:00:00Z")))

	items, err := mem.Recall(context.Background(), "q", WithRecallK(2), WithRecencyWeight(5.0))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "b" {
		t.Errorf("expected newest doc 'b' first under pure recency, got %+v", items)
	}
}

func TestMemoryRecallRecencyEmpty(t *testing.T) {
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
	})
	items, err := mem.Recall(context.Background(), "q", WithRecencyWeight(0.5))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("expected empty result, got %d", len(items))
	}
}

// ── §8 case 6: list (newest-first, text downloaded, entity filter) ───────

func TestMemoryList(t *testing.T) {
	var gotListQuery string
	t1 := "2026-06-14T00:00:00Z"
	t2 := "2026-06-10T00:00:00Z"
	eid := "user-1"
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/documents" {
			gotListQuery = r.URL.RawQuery
			// Server returns newest-first for filtered listings.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": []DocumentRecord{
					{DocID: "newer", CreatedAt: &t1, EntityID: &eid, ContentType: "text/plain"},
					{DocID: "older", CreatedAt: &t2, EntityID: &eid, ContentType: "text/plain"},
				},
				"count": 2, "total": 2, "has_more": false,
			})
			return
		}
		// /documents/{id}/download
		switch {
		case strings.Contains(r.URL.Path, "/v1/documents/newer/download"):
			_, _ = w.Write([]byte("newer text"))
		case strings.Contains(r.URL.Path, "/v1/documents/older/download"):
			_, _ = w.Write([]byte("older text"))
		default:
			w.WriteHeader(404)
		}
	})

	items, err := mem.List(context.Background(), WithListLimit(50))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotListQuery, "entity_id=user-1") {
		t.Errorf("expected entity_id filter on list, got %s", gotListQuery)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// newest first.
	if items[0].ID != "newer" || items[1].ID != "older" {
		t.Errorf("expected [newer older], got [%s %s]", items[0].ID, items[1].ID)
	}
	// text populated from per-item download.
	if items[0].Text != "newer text" || items[1].Text != "older text" {
		t.Errorf("expected downloaded text, got %q / %q", items[0].Text, items[1].Text)
	}
	if items[0].CreatedAt == nil || *items[0].CreatedAt != t1 {
		t.Errorf("expected created_at populated, got %v", items[0].CreatedAt)
	}
	if items[0].Score != nil {
		t.Errorf("expected nil score for list, got %v", *items[0].Score)
	}
}

// ── §8 case 7: forget / forget_all ───────────────────────────────────────

func TestMemoryForget(t *testing.T) {
	var gotMethod, gotPath string
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "tombstoned"})
	})

	if err := mem.Forget(context.Background(), "mem-1"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/v1/documents/mem-1" {
		t.Errorf("expected /documents/mem-1, got %s", gotPath)
	}
}

func TestMemoryForgetEmptyID(t *testing.T) {
	called := false
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	if err := mem.Forget(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty id")
	}
	if called {
		t.Error("expected no HTTP call for empty id")
	}
}

func TestMemoryForgetAll(t *testing.T) {
	var mu sync.Mutex
	listCalls := 0
	deleted := map[string]bool{}
	var gotListQuery string

	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/documents" && r.Method == http.MethodGet {
			listCalls++
			gotListQuery = r.URL.RawQuery
			// First listing: 3 docs. Second listing (after all deleted): empty.
			var docs []DocumentRecord
			for _, id := range []string{"a", "b", "c"} {
				if !deleted[id] {
					docs = append(docs, DocumentRecord{DocID: id})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": docs, "count": len(docs), "total": len(docs), "has_more": false,
			})
			return
		}
		if r.Method == http.MethodDelete {
			id := strings.TrimPrefix(r.URL.Path, "/v1/documents/")
			deleted[id] = true
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "tombstoned"})
			return
		}
		w.WriteHeader(404)
	})

	n, err := mem.ForgetAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("expected 3 deleted, got %d", n)
	}
	if !strings.Contains(gotListQuery, "entity_id=user-1") {
		t.Errorf("expected entity_id filter on forget-all list, got %s", gotListQuery)
	}
	// Should loop until the listing is empty: one populated + one empty page.
	if listCalls < 2 {
		t.Errorf("expected at least 2 list calls (loop to empty), got %d", listCalls)
	}
	if !deleted["a"] || !deleted["b"] || !deleted["c"] {
		t.Errorf("expected all three deleted, got %v", deleted)
	}
}

// ── §8 case 8: error passthrough (typed error, no wrapping) ──────────────

func TestMemoryErrorPassthrough(t *testing.T) {
	// 402 credit_exhausted must surface as the same typed error the raw client
	// raises — matchable via errors.Is(err, ErrCreditExhausted).
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(402)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "prepaid credit exhausted",
			"code":  CodeCreditExhausted,
		})
	})

	_, err := mem.Remember(context.Background(), "text", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.StatusCode != 402 {
		t.Errorf("expected 402, got %d", apiErr.StatusCode)
	}
	if !errors.Is(err, ErrCreditExhausted) {
		t.Errorf("expected errors.Is(err, ErrCreditExhausted) to hold")
	}
}

func TestMemoryRecallErrorPassthrough(t *testing.T) {
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "tenant paused",
			"code":  CodeTenantPaused,
		})
	})
	_, err := mem.Recall(context.Background(), "q")
	if !errors.Is(err, ErrTenantPaused) {
		t.Errorf("expected ErrTenantPaused, got %v", err)
	}
}

// ── §8 case 9: invalid construction ──────────────────────────────────────

func TestMemoryInvalidConstruction(t *testing.T) {
	client := NewClient("http://localhost:9000")

	if _, err := NewMemoryWithClient("", client); err == nil {
		t.Error("expected error for empty entity id")
	}

	oversized := strings.Repeat("x", 257)
	if _, err := NewMemoryWithClient(oversized, client); err == nil {
		t.Error("expected error for oversized entity id (>256)")
	}

	// Exactly 256 is valid.
	maxOK := strings.Repeat("x", 256)
	if _, err := NewMemoryWithClient(maxOK, client); err != nil {
		t.Errorf("expected 256-char entity id to be valid, got %v", err)
	}

	// NewMemory (convenience path) validates without a network round-trip.
	if _, err := NewMemory(""); err == nil {
		t.Error("expected NewMemory to reject empty entity id")
	}

	// Nil client is rejected.
	if _, err := NewMemoryWithClient("user-1", nil); err == nil {
		t.Error("expected error for nil client")
	}
}

func TestMemoryWhitespaceOnlyEntityIDRejected(t *testing.T) {
	// Regression: a whitespace-only entity id must be rejected client-side, just
	// like the empty string. If it leaked through, the raw layer would drop the
	// blank entity_id and every call would silently scope tenant-wide. No HTTP
	// call may be made (validation happens before any client is even built).
	for _, eid := range []string{"   ", "\t", "\n", " \t\n "} {
		if _, err := NewMemory(eid); err == nil {
			t.Errorf("expected NewMemory(%q) to reject whitespace-only entity id", eid)
		}

		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		client := NewClient(srv.URL)
		if _, err := NewMemoryWithClient(eid, client); err == nil {
			t.Errorf("expected NewMemoryWithClient(%q) to reject whitespace-only entity id", eid)
		}
		srv.Close()
		if called {
			t.Errorf("expected no HTTP call for whitespace-only entity id %q", eid)
		}
	}
}

// ── Options & helpers ────────────────────────────────────────────────────

func TestMemoryWithHalfLife(t *testing.T) {
	client := NewClient("http://localhost:9000")
	mem, err := NewMemoryWithClient("user-1", client, WithHalfLife(60*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if mem.halfLifeDays != 60 {
		t.Errorf("expected half-life 60 days, got %f", mem.halfLifeDays)
	}
}

func TestMemoryFactExtractionSendsFlag(t *testing.T) {
	// With extract_facts enabled, Remember issues exactly one insert call that
	// carries extract_facts=true (the fan-out into fact siblings happens
	// server-side) and returns the raw memory, not the facts.
	calls := 0
	gotExtract := ""
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotExtract = r.URL.Query().Get("extract_facts")
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	}, WithFactExtraction(true))

	item, err := mem.Remember(context.Background(), "fact one. fact two. fact three.", nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 insert call, got %d", calls)
	}
	if gotExtract != "true" {
		t.Errorf("expected extract_facts=true on the insert request, got %q", gotExtract)
	}
	if item.Text != "fact one. fact two. fact three." {
		t.Errorf("expected the raw text returned as one memory, got %q", item.Text)
	}
}

func TestMemoryFactExtractionDefaultOff(t *testing.T) {
	// Without WithFactExtraction, Remember must not request extraction — the
	// insert carries no extract_facts parameter at all.
	sawExtract := false
	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		_, sawExtract = r.URL.Query()["extract_facts"]
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
	})

	if _, err := mem.Remember(context.Background(), "plain memory", nil); err != nil {
		t.Fatal(err)
	}
	if sawExtract {
		t.Errorf("expected no extract_facts parameter on the default insert request")
	}
}

func TestRecencyScoreFormula(t *testing.T) {
	now := mustTime("2026-06-15T00:00:00Z")

	// At exactly one half-life the recency contribution is 0.5.
	oneHalfLifeAgo := "2026-05-16T00:00:00Z" // 30 days before now
	if got := recencyScore(&oneHalfLifeAgo, now, 30.0); got-0.5 > 1e-9 || got-0.5 < -1e-9 {
		t.Errorf("expected 0.5 at one half-life, got %f", got)
	}

	// Nil timestamp scores 0.
	if got := recencyScore(nil, now, 30.0); got != 0.0 {
		t.Errorf("expected 0 for nil timestamp, got %f", got)
	}

	// Unparseable timestamp scores 0.
	bad := "not-a-timestamp"
	if got := recencyScore(&bad, now, 30.0); got != 0.0 {
		t.Errorf("expected 0 for unparseable timestamp, got %f", got)
	}

	// Future timestamp clamps age to 0 -> score 1.0.
	future := "2027-01-01T00:00:00Z"
	if got := recencyScore(&future, now, 30.0); got != 1.0 {
		t.Errorf("expected 1.0 for future timestamp, got %f", got)
	}

	// now itself -> age 0 -> 1.0.
	nowTs := "2026-06-15T00:00:00Z"
	if got := recencyScore(&nowTs, now, 30.0); got != 1.0 {
		t.Errorf("expected 1.0 for now, got %f", got)
	}
}

func TestParseRFC3339(t *testing.T) {
	cases := []string{
		"2026-06-15T12:30:00Z",
		"2026-06-15T12:30:00.123456789Z",
		"2026-06-15T12:30:00+02:00",
		"2026-06-15T12:30:00", // naive -> UTC
		"2026-06-15",          // date only
	}
	for _, c := range cases {
		if _, ok := parseRFC3339(c); !ok {
			t.Errorf("expected to parse %q", c)
		}
	}
	if _, ok := parseRFC3339("garbage"); ok {
		t.Error("expected parse failure for garbage")
	}

	// Trailing Z and explicit +00:00 must resolve to the same instant.
	z, _ := parseRFC3339("2026-06-15T12:30:00Z")
	off, _ := parseRFC3339("2026-06-15T12:30:00+00:00")
	if !z.Equal(off) {
		t.Errorf("expected Z and +00:00 to be equal instants")
	}
}

// ── Facade AC: existing raw-API users see no behavior change ─────────────

// TestMemoryRawAPIUnaffected drives the raw Client (insert/search/list/delete)
// directly through the same mock transport the Memory tests use, asserting the
// raw surface behaves normally and is untouched by the Memory facade. Memory is
// a thin composition over Client (it adds no routes and changes no behavior), so
// a raw-client caller must see identical behavior — the facade's core AC.
func TestMemoryRawAPIUnaffected(t *testing.T) {
	created := "2026-06-15T12:00:00Z"
	content := "raw content"
	var deletedPath, deletedMethod string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/documents" && r.Method == http.MethodPost:
			// raw InsertText
			_ = json.NewEncoder(w).Encode(DocumentRecord{
				DocID: "raw-1", ContentType: "text/plain", CreatedAt: &created,
			})
		case strings.HasPrefix(r.URL.Path, "/v1/search"):
			// raw Search
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{
				{DocID: "raw-1", Score: 88, ContentType: "text/plain", Content: &content},
			}})
		case r.URL.Path == "/v1/documents" && r.Method == http.MethodGet:
			// raw List
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": []DocumentRecord{
					{DocID: "raw-1", ContentType: "text/plain", CreatedAt: &created},
				},
				"count": 1, "total": 1, "has_more": false,
			})
		case r.Method == http.MethodDelete:
			deletedMethod = r.Method
			deletedPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "tombstoned"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	// Construct the raw Client directly — no Memory involved.
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	ctx := context.Background()

	// insert_text → DocumentRecord, unchanged.
	doc, err := client.InsertText(ctx, "raw content", "")
	if err != nil {
		t.Fatalf("raw InsertText: %v", err)
	}
	if doc.DocID != "raw-1" {
		t.Errorf("expected raw doc id raw-1, got %s", doc.DocID)
	}
	if doc.CreatedAt == nil || *doc.CreatedAt != created {
		t.Errorf("expected raw created_at %q, got %v", created, doc.CreatedAt)
	}

	// search → []SearchResult, unchanged (raw read model carries no tags field).
	hits, err := client.Search(ctx, "query", 5)
	if err != nil {
		t.Fatalf("raw Search: %v", err)
	}
	if len(hits) != 1 || hits[0].DocID != "raw-1" {
		t.Fatalf("expected one raw hit raw-1, got %+v", hits)
	}
	if hits[0].Score != 88 {
		t.Errorf("expected raw score 88, got %v", hits[0].Score)
	}

	// list → *ListResult, unchanged.
	res, err := client.List(ctx, &ListOptions{Limit: 50})
	if err != nil {
		t.Fatalf("raw List: %v", err)
	}
	if len(res.Documents) != 1 || res.Documents[0].DocID != "raw-1" {
		t.Fatalf("expected one raw document raw-1, got %+v", res.Documents)
	}

	// delete → void, unchanged.
	if err := client.Delete(ctx, "raw-1"); err != nil {
		t.Fatalf("raw Delete: %v", err)
	}
	if deletedMethod != http.MethodDelete || deletedPath != "/v1/documents/raw-1" {
		t.Errorf("expected DELETE /documents/raw-1, got %s %s", deletedMethod, deletedPath)
	}

	// Raw client-side validation is also unchanged (empty query/id rejected).
	if _, err := client.Search(ctx, "", 5); err == nil {
		t.Error("expected raw Search to reject empty query")
	}
	if err := client.Delete(ctx, ""); err == nil {
		t.Error("expected raw Delete to reject empty id")
	}
}

// ── Write-only tags: reads carry no metadata (documented v1 limitation) ──

// TestMemoryReadsHaveEmptyMetadata locks the documented write-only-tags
// limitation: Remember writes metadata as searchable
// key:value tags, but the raw read models echo no tags, so MemoryItem carries no
// metadata field and read paths (Recall, List) cannot surface it. The mock
// deliberately omits tags on every read response (mirroring the real server);
// the returned items expose only id/text/created_at/entity_id/score, with
// entity_id being the Memory's own id — never any remembered metadata.
func TestMemoryReadsHaveEmptyMetadata(t *testing.T) {
	eid := "user-1"
	created := "2026-06-14T00:00:00Z"

	_, mem := memoryServer(t, "user-1", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/documents" && r.Method == http.MethodPost:
			// Remember: write succeeds; tags are accepted but never echoed back.
			_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "m1", CreatedAt: &created})
		case strings.HasPrefix(r.URL.Path, "/v1/search"):
			// Recall: search results have no tags field at all.
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{
				{DocID: "m1", Score: 90, ContentType: "text/plain", Content: ptr("hello")},
			}})
		case r.URL.Path == "/v1/documents" && r.Method == http.MethodGet:
			// List: document records have entity_id + created_at but no tags.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": []DocumentRecord{
					{DocID: "m1", CreatedAt: &created, EntityID: &eid, ContentType: "text/plain"},
				},
				"count": 1, "total": 1, "has_more": false,
			})
		case strings.Contains(r.URL.Path, "/v1/documents/m1/download"):
			_, _ = w.Write([]byte("hello"))
		case strings.HasPrefix(r.URL.Path, "/v1/documents/m1"):
			// Get (recency mode created_at resolution): no tags echoed.
			_ = json.NewEncoder(w).Encode(DocumentRecord{
				DocID: "m1", CreatedAt: &created, EntityID: &eid, ContentType: "text/plain",
			})
		default:
			w.WriteHeader(404)
		}
	}, WithClock(fixedClock("2026-06-15T00:00:00Z")))

	ctx := context.Background()

	// Write a memory WITH metadata.
	if _, err := mem.Remember(ctx, "hello", map[string]string{"topic": "anxiety"}); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	// MemoryItem has no metadata field by design — assert the read paths return
	// items populated only with id/text/created_at/entity_id/score and never
	// surface the remembered "topic:anxiety" tag.
	assertNoMetadata := func(label string, items []MemoryItem) {
		if len(items) != 1 {
			t.Fatalf("%s: expected 1 item, got %d", label, len(items))
		}
		it := items[0]
		if it.ID != "m1" {
			t.Errorf("%s: expected id m1, got %s", label, it.ID)
		}
		// entity_id is the Memory's own id, NOT remembered metadata.
		if it.EntityID == nil || *it.EntityID != "user-1" {
			t.Errorf("%s: expected entity_id user-1, got %v", label, it.EntityID)
		}
		// The remembered tag must not leak into any string field of the item.
		for fld, v := range map[string]string{"id": it.ID, "text": it.Text} {
			if strings.Contains(v, "topic") || strings.Contains(v, "anxiety") {
				t.Errorf("%s: remembered metadata leaked into %s field: %q", label, fld, v)
			}
		}
	}

	// Recall (recency mode → resolves created_at, exercises the read-back path).
	recalled, err := mem.Recall(ctx, "q", WithRecencyWeight(0.5))
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	assertNoMetadata("recall", recalled)

	// List.
	listed, err := mem.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertNoMetadata("list", listed)
}

// TestMemoryOnPartitionHandleScopesBoth verifies that a Memory built on a
// partition handle (via dependency injection) is automatically scoped to BOTH
// the partition and the entity — Remember and Recall send both on the wire.
func TestMemoryOnPartitionHandleScopesBoth(t *testing.T) {
	var rememberQuery, recallQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/documents" && r.Method == http.MethodPost:
			rememberQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "mem-1"})
		case r.URL.Path == "/v1/search":
			recallQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(searchResponse{Results: []SearchResult{}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	scoped, err := client.Partition("tenant-x")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	mem, err := NewMemoryWithClient("patient-john", scoped)
	if err != nil {
		t.Fatalf("NewMemoryWithClient: %v", err)
	}

	if _, err := mem.Remember(context.Background(), "Anxious about flying", nil); err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if !strings.Contains(rememberQuery, "partition=tenant-x") {
		t.Errorf("remember: expected partition=tenant-x, got %s", rememberQuery)
	}
	if !strings.Contains(rememberQuery, "entity_id=patient-john") {
		t.Errorf("remember: expected entity_id=patient-john, got %s", rememberQuery)
	}

	if _, err := mem.Recall(context.Background(), "anxiety"); err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !strings.Contains(recallQuery, "partition=tenant-x") {
		t.Errorf("recall: expected partition=tenant-x, got %s", recallQuery)
	}
	if !strings.Contains(recallQuery, "entity_id=patient-john") {
		t.Errorf("recall: expected entity_id=patient-john, got %s", recallQuery)
	}
}

func ptr[T any](v T) *T { return &v }

func mustTime(ts string) time.Time {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}
