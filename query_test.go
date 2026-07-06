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

// decodeReqBody reads + JSON-decodes a request body into a generic map.
func decodeReqBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, _ := io.ReadAll(r.Body)
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return m
}

func TestQueryModeAReturnsDocumentPage(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = decodeReqBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documents": []map[string]any{{"doc_id": "d1", "content_type": "text/plain"}},
			"total":     1,
			"has_more":  false,
		})
	})

	limit := 10
	resp, err := client.Query(context.Background(), QueryRequest{
		Filter: map[string]any{"field": "status", "op": "eq", "value": "paid"},
		Sort:   []QuerySort{{By: "created_at", Dir: "desc"}},
		Limit:  &limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.IsAggregate() {
		t.Fatal("expected a Mode A page, got an aggregate")
	}
	if resp.Page == nil || len(resp.Page.Documents) != 1 || resp.Page.Documents[0].DocID != "d1" {
		t.Fatalf("unexpected page: %+v", resp.Page)
	}
	if resp.Page.Total != 1 || resp.Page.HasMore {
		t.Errorf("total/has_more mismatch: %+v", resp.Page)
	}
	if gotMethod != http.MethodPost || !strings.HasSuffix(gotPath, "/query") {
		t.Errorf("expected POST .../query, got %s %s", gotMethod, gotPath)
	}
	if _, ok := gotBody["filter"]; !ok {
		t.Errorf("expected filter in body, got %v", gotBody)
	}
	if _, ok := gotBody["aggregate"]; ok {
		t.Errorf("Mode A body must not carry aggregate")
	}
	if _, ok := gotBody["partition"]; ok {
		t.Errorf("unscoped query must not carry partition")
	}
}

func TestQueryModeBReturnsAggregateResult(t *testing.T) {
	var gotBody map[string]any
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeReqBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"groups": []map[string]any{
				{"keys": map[string]any{"status": "paid"}, "aggregates": map[string]any{"total": 3}},
			},
			"total_groups": 1,
			"scanned":      12,
		})
	})

	resp, err := client.Query(context.Background(), QueryRequest{
		GroupBy:   []string{"status"},
		Aggregate: []map[string]any{{"op": "count", "as": "total"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsAggregate() || resp.Aggregate == nil {
		t.Fatalf("expected a Mode B aggregate, got %+v", resp)
	}
	if len(resp.Aggregate.Groups) != 1 || resp.Aggregate.TotalGroups != 1 || resp.Aggregate.Scanned != 12 {
		t.Fatalf("unexpected aggregate: %+v", resp.Aggregate)
	}
	if resp.Aggregate.Groups[0].Keys["status"] != "paid" {
		t.Errorf("group key mismatch: %+v", resp.Aggregate.Groups[0])
	}
	if _, ok := gotBody["aggregate"]; !ok {
		t.Errorf("Mode B body must carry aggregate, got %v", gotBody)
	}
	if _, ok := gotBody["group_by"]; !ok {
		t.Errorf("expected group_by in body, got %v", gotBody)
	}
}

func TestSchemaDeclareListDelete(t *testing.T) {
	fieldsBody := map[string]any{
		"fields": []map[string]any{
			{"name": "amount", "type": "int", "source": map[string]any{"metadata": "amount"},
				"coverage": 2, "mismatch_count": 1, "backfill": "complete"},
		},
	}

	// Declare
	var declMethod, declPath string
	var declBody map[string]any
	_, dc := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		declMethod, declPath = r.Method, r.URL.Path
		declBody = decodeReqBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fieldsBody)
	})
	fields, err := dc.DeclareFields(context.Background(), []FieldInput{
		{Name: "amount", Type: "int", Source: map[string]any{"metadata": "amount"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if declMethod != http.MethodPut || !strings.HasSuffix(declPath, "/schema/fields") {
		t.Errorf("expected PUT .../schema/fields, got %s %s", declMethod, declPath)
	}
	if len(fields) != 1 || fields[0].Name != "amount" || fields[0].Type != "int" {
		t.Fatalf("unexpected declared fields: %+v", fields)
	}
	if fields[0].Coverage != 2 || fields[0].MismatchCount != 1 {
		t.Errorf("coverage/mismatch not parsed: %+v", fields[0])
	}
	if _, ok := declBody["fields"]; !ok {
		t.Errorf("declare must send a fields array, got %v", declBody)
	}

	// List
	var listMethod string
	_, lc := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		listMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fieldsBody)
	})
	listed, err := lc.ListFields(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listMethod != http.MethodGet || len(listed) != 1 {
		t.Errorf("list: method %s, %d fields", listMethod, len(listed))
	}

	// Delete
	var delMethod, delPath string
	_, delc := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		delMethod, delPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"fields": []any{}})
	})
	remaining, err := delc.DeleteField(context.Background(), "amount")
	if err != nil {
		t.Fatal(err)
	}
	if delMethod != http.MethodDelete || !strings.HasSuffix(delPath, "/schema/fields/amount") {
		t.Errorf("expected DELETE .../schema/fields/amount, got %s %s", delMethod, delPath)
	}
	if len(remaining) != 0 {
		t.Errorf("expected no remaining fields, got %+v", remaining)
	}
}

func TestPartitionScopingOnQueryAndSchema(t *testing.T) {
	// Query on a scoped handle pins the partition into the body.
	var qBody map[string]any
	_, qc := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		qBody = decodeReqBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"documents": []any{}, "total": 0, "has_more": false})
	})
	scoped, err := qc.Partition("client-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scoped.Query(context.Background(), QueryRequest{}); err != nil {
		t.Fatal(err)
	}
	if qBody["partition"] != "client-a" {
		t.Errorf("scoped query must pin partition in the body, got %v", qBody)
	}

	// Schema calls on a scoped handle pin the partition query param.
	var gotPartition string
	_, sc := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPartition = r.URL.Query().Get("partition")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"fields": []any{}})
	})
	scopedSchema, _ := sc.Partition("client-a")
	if _, err := scopedSchema.ListFields(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotPartition != "client-a" {
		t.Errorf("scoped schema call must pin ?partition=client-a, got %q", gotPartition)
	}
}

func TestQueryMapsTypedError(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "partition required — scope the call through Partition(id)",
			"code":  "partition_required",
		})
	})
	_, err := client.Query(context.Background(), QueryRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrPartitionRequired) {
		t.Fatalf("expected ErrPartitionRequired, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected a 400 APIError, got %v", err)
	}
}
