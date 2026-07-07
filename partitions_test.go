package aether

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// ── ListPartitions ────────────────────────────────────────────────

func TestListPartitionsParsesCountsAndWarnings(t *testing.T) {
	var gotMethod, gotPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{
			"partitions": []map[string]any{
				{"id": "client-a", "document_count": 3},
				{"id": "client-b", "document_count": 1},
			},
			"count": 2,
			"warnings": []map[string]any{
				{
					"kind":       "single_document",
					"partitions": []string{"client-b"},
					"detail":     "holds a single document",
				},
			},
		})
	})

	list, err := client.ListPartitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/v1/partitions" {
		t.Errorf("expected /partitions, got %s", gotPath)
	}
	if len(list.Partitions) != 2 {
		t.Fatalf("expected 2 partitions, got %d", len(list.Partitions))
	}
	if list.Partitions[0].ID != "client-a" || list.Partitions[1].ID != "client-b" {
		t.Errorf("unexpected partition ids: %+v", list.Partitions)
	}
	if list.Partitions[0].DocumentCount != 3 {
		t.Errorf("expected document_count 3, got %d", list.Partitions[0].DocumentCount)
	}
	if len(list.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(list.Warnings))
	}
	if list.Warnings[0].Kind != "single_document" {
		t.Errorf("expected single_document warning, got %s", list.Warnings[0].Kind)
	}
	if len(list.Warnings[0].Partitions) != 1 || list.Warnings[0].Partitions[0] != "client-b" {
		t.Errorf("unexpected warning partitions: %+v", list.Warnings[0].Partitions)
	}
}

// ── DeletePartition ───────────────────────────────────────────────

func TestDeletePartitionReturnsCountAndEncodesPath(t *testing.T) {
	var gotMethod, gotRawPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		// EscapedPath reflects the slash-escaped id in the path segment.
		gotRawPath = r.URL.EscapedPath()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":            "deleted",
			"partition":         "client/42",
			"documents_deleted": 7,
		})
	})

	n, err := client.DeletePartition(context.Background(), "client/42")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	// The id is URL-encoded into the path segment, slashes included.
	if gotRawPath != "/v1/partitions/client%2F42" {
		t.Errorf("expected /partitions/client%%2F42, got %s", gotRawPath)
	}
	if n != 7 {
		t.Errorf("expected 7 documents deleted, got %d", n)
	}
}

func TestDeletePartitionRejectsEmptyID(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{"documents_deleted": 0}))
	_, err := client.DeletePartition(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty id")
	}
	if !strings.Contains(err.Error(), "partition cannot be empty") {
		t.Errorf("expected validation error, got %v", err)
	}
}

func TestDeletePartitionIdempotentZero(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":            "deleted",
			"partition":         "ghost",
			"documents_deleted": 0,
		})
	})
	n, err := client.DeletePartition(context.Background(), "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 documents deleted, got %d", n)
	}
}

// ── SearchTrace + VerifyIsolation ─────────────────────────────────

// traceHandler returns a handler that asserts the request carried trace=true
// and partition=client-a, then responds with a trace whose partitions_touched
// and flags are supplied by the caller.
func traceHandler(t *testing.T, partitionsTouched []string, defaultTouched bool, results int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("trace") != "true" {
			t.Errorf("expected trace=true, got query %s", r.URL.RawQuery)
		}
		if q.Get("partition") != "client-a" {
			t.Errorf("expected partition=client-a, got query %s", r.URL.RawQuery)
		}
		hits := []SearchResult{}
		if results > 0 {
			hits = []SearchResult{{DocID: "d1", Score: 90, ContentType: "text/plain"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"query":   "q",
			"results": hits,
			"trace": map[string]any{
				"scoped_to":                 "client-a",
				"partitions_touched":        partitionsTouched,
				"default_partition_touched": defaultTouched,
				"results":                   results,
				"candidates_in_scope":       1,
				"boundary":                  "partition",
			},
		})
	}
}

