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

// fixedNow pins the recency clock for deterministic blended-ranking assertions.
var fixedNow = time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

// recorder is an httptest handler that scripts responses keyed by (method,
// path-prefix) and records every request it receives. It pins the facade to the
// shipped 0.3.x surface: search hits carry a calibrated 0–100 score + a passage
// (no distance, no inline content), and Recall fetches each matched document's
// text with a follow-up GET /documents/{id}/download.
type recorder struct {
	t  *testing.T
	mu sync.Mutex
	// calls records (METHOD, path) for every request, in order.
	calls []call
}

type call struct {
	method string
	path   string
	query  string
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{req.Method, req.URL.Path, req.URL.RawQuery})
}

func (r *recorder) count(method, path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.method == method && c.path == path {
			n++
		}
	}
	return n
}

func (r *recorder) firstQuery(method, path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.method == method && c.path == path {
			return c.query
		}
	}
	return ""
}

// hit is a single 0.3.x search result (score 0–100, higher = better; passage,
// no distance, no content).
type hit struct {
	doc   string
	score int
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func searchResults(hits ...hit) []SearchResult {
	out := make([]SearchResult, 0, len(hits))
	for _, h := range hits {
		p := "p"
		out = append(out, SearchResult{
			DocID:       h.doc,
			Score:       h.score,
			ContentType: "text/plain",
			Passage:     &p,
		})
	}
	return out
}

// memServer builds an httptest server with scripted responses for the routes a
// Memory call touches, plus a Memory wired to it through the DI path with a fixed
// clock. The handlers slice is consulted in order; the first whose match func
// returns true handles the request.
type route struct {
	match  func(*http.Request) bool
	handle http.HandlerFunc
}

func memServer(t *testing.T, routes []route, opts ...MemoryOption) (*recorder, *Memory) {
	t.Helper()
	rec := &recorder{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		for _, rt := range routes {
			if rt.match(r) {
				rt.handle(w, r)
				return
			}
		}
		t.Errorf("unexpected request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := NewClient(srv.URL, WithMaxRetries(0))
	allOpts := append([]MemoryOption{WithClock(func() time.Time { return fixedNow })}, opts...)
	mem, err := NewMemoryWithClient("user-42", client, allOpts...)
	if err != nil {
		t.Fatalf("NewMemoryWithClient: %v", err)
	}
	return rec, mem
}

func isSearch(r *http.Request) bool { return r.Method == "GET" && r.URL.Path == "/search" }
func isInsert(r *http.Request) bool { return r.Method == "POST" && r.URL.Path == "/documents" }
func isList(r *http.Request) bool   { return r.Method == "GET" && r.URL.Path == "/documents" }
func isDownload(r *http.Request) bool {
	return r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/download")
}
func isGetDoc(r *http.Request) bool {
	return r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/documents/") &&
		!strings.HasSuffix(r.URL.Path, "/download")
}
func isDelete(r *http.Request) bool {
	return r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/documents/")
}

func insertRoute(docID, createdAt string) route {
	return route{isInsert, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, DocumentRecord{
			DocID: docID, CID: "cid-1", Chunks: 1, Vectors: 1, Version: 1,
			CreatedAt: &createdAt, EntityID: strPtr("user-42"),
		})
	}}
}

func searchRoute(results []SearchResult) route {
	return route{isSearch, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, searchResponse{Query: "q", Results: results})
	}}
}

// downloadRoute maps doc_id -> body for GET /documents/{id}/download.
func downloadRoute(bodies map[string]string) route {
	return route{isDownload, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/documents/"), "/download")
		if body, ok := bodies[id]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}}
}

// getDocRoute maps doc_id -> created_at for GET /documents/{id} (metadata).
func getDocRoute(createdAt map[string]*string) route {
	return route{isGetDoc, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/documents/")
		ca := createdAt[id]
		writeJSON(w, DocumentRecord{
			DocID: id, CID: "c", ContentType: "text/plain",
			SizeBytes: 1, Version: 1, CreatedAt: ca, EntityID: strPtr("user-42"),
		})
	}}
}

