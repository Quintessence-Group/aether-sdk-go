package aether

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodeBody reads and JSON-decodes a request body into a map for assertions.
func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	if len(b) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode body %q: %v", string(b), err)
	}
	return m
}

// ── §14 case 1: entity round-trip ─────────────────────────────────────────

func TestGraphUpsertEntityRoundTrip(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotBody = decodeBody(t, r)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memory_entity_id": "ent-1",
			"entity_id":        "owner-1",
			"entity_type":      "person",
			"display_name":     "John",
			"aliases":          []string{"Johnny"},
			"attributes":       map[string]any{"vip": true, "age": 42},
			"created_at":       "2026-06-15T00:00:00Z",
			"updated_at":       "2026-06-15T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, err := NewMemoryWithClient("owner-1", client)
	if err != nil {
		t.Fatal(err)
	}

	ent, err := mem.UpsertEntity(context.Background(), "person",
		WithDisplayName("John"),
		WithAliases([]string{"Johnny"}),
		WithEntityAttributes(map[string]any{"vip": true}),
	)
	if err != nil {
		t.Fatal(err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/memory/entities" {
		t.Errorf("expected /memory/entities, got %s", gotPath)
	}
	// entity_id=<owner> on the query string.
	if !strings.Contains(gotQuery, "entity_id=owner-1") {
		t.Errorf("expected entity_id=owner-1 in query, got %s", gotQuery)
	}
	// body carries entity_type + the provided fields.
	if gotBody["entity_type"] != "person" {
		t.Errorf("expected entity_type=person in body, got %v", gotBody["entity_type"])
	}
	if gotBody["display_name"] != "John" {
		t.Errorf("expected display_name=John in body, got %v", gotBody["display_name"])
	}
	if _, ok := gotBody["aliases"]; !ok {
		t.Errorf("expected aliases in body, got %v", gotBody)
	}
	if attrs, ok := gotBody["attributes"].(map[string]any); !ok || attrs["vip"] != true {
		t.Errorf("expected attributes.vip=true in body, got %v", gotBody["attributes"])
	}
	// memory_entity_id omitted (not supplied).
	if _, ok := gotBody["memory_entity_id"]; ok {
		t.Errorf("expected memory_entity_id omitted when not supplied, got %v", gotBody)
	}

	// parsed result reflects the response including memory_entity_id.
	if ent.MemoryEntityID != "ent-1" {
		t.Errorf("expected memory_entity_id ent-1, got %s", ent.MemoryEntityID)
	}
	if ent.EntityID != "owner-1" {
		t.Errorf("expected entity_id owner-1, got %s", ent.EntityID)
	}
	if ent.EntityType != "person" {
		t.Errorf("expected entity_type person, got %s", ent.EntityType)
	}
	if ent.DisplayName == nil || *ent.DisplayName != "John" {
		t.Errorf("expected display_name John, got %v", ent.DisplayName)
	}
	if len(ent.Aliases) != 1 || ent.Aliases[0] != "Johnny" {
		t.Errorf("expected aliases [Johnny], got %v", ent.Aliases)
	}
	if ent.Attributes["vip"] != true {
		t.Errorf("expected attributes.vip=true, got %v", ent.Attributes)
	}
}

func TestGraphUpsertEntityWithMemoryEntityID(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"memory_entity_id": "ent-x", "entity_id": "owner-1", "entity_type": "person"})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	if _, err := mem.UpsertEntity(context.Background(), "person", WithMemoryEntityID("ent-x")); err != nil {
		t.Fatal(err)
	}
	if gotBody["memory_entity_id"] != "ent-x" {
		t.Errorf("expected memory_entity_id ent-x in body, got %v", gotBody["memory_entity_id"])
	}
}

func TestGraphGetEntity(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memory_entity_id": "ent-1", "entity_id": "owner-1", "entity_type": "person",
			"partition": nil,
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	ent, err := mem.GetEntity(context.Background(), "ent-1")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	if gotPath != "/v1/memory/entities/ent-1" {
		t.Errorf("expected /memory/entities/ent-1, got %s", gotPath)
	}
	// by-id read is still partition-aware → entity_id injected.
	if !strings.Contains(gotQuery, "entity_id=owner-1") {
		t.Errorf("expected entity_id on by-id read, got %s", gotQuery)
	}
	if ent.MemoryEntityID != "ent-1" {
		t.Errorf("expected ent-1, got %s", ent.MemoryEntityID)
	}
	// nullable partition decodes to nil.
	if ent.Partition != nil {
		t.Errorf("expected nil partition, got %v", *ent.Partition)
	}
}