func TestSearchTraceReturnsResultsAndTrace(t *testing.T) {
	_, base := jsonServer(t, traceHandler(t, []string{"client-a"}, false, 1))
	client, err := base.Partition("client-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	traced, err := client.SearchTrace(context.Background(), "returns policy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if traced.Trace.ScopedTo == nil || *traced.Trace.ScopedTo != "client-a" {
		t.Errorf("expected scoped_to client-a, got %v", traced.Trace.ScopedTo)
	}
	if len(traced.Trace.PartitionsTouched) != 1 || traced.Trace.PartitionsTouched[0] != "client-a" {
		t.Errorf("unexpected partitions_touched: %+v", traced.Trace.PartitionsTouched)
	}
	if traced.Trace.CandidatesInScope == nil || *traced.Trace.CandidatesInScope != 1 {
		t.Errorf("expected candidates_in_scope 1, got %v", traced.Trace.CandidatesInScope)
	}
	if traced.Trace.Boundary != "partition" {
		t.Errorf("expected boundary partition, got %s", traced.Trace.Boundary)
	}
	if len(traced.Results) != 1 || traced.Results[0].DocID != "d1" {
		t.Errorf("unexpected results: %+v", traced.Results)
	}
}

func TestVerifyIsolationOKWhenScopeHolds(t *testing.T) {
	_, base := jsonServer(t, traceHandler(t, []string{"client-a"}, false, 1))
	client, err := base.Partition("client-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	check, err := client.VerifyIsolation(context.Background(), "returns policy")
	if err != nil {
		t.Fatal(err)
	}
	if !check.OK {
		t.Errorf("expected ok=true, got %+v", check)
	}
	if len(check.Leaked) != 0 {
		t.Errorf("expected no leaks, got %+v", check.Leaked)
	}
}

func TestVerifyIsolationFlagsALeak(t *testing.T) {
	_, base := jsonServer(t, traceHandler(t, []string{"client-a", "client-b"}, false, 1))
	client, err := base.Partition("client-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	check, err := client.VerifyIsolation(context.Background(), "returns policy")
	if err != nil {
		t.Fatal(err)
	}
	if check.OK {
		t.Errorf("expected ok=false on leak, got %+v", check)
	}
	if len(check.Leaked) != 1 || check.Leaked[0] != "client-b" {
		t.Errorf("expected leaked=[client-b], got %+v", check.Leaked)
	}
}

func TestVerifyIsolationFlagsDefaultPartitionTouched(t *testing.T) {
	_, base := jsonServer(t, traceHandler(t, []string{"client-a"}, true, 1))
	client, err := base.Partition("client-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	check, err := client.VerifyIsolation(context.Background(), "returns policy")
	if err != nil {
		t.Fatal(err)
	}
	if check.OK {
		t.Errorf("expected ok=false when default partition touched, got %+v", check)
	}
}

func TestVerifyIsolationRequiresAHandle(t *testing.T) {
	_, client := jsonServer(t, traceHandler(t, []string{"client-a"}, false, 1))
	_, err := client.VerifyIsolation(context.Background(), "returns policy")
	if err == nil {
		t.Fatal("expected error without a partition handle")
	}
	if !strings.Contains(err.Error(), "requires a partition handle") {
		t.Errorf("expected handle-required error, got %v", err)
	}
}

// ── Doc_id-addressed partition guard ──────────────────────────────

// TestPartitionHandleGuardsDocIDRoutes verifies that a scoped handle injects
// the partition as a query-param guard on every doc_id-addressed operation,
// while the unscoped client keeps sending exactly what it sent before (no
// partition at all).
func TestPartitionHandleGuardsDocIDRoutes(t *testing.T) {
	var gotPath, gotRawQuery, gotPartition string
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		gotPartition = r.URL.Query().Get("partition")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	scoped, err := base.Partition("client-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	ctx := context.Background()

	cases := []struct {
		name string
		call func(c *Client) error
		path string
	}{
		{"get", func(c *Client) error { _, e := c.Get(ctx, "doc-1"); return e }, "/v1/documents/doc-1"},
		{"download", func(c *Client) error { _, e := c.Download(ctx, "doc-1"); return e }, "/v1/documents/doc-1/download"},
		{"download_text", func(c *Client) error { _, e := c.DownloadText(ctx, "doc-1"); return e }, "/v1/documents/doc-1/download"},
		{"delete", func(c *Client) error { return c.Delete(ctx, "doc-1") }, "/v1/documents/doc-1"},
		{"hard_delete", func(c *Client) error { return c.HardDelete(ctx, "doc-1") }, "/v1/documents/doc-1"},
		{"restore", func(c *Client) error { return c.Restore(ctx, "doc-1") }, "/v1/documents/doc-1/restore"},
		{"backfill_entity", func(c *Client) error { _, e := c.BackfillEntityFromTags(ctx, "client:", false); return e }, "/v1/documents/backfill-entity"},
	}
	for _, tc := range cases {
		// The scoped handle injects the guard.
		gotPath, gotPartition = "", ""
		if err := tc.call(scoped); err != nil {
			t.Fatalf("%s (scoped): %v", tc.name, err)
		}
		if gotPath != tc.path {
			t.Errorf("%s: expected path %s, got %s", tc.name, tc.path, gotPath)
		}
		if gotPartition != "client-a" {
			t.Errorf("%s: expected partition=client-a guard, got query %s", tc.name, gotRawQuery)
		}

		// The unscoped client stays byte-identical to before.
		gotRawQuery = ""
		if err := tc.call(base); err != nil {
			t.Fatalf("%s (unscoped): %v", tc.name, err)
		}
		if strings.Contains(gotRawQuery, "partition") {
			t.Errorf("%s: expected unscoped call to send no partition, got %s", tc.name, gotRawQuery)
		}
	}

	// HardDelete keeps its hard=true param alongside the guard.
	if err := scoped.HardDelete(ctx, "doc-1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotRawQuery, "hard=true") || !strings.Contains(gotRawQuery, "partition=client-a") {
		t.Errorf("hard_delete: expected hard=true and partition=client-a, got %s", gotRawQuery)
	}
}

// ── MoveDocument ──────────────────────────────────────────────────

func TestMoveDocumentSendsBothWireFields(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]json.RawMessage
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"doc_id":       "doc-1",
			"cid":          "cid-1",
			"content_type": "text/plain",
			"version":      2,
			"partition":    "client-b",
		})
	})

	// from=nil names the default pool; to names the destination.
	to := "client-b"
	doc, err := client.MoveDocument(context.Background(), "doc-1", nil, &to)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/documents/doc-1/move" {
		t.Errorf("expected /v1/documents/doc-1/move, got %s", gotPath)
	}
	// Both keys must be PRESENT on the wire; nil serializes as an explicit null.
	raw, ok := gotBody["expect_partition"]
	if !ok {
		t.Error("expected expect_partition key in body")
	} else if string(raw) != "null" {
		t.Errorf("expected expect_partition null, got %s", raw)
	}
	raw, ok = gotBody["to_partition"]
	if !ok {
		t.Error("expected to_partition key in body")
	} else if string(raw) != `"client-b"` {
		t.Errorf("expected to_partition \"client-b\", got %s", raw)
	}
	// The response is the full record with the new partition echoed.
	if doc.Partition == nil || *doc.Partition != "client-b" {
		t.Errorf("expected partition echo client-b, got %v", doc.Partition)
	}
	if doc.Version != 2 {
		t.Errorf("expected version 2, got %d", doc.Version)
	}
}