func strPtr(s string) *string { return &s }

// ── scoping ──────────────────────────────────────────────────────────

func TestMemory_RememberSendsEntityID(t *testing.T) {
	rec, mem := memServer(t, []route{insertRoute("doc-new", "2026-06-15T00:00:00Z")})
	if _, err := mem.Remember(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	q := rec.firstQuery("POST", "/documents")
	if !strings.Contains(q, "entity_id=user-42") {
		t.Errorf("expected entity_id=user-42 in query, got %q", q)
	}
}

func TestMemory_RecallSendsEntityIDFilter(t *testing.T) {
	rec, mem := memServer(t, []route{searchRoute(nil)})
	if _, err := mem.Recall(context.Background(), "anxiety"); err != nil {
		t.Fatal(err)
	}
	q := rec.firstQuery("GET", "/search")
	if !strings.Contains(q, "entity_id=user-42") {
		t.Errorf("expected entity_id=user-42 in search query, got %q", q)
	}
}

func TestMemory_ListSendsEntityIDFilter(t *testing.T) {
	rec, mem := memServer(t, []route{
		{isList, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"documents": []any{}, "total": 0, "has_more": false})
		}},
	})
	if _, err := mem.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	q := rec.firstQuery("GET", "/documents")
	if !strings.Contains(q, "entity_id=user-42") {
		t.Errorf("expected entity_id=user-42 in list query, got %q", q)
	}
}

// ── remember round-trip ──────────────────────────────────────────────

func TestMemory_RememberReturnsItem(t *testing.T) {
	_, mem := memServer(t, []route{insertRoute("doc-7", "2026-06-15T09:30:00Z")})
	item, err := mem.Remember(context.Background(), "anxious about flying", nil)
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "doc-7" || item.Text != "anxious about flying" {
		t.Errorf("unexpected item: %+v", item)
	}
	if item.EntityID == nil || *item.EntityID != "user-42" {
		t.Errorf("expected entity_id user-42, got %v", item.EntityID)
	}
	if item.Score != nil {
		t.Errorf("expected nil score on remember, got %v", *item.Score)
	}
	if item.CreatedAt == nil || *item.CreatedAt != "2026-06-15T09:30:00Z" {
		t.Errorf("unexpected created_at: %v", item.CreatedAt)
	}
}

func TestMemory_RememberEmptyTextNoHTTP(t *testing.T) {
	rec, mem := memServer(t, nil)
	if _, err := mem.Remember(context.Background(), "   ", nil); err == nil {
		t.Fatal("expected error for empty text")
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected no HTTP calls, got %d", len(rec.calls))
	}
}

// ── metadata → tags (write-only) ─────────────────────────────────────

func TestMemory_MetadataEncodedAsTags(t *testing.T) {
	rec, mem := memServer(t, []route{insertRoute("d", "2026-06-15T00:00:00Z")})
	if _, err := mem.Remember(context.Background(), "breathing helps", map[string]string{"topic": "anxiety"}); err != nil {
		t.Fatal(err)
	}
	q := rec.firstQuery("POST", "/documents")
	if !strings.Contains(q, "tags=topic%3Aanxiety") {
		t.Errorf("expected tags=topic%%3Aanxiety, got %q", q)
	}
}

func TestMemory_MultipleMetadataSortedByKey(t *testing.T) {
	rec, mem := memServer(t, []route{insertRoute("d", "2026-06-15T00:00:00Z")})
	_, err := mem.Remember(context.Background(), "x", map[string]string{"topic": "anxiety", "score": "5", "active": "yes"})
	if err != nil {
		t.Fatal(err)
	}
	q := rec.firstQuery("POST", "/documents")
	// active:yes,score:5,topic:anxiety  (url.Values encodes ':' as %3A, ',' as %2C)
	if !strings.Contains(q, "tags=active%3Ayes%2Cscore%3A5%2Ctopic%3Aanxiety") {
		t.Errorf("expected sorted-by-key tags, got %q", q)
	}
}