// ── §14 case 2: entity scoping + partition ────────────────────────────────

func TestGraphScopingPartition(t *testing.T) {
	queries := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries[r.Method+" "+r.URL.Path] = r.URL.RawQuery
		switch {
		case r.URL.Path == "/v1/memory/entities" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"memory_entity_id": "e1", "entity_id": "patient-john", "entity_type": "person"})
		case r.URL.Path == "/v1/memory/facts" && r.Method == http.MethodPost:
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"fact_id": "f1", "entity_id": "patient-john", "subject_type": "owner", "predicate": "p", "value": "v", "cardinality": "single"})
		case r.URL.Path == "/v1/memory/consolidate":
			_ = json.NewEncoder(w).Encode(map[string]any{"active_facts_before": 1, "active_facts_after": 1, "retracted": 0})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"entities": []any{}})
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
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := mem.UpsertEntity(ctx, "person"); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.ListEntities(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.RememberFact(ctx, "p", "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Consolidate(ctx); err != nil {
		t.Fatal(err)
	}

	// every graph request carries BOTH entity_id=<owner> and partition=<scope>.
	for key, q := range queries {
		if !strings.Contains(q, "entity_id=patient-john") {
			t.Errorf("%s: expected entity_id=patient-john, got %s", key, q)
		}
		if !strings.Contains(q, "partition=tenant-x") {
			t.Errorf("%s: expected partition=tenant-x, got %s", key, q)
		}
	}
}

// ── §14 case 3: list filters present/omitted ──────────────────────────────

func TestGraphListEntitiesFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entities": []map[string]any{
				{"memory_entity_id": "e1", "entity_id": "owner-1", "entity_type": "person"},
			},
			"count": 1,
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	ents, err := mem.ListEntities(context.Background(), WithEntityType("person"), WithEntityLimit(10))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "entity_type=person") {
		t.Errorf("expected entity_type=person, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "limit=10") {
		t.Errorf("expected limit=10, got %s", gotQuery)
	}
	if len(ents) != 1 || ents[0].MemoryEntityID != "e1" {
		t.Errorf("expected one entity e1, got %v", ents)
	}
}

func TestGraphListEntitiesNoFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"entities": []any{}, "count": 0})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	if _, err := mem.ListEntities(context.Background()); err != nil {
		t.Fatal(err)
	}
	// unset filters are ABSENT from the query (only entity_id remains).
	if strings.Contains(gotQuery, "entity_type") {
		t.Errorf("expected entity_type absent, got %s", gotQuery)
	}
	if strings.Contains(gotQuery, "limit") {
		t.Errorf("expected limit absent, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "entity_id=owner-1") {
		t.Errorf("expected entity_id present, got %s", gotQuery)
	}
}

// ── §14 case 4: relationship round-trip + active filter ───────────────────