// TestMoveDocumentIsNotAutoScoped verifies a partition handle never aims a
// move: both partitions come only from the explicit arguments, and no
// partition query param is sent.
func TestMoveDocumentIsNotAutoScoped(t *testing.T) {
	var gotRawQuery string
	var gotBody map[string]json.RawMessage
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"doc_id": "doc-1", "cid": "c", "content_type": "text/plain"})
	})
	scoped, err := base.Partition("client-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}

	from, to := "client-a", "client-b"
	if _, err := scoped.MoveDocument(context.Background(), "doc-1", &from, &to); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotRawQuery, "partition") {
		t.Errorf("expected no partition query param on move, got %s", gotRawQuery)
	}
	if string(gotBody["expect_partition"]) != `"client-a"` || string(gotBody["to_partition"]) != `"client-b"` {
		t.Errorf("expected explicit from/to in body, got %v", gotBody)
	}
}

// TestMoveDocumentMismatchSurfacesNotFound verifies a wrong from assertion
// surfaces as the same 404 document_not_found as a nonexistent id.
func TestMoveDocumentMismatchSurfacesNotFound(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "document doc-1 not found",
			"code":  "document_not_found",
		})
	})

	from, to := "client-a", "client-b"
	_, err := client.MoveDocument(context.Background(), "doc-1", &from, &to)
	if err == nil {
		t.Fatal("expected not-found error on mismatch")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 404 {
		t.Errorf("expected 404, got %d", apiErr.StatusCode)
	}
	if apiErr.ErrorCode != "document_not_found" {
		t.Errorf("expected error code document_not_found, got %s", apiErr.ErrorCode)
	}
}

// TestMoveDocumentValidatesArguments verifies client-side validation: the doc
// id must be non-empty, and a non-nil partition must pass the usual id rule
// (nil is exempt — it is the default partition, not an omission). No HTTP call
// is made on a validation failure.
func TestMoveDocumentValidatesArguments(t *testing.T) {
	var called bool
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{"doc_id": "doc-1"})
	})
	ctx := context.Background()
	to := "client-b"

	if _, err := client.MoveDocument(ctx, "", nil, &to); err == nil || !strings.Contains(err.Error(), "docID cannot be empty") {
		t.Errorf("expected docID validation error, got %v", err)
	}
	empty := ""
	if _, err := client.MoveDocument(ctx, "doc-1", &empty, &to); err == nil || !strings.Contains(err.Error(), "partition cannot be empty") {
		t.Errorf("expected empty-from validation error, got %v", err)
	}
	long := strings.Repeat("x", 257)
	if _, err := client.MoveDocument(ctx, "doc-1", nil, &long); err == nil || !strings.Contains(err.Error(), "must be 1-256 characters") {
		t.Errorf("expected over-long-to validation error, got %v", err)
	}
	if called {
		t.Error("expected no HTTP call on validation failure")
	}
}

// ── Partition echo on responses ───────────────────────────────────

