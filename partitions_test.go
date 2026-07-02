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