func TestGraphRelateRoundTrip(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody = decodeBody(t, r)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"relationship_id":   "rel-1",
			"entity_id":         "owner-1",
			"from_entity_id":    "a",
			"to_entity_id":      "b",
			"relationship_type": "works_at",
			"observed_at":       "2026-06-15T00:00:00Z",
			"created_at":        "2026-06-15T00:00:00Z",
			"updated_at":        "2026-06-15T00:00:00Z",
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	rel, err := mem.Relate(context.Background(), "a", "b", "works_at",
		WithRelationshipValidFrom("2026-01-01T00:00:00Z"),
		WithRelationshipAttributes(map[string]any{"role": "eng"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/memory/relationships" {
		t.Errorf("expected POST /memory/relationships, got %s %s", gotMethod, gotPath)
	}
	for _, key := range []string{"from_entity_id", "to_entity_id", "relationship_type"} {
		if _, ok := gotBody[key]; !ok {
			t.Errorf("expected %s in body, got %v", key, gotBody)
		}
	}
	if gotBody["valid_from"] != "2026-01-01T00:00:00Z" {
		t.Errorf("expected valid_from in body, got %v", gotBody["valid_from"])
	}
	if rel.RelationshipID != "rel-1" || rel.FromEntityID != "a" || rel.ToEntityID != "b" {
		t.Errorf("expected parsed edge rel-1 a->b, got %+v", rel)
	}
}

func TestGraphListRelationshipsIncludeInactiveAsOf(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"relationships": []map[string]any{
				{"relationship_id": "rel-1", "entity_id": "owner-1", "from_entity_id": "a", "to_entity_id": "b", "relationship_type": "works_at"},
			},
			"count": 1,
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	rels, err := mem.ListRelationships(context.Background(),
		WithRelationshipsFrom("a"),
		WithIncludeInactiveRelationships(true),
		WithRelationshipsAsOf("2026-06-01T00:00:00Z"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "include_inactive=true") {
		t.Errorf("expected include_inactive=true, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "as_of=2026-06-01T00%3A00%3A00Z") {
		t.Errorf("expected as_of encoded, got %s", gotQuery)
	}
	if !strings.Contains(gotQuery, "from_entity_id=a") {
		t.Errorf("expected from_entity_id=a, got %s", gotQuery)
	}
	if len(rels) != 1 || rels[0].RelationshipID != "rel-1" {
		t.Errorf("expected one edge rel-1, got %v", rels)
	}
}

func TestGraphListRelationshipsDefaultOmitsInclude(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"relationships": []any{}, "count": 0})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	if _, err := mem.ListRelationships(context.Background()); err != nil {
		t.Fatal(err)
	}
	// default omits include_inactive (server defaults it false).
	if strings.Contains(gotQuery, "include_inactive") {
		t.Errorf("expected include_inactive omitted by default, got %s", gotQuery)
	}
}

// ── §14 case 5: fact assert + subject (incl scalar value types + null) ────

func TestGraphRememberFactOwnerDefault(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"fact_id": "f1", "entity_id": "owner-1", "subject_type": "owner",
			"predicate": "favorite_color", "value": "blue", "cardinality": "single",
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	fact, err := mem.RememberFact(context.Background(), "favorite_color", "blue")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["subject_type"] != "owner" {
		t.Errorf("expected subject_type=owner, got %v", gotBody["subject_type"])
	}
	if gotBody["predicate"] != "favorite_color" {
		t.Errorf("expected predicate, got %v", gotBody["predicate"])
	}
	if gotBody["value"] != "blue" {
		t.Errorf("expected value=blue, got %v", gotBody["value"])
	}
	// owner subject → subject_id omitted.
	if _, ok := gotBody["subject_id"]; ok {
		t.Errorf("expected subject_id omitted for owner, got %v", gotBody)
	}
	if fact.FactID != "f1" || fact.Value != "blue" {
		t.Errorf("expected parsed fact f1/blue, got %+v", fact)
	}
}

func TestGraphRememberFactEntitySubject(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody = decodeBody(t, r)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"fact_id": "f1", "entity_id": "owner-1", "subject_type": "entity",
			"subject_id": "ent-9", "predicate": "status", "value": "active", "cardinality": "single",
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	fact, err := mem.RememberFact(context.Background(), "status", "active", WithFactSubjectEntity("ent-9"))
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["subject_type"] != "entity" {
		t.Errorf("expected subject_type=entity, got %v", gotBody["subject_type"])
	}
	if gotBody["subject_id"] != "ent-9" {
		t.Errorf("expected subject_id=ent-9, got %v", gotBody["subject_id"])
	}
	if fact.SubjectID == nil || *fact.SubjectID != "ent-9" {
		t.Errorf("expected parsed subject_id ent-9, got %v", fact.SubjectID)
	}
}

func TestGraphRememberFactScalarValueTypes(t *testing.T) {
	// number, bool, and null all round-trip in the body; value is ALWAYS sent.
	cases := []struct {
		name  string
		value any
		check func(raw json.RawMessage) bool
	}{
		{"string", "blue", func(raw json.RawMessage) bool { return string(raw) == `"blue"` }},
		{"number", 42, func(raw json.RawMessage) bool { return string(raw) == `42` }},
		{"float", 3.5, func(raw json.RawMessage) bool { return string(raw) == `3.5` }},
		{"bool", true, func(raw json.RawMessage) bool { return string(raw) == `true` }},
		{"null", nil, func(raw json.RawMessage) bool { return string(raw) == `null` }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rawBody map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &rawBody)
				w.WriteHeader(201)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"fact_id": "f1", "entity_id": "owner-1", "subject_type": "owner",
					"predicate": "p", "value": tc.value, "cardinality": "single",
				})
			}))
			t.Cleanup(srv.Close)
			client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
			mem, _ := NewMemoryWithClient("owner-1", client)

			if _, err := mem.RememberFact(context.Background(), "p", tc.value); err != nil {
				t.Fatal(err)
			}
			raw, ok := rawBody["value"]
			if !ok {
				t.Fatalf("expected value key always present, got %v", rawBody)
			}
			if !tc.check(raw) {
				t.Errorf("value %s: unexpected wire form %s", tc.name, string(raw))
			}
		})
	}
}