func TestMemory_ValueWithColonSplitsOnFirst(t *testing.T) {
	rec, mem := memServer(t, []route{insertRoute("d", "2026-06-15T00:00:00Z")})
	if _, err := mem.Remember(context.Background(), "x", map[string]string{"time": "12:30"}); err != nil {
		t.Fatal(err)
	}
	q := rec.firstQuery("POST", "/documents")
	if !strings.Contains(q, "tags=time%3A12%3A30") {
		t.Errorf("expected tags=time%%3A12%%3A30, got %q", q)
	}
}

func TestMemory_BadMetadataNoHTTP(t *testing.T) {
	cases := []map[string]string{
		{"topic": "a,b"}, {"": "v"}, {"a,b": "v"}, {"a:b": "v"},
	}
	for _, md := range cases {
		rec, mem := memServer(t, nil)
		if _, err := mem.Remember(context.Background(), "x", md); err == nil {
			t.Errorf("expected error for metadata %v", md)
		}
		if len(rec.calls) != 0 {
			t.Errorf("expected no HTTP for metadata %v, got %d calls", md, len(rec.calls))
		}
	}
}

// ── recall (default: recency_weight=0) ───────────────────────────────

func TestMemory_RecallSearchThenDownloadServerOrder(t *testing.T) {
	rec, mem := memServer(t, []route{
		searchRoute(searchResults(hit{"d1", 95}, hit{"d2", 70})),
		downloadRoute(map[string]string{"d1": "first", "d2": "second"}),
	})
	items, err := mem.Recall(context.Background(), "query", WithRecallK(5))
	if err != nil {
		t.Fatal(err)
	}
	// one search + one download per unique hit; NO metadata GET calls
	if rec.count("GET", "/search") != 1 {
		t.Errorf("expected 1 search call, got %d", rec.count("GET", "/search"))
	}
	if rec.count("GET", "/documents/d1/download") != 1 {
		t.Errorf("expected 1 download for d1")
	}
	if rec.count("GET", "/documents/d2/download") != 1 {
		t.Errorf("expected 1 download for d2")
	}
	if rec.count("GET", "/documents/d1") != 0 {
		t.Errorf("default recall should not GET metadata")
	}
	if got := []string{items[0].ID, items[1].ID}; got[0] != "d1" || got[1] != "d2" {
		t.Errorf("expected server order [d1 d2], got %v", got)
	}
	if items[0].Text != "first" || items[1].Text != "second" {
		t.Errorf("unexpected texts: %q %q", items[0].Text, items[1].Text)
	}
	for _, it := range items {
		if it.CreatedAt != nil {
			t.Errorf("default recall should leave created_at nil, got %v", *it.CreatedAt)
		}
	}
	// score normalized from the 0–100 wire score; higher = better
	if *items[0].Score != 0.95 {
		t.Errorf("expected score 0.95, got %v", *items[0].Score)
	}
	if *items[1].Score != 0.70 {
		t.Errorf("expected score 0.70, got %v", *items[1].Score)
	}
	// entity filter + k forwarded
	q := rec.firstQuery("GET", "/search")
	if !strings.Contains(q, "entity_id=user-42") || !strings.Contains(q, "k=5") {
		t.Errorf("expected entity_id + k in search query, got %q", q)
	}
}

func TestMemory_RecallEmptyQueryNoHTTP(t *testing.T) {
	rec, mem := memServer(t, nil)
	if _, err := mem.Recall(context.Background(), "   "); err == nil {
		t.Fatal("expected error for empty query")
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected no HTTP, got %d calls", len(rec.calls))
	}
}

func TestMemory_RecallKBelowOneNoHTTP(t *testing.T) {
	rec, mem := memServer(t, nil)
	if _, err := mem.Recall(context.Background(), "query", WithRecallK(0)); err == nil {
		t.Fatal("expected error for k<1")
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected no HTTP, got %d calls", len(rec.calls))
	}
}