// TestDocumentResponsesCarryPartitionEcho verifies the partition field is
// parsed from insert/get/list responses: a string for a named partition and
// nil for the default partition (explicit null), mirroring EntityID/Source.
func TestDocumentResponsesCarryPartitionEcho(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"doc_id": "d1", "cid": "c1", "content_type": "text/plain",
				"partition": "client-a",
			})
		case r.URL.Path == "/v1/documents":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"documents": []map[string]any{
					{"doc_id": "d1", "cid": "c1", "content_type": "text/plain", "partition": "client-a"},
					{"doc_id": "d2", "cid": "c2", "content_type": "text/plain", "partition": nil},
				},
				"total": 2,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"doc_id": "d1", "cid": "c1", "content_type": "text/plain",
				"partition": nil,
			})
		}
	})
	ctx := context.Background()

	inserted, err := client.InsertText(ctx, "hello", "n.txt")
	if err != nil {
		t.Fatal(err)
	}
	if inserted.Partition == nil || *inserted.Partition != "client-a" {
		t.Errorf("insert: expected partition client-a, got %v", inserted.Partition)
	}

	got, err := client.Get(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Partition != nil {
		t.Errorf("get: expected nil partition for the default pool, got %v", *got.Partition)
	}

	list, err := client.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Documents) != 2 {
		t.Fatalf("expected 2 documents, got %d", len(list.Documents))
	}
	if list.Documents[0].Partition == nil || *list.Documents[0].Partition != "client-a" {
		t.Errorf("list[0]: expected partition client-a, got %v", list.Documents[0].Partition)
	}
	if list.Documents[1].Partition != nil {
		t.Errorf("list[1]: expected nil partition, got %v", *list.Documents[1].Partition)
	}
}

// TestSearchHitsCarryPartitionEcho verifies the partition field is parsed on
// every search hit and threaded through Retrieve's enriched results.
func TestSearchHitsCarryPartitionEcho(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{
		"query": "q",
		"results": []map[string]any{
			{"doc_id": "d1", "score": 90, "content_type": "text/plain", "content": "x", "partition": "client-a"},
			{"doc_id": "d2", "score": 80, "content_type": "text/plain", "content": "y", "partition": nil},
		},
	}))
	ctx := context.Background()

	results, err := client.Search(ctx, "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Partition == nil || *results[0].Partition != "client-a" {
		t.Errorf("hit 0: expected partition client-a, got %v", results[0].Partition)
	}
	if results[1].Partition != nil {
		t.Errorf("hit 1: expected nil partition, got %v", *results[1].Partition)
	}

	retrieved, err := client.Retrieve(ctx, "q", 2)
	if err != nil {
		t.Fatal(err)
	}
	if retrieved[0].Partition == nil || *retrieved[0].Partition != "client-a" {
		t.Errorf("retrieve 0: expected partition client-a, got %v", retrieved[0].Partition)
	}
	if retrieved[1].Partition != nil {
		t.Errorf("retrieve 1: expected nil partition, got %v", *retrieved[1].Partition)
	}
}

// ── partition_required typed error ────────────────────────────────

func TestUnscopedMultiTenantCallRaisesPartitionRequired(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "This API key is multi-tenant, so every search must name a partition.",
			"code":  "partition_required",
		})
	})

	_, err := client.Search(context.Background(), "anything", 10)
	if err == nil {
		t.Fatal("expected partition_required error")
	}
	if !errors.Is(err, ErrPartitionRequired) {
		t.Errorf("expected errors.Is(err, ErrPartitionRequired), got %v", err)
	}
	// It must not match the unrelated billing sentinels.
	if errors.Is(err, ErrCreditExhausted) || errors.Is(err, ErrTenantPaused) {
		t.Errorf("partition_required must not match billing sentinels: %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected APIError, got %T", err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("expected 400, got %d", apiErr.StatusCode)
	}
	if apiErr.ErrorCode != CodePartitionRequired {
		t.Errorf("expected error code %s, got %s", CodePartitionRequired, apiErr.ErrorCode)
	}
	if apiErr.IsRetryable() {
		t.Errorf("partition_required must not be retryable")
	}
}

// TestUnguardedDocIDCallRaisesPartitionRequired verifies the typed error is
// detectable on the doc_id-addressed routes too: a key minted with strict
// scoping rejects an unguarded by-id call with the same partition_required
// code, and errors.Is matches it.
func TestUnguardedDocIDCallRaisesPartitionRequired(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "This API key requires strict scoping, so every document access must name a partition.",
			"code":  "partition_required",
		})
	})

	_, err := client.Get(context.Background(), "doc-1")
	if err == nil {
		t.Fatal("expected partition_required error")
	}
	if !errors.Is(err, ErrPartitionRequired) {
		t.Errorf("expected errors.Is(err, ErrPartitionRequired), got %v", err)
	}
}