func TestGraphRememberFactMissingSubjectIDNoHTTP(t *testing.T) {
	// A non-owner subject without subject_id raises client-side with no HTTP.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	// WithFactSubjectEntity("") sets subject_type=entity with empty subject_id.
	_, err := mem.RememberFact(context.Background(), "status", "x", WithFactSubjectEntity(""))
	if err == nil {
		t.Fatal("expected error for entity subject without subject_id")
	}
	if !strings.Contains(err.Error(), "subject_id is required") {
		t.Errorf("expected subject_id error, got %v", err)
	}
	if called {
		t.Error("expected no HTTP call for invalid subject")
	}
}

// ── §14 case 6: fact history ──────────────────────────────────────────────

func TestGraphFactHistory(t *testing.T) {
	var gotMethod, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"facts": []map[string]any{
				{"fact_id": "f2", "entity_id": "owner-1", "subject_type": "entity", "subject_id": "E", "predicate": "status", "value": "active", "cardinality": "single"},
				{"fact_id": "f1", "entity_id": "owner-1", "subject_type": "entity", "subject_id": "E", "predicate": "status", "value": "pending", "cardinality": "single", "invalid_from": "2026-06-10T00:00:00Z"},
			},
			"count": 2,
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	facts, err := mem.FactHistory(context.Background(), "status", WithHistorySubjectEntity("E"))
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("expected GET, got %s", gotMethod)
	}
	for _, want := range []string{"history=true", "subject_type=entity", "subject_id=E", "predicate=status"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
	if len(facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(facts))
	}
	// second fact carries invalid_from (the superseded one).
	if facts[1].InvalidFrom == nil || *facts[1].InvalidFrom != "2026-06-10T00:00:00Z" {
		t.Errorf("expected invalid_from on superseded fact, got %v", facts[1].InvalidFrom)
	}
}

func TestGraphFactHistoryOwnerDefault(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"facts": []any{}, "count": 0})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	if _, err := mem.FactHistory(context.Background(), "status"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "subject_type=owner") {
		t.Errorf("expected subject_type=owner default, got %s", gotQuery)
	}
	// owner default → no subject_id on the wire.
	if strings.Contains(gotQuery, "subject_id") {
		t.Errorf("expected no subject_id for owner default, got %s", gotQuery)
	}
}

// ── §14 case 7: consolidate ───────────────────────────────────────────────

func TestGraphConsolidate(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	var bodyLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		bodyLen = len(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active_facts_before": 10, "active_facts_after": 7, "retracted": 3,
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	report, err := mem.Consolidate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/memory/consolidate" {
		t.Errorf("expected POST /memory/consolidate, got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "entity_id=owner-1") {
		t.Errorf("expected entity_id on consolidate, got %s", gotQuery)
	}
	if bodyLen != 0 {
		t.Errorf("expected no body on consolidate, got %d bytes", bodyLen)
	}
	if report.ActiveFactsBefore != 10 || report.ActiveFactsAfter != 7 || report.Retracted != 3 {
		t.Errorf("expected report 10/7/3, got %+v", report)
	}
}

// ── ListFacts filters ─────────────────────────────────────────────────────

func TestGraphListFactsFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{
			"facts": []map[string]any{
				{"fact_id": "f1", "entity_id": "owner-1", "subject_type": "entity", "subject_id": "E", "predicate": "status", "value": "active", "cardinality": "single"},
			},
			"count": 1,
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	facts, err := mem.ListFacts(context.Background(),
		WithFactsSubjectEntity("E"),
		WithFactsPredicate("status"),
		WithIncludeInactiveFacts(true),
		WithFactsAsOf("2026-06-01T00:00:00Z"),
		WithFactsLimit(5),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"subject_type=entity", "subject_id=E", "predicate=status", "include_inactive=true", "limit=5"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected %s in query, got %s", want, gotQuery)
		}
	}
	if len(facts) != 1 || facts[0].FactID != "f1" {
		t.Errorf("expected one fact f1, got %v", facts)
	}
}