// ── recall (recency_weight>0: blended re-ranking) ────────────────────
//
// recency_weight=0.5, half_life=30d, now=2026-06-15. similarity = score/100,
// recency = 0.5 ** (age_days / 30). blended = 0.5*sim + 0.5*recency:
//
//	docA score=90 age=0d   -> 0.5*0.90 + 0.5*1.0 = 0.95
//	docB score=80 age=30d  -> 0.5*0.80 + 0.5*0.5 = 0.65
//	docC score=100 created=null (recency 0) -> 0.5*1.00 + 0.5*0.0 = 0.50
//
// Pure score order is [docC, docA, docB]; recency reorders to [docA, docB, docC].
func recencyRoutes() []route {
	return []route{
		searchRoute(searchResults(hit{"docA", 90}, hit{"docB", 80}, hit{"docC", 100})),
		downloadRoute(map[string]string{"docA": "A", "docB": "B", "docC": "C"}),
		getDocRoute(map[string]*string{
			"docA": strPtr("2026-06-15T00:00:00Z"),
			"docB": strPtr("2026-05-16T00:00:00Z"),
			"docC": nil,
		}),
	}
}

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func TestMemory_RecallBlendedReorder(t *testing.T) {
	_, mem := memServer(t, recencyRoutes())
	items, err := mem.Recall(context.Background(), "q", WithRecallK(5), WithRecencyWeight(0.5))
	if err != nil {
		t.Fatal(err)
	}
	gotIDs := []string{items[0].ID, items[1].ID, items[2].ID}
	if gotIDs[0] != "docA" || gotIDs[1] != "docB" || gotIDs[2] != "docC" {
		t.Errorf("expected [docA docB docC], got %v", gotIDs)
	}
	want := []float64{0.95, 0.65, 0.50}
	for i, w := range want {
		if !approx(*items[i].Score, w) {
			t.Errorf("item %d: expected blended %v, got %v", i, w, *items[i].Score)
		}
	}
	// recency mode resolves created_at, so it is populated
	if items[0].CreatedAt == nil || *items[0].CreatedAt != "2026-06-15T00:00:00Z" {
		t.Errorf("expected docA created_at populated, got %v", items[0].CreatedAt)
	}
}

func TestMemory_RecallTopKTruncation(t *testing.T) {
	_, mem := memServer(t, recencyRoutes())
	items, err := mem.Recall(context.Background(), "q", WithRecallK(2), WithRecencyWeight(0.5))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "docA" || items[1].ID != "docB" {
		t.Errorf("expected top-2 [docA docB], got %+v", items)
	}
}

// ── list (chronological) ─────────────────────────────────────────────

func TestMemory_ListNewestFirstTextDownloadedScoreNil(t *testing.T) {
	_, mem := memServer(t, []route{
		{isList, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"documents": []DocumentRecord{
					{DocID: "m1", ContentType: "text/plain", CreatedAt: strPtr("2026-06-15T00:00:00Z")},
					{DocID: "m2", ContentType: "text/plain", CreatedAt: strPtr("2026-06-01T00:00:00Z")},
				},
				"total": 2, "has_more": false,
			})
		}},
		downloadRoute(map[string]string{"m1": "newest", "m2": "older"}),
	})
	items, err := mem.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "m1" || items[1].ID != "m2" {
		t.Errorf("expected [m1 m2], got %+v", items)
	}
	if items[0].Text != "newest" || items[1].Text != "older" {
		t.Errorf("unexpected texts: %q %q", items[0].Text, items[1].Text)
	}
	for _, it := range items {
		if it.Score != nil {
			t.Errorf("list items should have nil score, got %v", *it.Score)
		}
	}
}

// ── forget ───────────────────────────────────────────────────────────

func TestMemory_ForgetIssuesOneDelete(t *testing.T) {
	rec, mem := memServer(t, []route{
		{isDelete, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) }},
	})
	if err := mem.Forget(context.Background(), "doc-x"); err != nil {
		t.Fatal(err)
	}
	if rec.count("DELETE", "/documents/doc-x") != 1 {
		t.Errorf("expected 1 delete for doc-x, got %d", rec.count("DELETE", "/documents/doc-x"))
	}
}

func TestMemory_ForgetEmptyIDNoHTTP(t *testing.T) {
	rec, mem := memServer(t, nil)
	if err := mem.Forget(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty id")
	}
	if len(rec.calls) != 0 {
		t.Errorf("expected no HTTP, got %d calls", len(rec.calls))
	}
}

func TestMemory_ForgetAllDeletesEveryListedAndReturnsCount(t *testing.T) {
	var listCalls int
	var mu sync.Mutex
	rec, mem := memServer(t, []route{
		{isList, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n := listCalls
			listCalls++
			mu.Unlock()
			if n == 0 {
				writeJSON(w, map[string]any{
					"documents": []DocumentRecord{
						{DocID: "a", ContentType: "text/plain"},
						{DocID: "b", ContentType: "text/plain"},
					},
					"total": 2, "has_more": false,
				})
			} else {
				writeJSON(w, map[string]any{"documents": []any{}, "total": 0, "has_more": false})
			}
		}},
		{isDelete, func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{}) }},
	})
	n, err := mem.ForgetAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 deleted, got %d", n)
	}
	if rec.count("DELETE", "/documents/a") != 1 || rec.count("DELETE", "/documents/b") != 1 {
		t.Errorf("expected one delete each for a and b")
	}
}

// ── error passthrough ────────────────────────────────────────────────

func TestMemory_CreditExhaustedSurfacesTypedError(t *testing.T) {
	_, mem := memServer(t, []route{
		{isInsert, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusPaymentRequired)
			writeJSON(w, map[string]string{"error": "out of credit", "code": CodeCreditExhausted})
		}},
	})
	_, err := mem.Remember(context.Background(), "x", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrCreditExhausted) {
		t.Errorf("expected ErrCreditExhausted, got %v", err)
	}
}

// ── invalid construction ─────────────────────────────────────────────

func TestMemory_EmptyOrWhitespaceEntityIDRaises(t *testing.T) {
	client := NewClient("http://localhost:9000")
	for _, id := range []string{"", "   ", "\t"} {
		if _, err := NewMemoryWithClient(id, client); err == nil {
			t.Errorf("expected error for entity id %q", id)
		}
	}
}

func TestMemory_OversizedEntityIDRaises(t *testing.T) {
	client := NewClient("http://localhost:9000")
	if _, err := NewMemoryWithClient(strings.Repeat("x", 257), client); err == nil {
		t.Error("expected error for oversized entity id")
	}
}

func TestMemory_MaxLengthEntityIDOK(t *testing.T) {
	client := NewClient("http://localhost:9000")
	if _, err := NewMemoryWithClient(strings.Repeat("x", 256), client); err != nil {
		t.Errorf("256-char entity id should be valid, got %v", err)
	}
}

func TestMemory_NilClientRaises(t *testing.T) {
	if _, err := NewMemoryWithClient("u", nil); err == nil {
		t.Error("expected error for nil client")
	}
}

// ── options ──────────────────────────────────────────────────────────

func TestMemory_WithHalfLifeChangesRecency(t *testing.T) {
	// half_life=15d (via WithHalfLife) halves docB's recency vs the 30d default.
	// docB score=80 age=30d: recency = 0.5^(30/15) = 0.25.
	// blended = 0.5*0.80 + 0.5*0.25 = 0.525.
	_, mem := memServer(t, recencyRoutes(), WithHalfLife(15*24*time.Hour))
	items, err := mem.Recall(context.Background(), "q", WithRecallK(5), WithRecencyWeight(0.5))
	if err != nil {
		t.Fatal(err)
	}
	var docB *MemoryItem
	for i := range items {
		if items[i].ID == "docB" {
			docB = &items[i]
		}
	}
	if docB == nil {
		t.Fatal("docB missing from results")
	}
	if !approx(*docB.Score, 0.525) {
		t.Errorf("expected docB blended 0.525 with 15d half-life, got %v", *docB.Score)
	}
}