func TestGraphListFactsNoFilters(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"facts": []any{}, "count": 0})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	if _, err := mem.ListFacts(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"subject_type", "subject_id", "predicate", "include_inactive", "as_of", "limit"} {
		if strings.Contains(gotQuery, unwanted) {
			t.Errorf("expected %s absent with no filters, got %s", unwanted, gotQuery)
		}
	}
}

func TestGraphListFactsEntitySubjectMissingIDNoHTTP(t *testing.T) {
	// list_facts: when subject_type is entity, subject_id is required (selector).
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	_, err := mem.ListFacts(context.Background(), WithFactsSubjectEntity(""))
	if err == nil {
		t.Fatal("expected error for entity subject without subject_id")
	}
	if called {
		t.Error("expected no HTTP call for invalid list_facts subject")
	}
}

// ── §14 case 9: client-side validation makes no HTTP call ─────────────────

func TestGraphValidationNoHTTP(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)
	ctx := context.Background()

	checks := []struct {
		name string
		call func() error
	}{
		{"UpsertEntity empty type", func() error { _, e := mem.UpsertEntity(ctx, "  "); return e }},
		{"GetEntity empty id", func() error { _, e := mem.GetEntity(ctx, ""); return e }},
		{"Relate empty from", func() error { _, e := mem.Relate(ctx, "", "b", "t"); return e }},
		{"Relate empty to", func() error { _, e := mem.Relate(ctx, "a", "", "t"); return e }},
		{"Relate empty type", func() error { _, e := mem.Relate(ctx, "a", "b", " "); return e }},
		{"RememberFact empty predicate", func() error { _, e := mem.RememberFact(ctx, "  ", "v"); return e }},
		{"RememberFact bad cardinality", func() error {
			_, e := mem.RememberFact(ctx, "p", "v", WithCardinality("lots"))
			return e
		}},
		{"FactHistory empty predicate", func() error { _, e := mem.FactHistory(ctx, ""); return e }},
		{"FactHistory entity no id", func() error {
			_, e := mem.FactHistory(ctx, "p", WithHistorySubjectEntity(""))
			return e
		}},
	}
	for _, c := range checks {
		if err := c.call(); err == nil {
			t.Errorf("%s: expected client-side error", c.name)
		}
	}
	if called {
		t.Error("expected no HTTP call for any client-side validation failure")
	}
}

func TestGraphCardinalityAccepted(t *testing.T) {
	// "single" and "multi" pass validation and are sent on the body.
	for _, card := range []string{"single", "multi"} {
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody = decodeBody(t, r)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"fact_id": "f1", "entity_id": "owner-1", "subject_type": "owner", "predicate": "p", "value": "v", "cardinality": card,
			})
		}))
		client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
		mem, _ := NewMemoryWithClient("owner-1", client)
		if _, err := mem.RememberFact(context.Background(), "p", "v", WithCardinality(card)); err != nil {
			t.Fatalf("cardinality %s: %v", card, err)
		}
		if gotBody["cardinality"] != card {
			t.Errorf("expected cardinality=%s in body, got %v", card, gotBody["cardinality"])
		}
		srv.Close()
	}
}

// ── §14 case 8: error passthrough (typed APIError, no wrapping) ───────────

func TestGraphErrorPassthrough402(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(402)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "prepaid credit exhausted",
			"code":  CodeCreditExhausted,
		})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	_, err := mem.UpsertEntity(context.Background(), "person")
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
		t.Error("expected errors.Is(err, ErrCreditExhausted) to hold")
	}
}

func TestGraphErrorPassthrough400(t *testing.T) {
	// A 400 (e.g. engine rejecting a non-scalar value) surfaces as a typed APIError.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "value must be scalar"})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond))
	mem, _ := NewMemoryWithClient("owner-1", client)

	_, err := mem.RememberFact(context.Background(), "p", "v")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("expected 400, got %d", apiErr.StatusCode)
	}
}
