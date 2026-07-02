package aether

// Memory graph facade (Part II of the Memory surface).
//
// This file extends the entity-scoped Memory facade (memory.go) with the
// engine's typed memory graph: entities, directed relationships, temporal facts
// (with deterministic contradiction resolution), and consolidation. Unlike Part
// I, these methods are not sugar over insert_text/retrieve — they call the new
// /v1/memory/* engine routes. The facade still adds no transport of its own: every
// graph call flows through graphRequest, which injects the owner entity_id and
// the owned client's partition and reuses the raw client's URL building, retry,
// error mapping, and timeout semantics unchanged.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ── Result types ──────────────────────────────────────────────────────────
//
// Read models mirror the engine response DTOs 1:1. Timestamps are RFC 3339
// strings, left unparsed. attributes and a fact value are scalar JSON (string,
// number, bool, or null) — never nested objects/arrays. Nullable fields are
// pointers so an absent value is distinguishable from a zero value.

// MemoryEntity is a typed node in the owner's memory graph (/v1/memory/entities).
type MemoryEntity struct {
	// MemoryEntityID is engine-minted unless you supply one (an idempotency key).
	MemoryEntityID string `json:"memory_entity_id"`
	// EntityID is the owner scope (= the Memory's entity id).
	EntityID string `json:"entity_id"`
	// Partition is the scope's partition, or nil.
	Partition *string `json:"partition,omitempty"`
	// EntityType is caller-controlled ("person", "project", "preference", …).
	EntityType string `json:"entity_type"`
	// DisplayName is an optional label.
	DisplayName *string `json:"display_name,omitempty"`
	// Aliases is a possibly-empty list of alternate names.
	Aliases []string `json:"aliases,omitempty"`
	// Attributes is a possibly-empty map of scalar attributes.
	Attributes map[string]any `json:"attributes,omitempty"`
	// CreatedAt is the RFC 3339 creation timestamp.
	CreatedAt string `json:"created_at"`
	// UpdatedAt is the RFC 3339 last-modified timestamp.
	UpdatedAt string `json:"updated_at"`
}

// MemoryRelationship is a directed, typed edge between two entities
// (/v1/memory/relationships).
type MemoryRelationship struct {
	// RelationshipID is engine-minted unless supplied.
	RelationshipID string `json:"relationship_id"`
	// EntityID is the owner scope.
	EntityID string `json:"entity_id"`
	// Partition is the scope's partition, or nil.
	Partition *string `json:"partition,omitempty"`
	// FromEntityID is the source memory_entity_id. Edges are directional.
	FromEntityID string `json:"from_entity_id"`
	// ToEntityID is the target memory_entity_id.
	ToEntityID string `json:"to_entity_id"`
	// RelationshipType is caller-controlled ("works_at", "owns", "prefers", …).
	RelationshipType string `json:"relationship_type"`
	// Attributes is a possibly-empty map of scalar attributes.
	Attributes map[string]any `json:"attributes,omitempty"`
	// ValidFrom is when the edge became true, if known (RFC 3339), or nil.
	ValidFrom *string `json:"valid_from,omitempty"`
	// ObservedAt is when Aether ingested it (RFC 3339).
	ObservedAt string `json:"observed_at"`
	// InvalidFrom is nil while active; set when the edge is retracted/superseded.
	InvalidFrom *string `json:"invalid_from,omitempty"`
	// CreatedAt is the RFC 3339 creation timestamp.
	CreatedAt string `json:"created_at"`
	// UpdatedAt is the RFC 3339 last-modified timestamp.
	UpdatedAt string `json:"updated_at"`
}

// MemoryFact is the atomic temporal assertion unit — it drives contradiction
// resolution and history (/v1/memory/facts).
type MemoryFact struct {
	// FactID is engine-minted.
	FactID string `json:"fact_id"`
	// EntityID is the owner scope.
	EntityID string `json:"entity_id"`
	// Partition is the scope's partition, or nil.
	Partition *string `json:"partition,omitempty"`
	// SubjectType is "owner", "entity", or "relationship".
	SubjectType string `json:"subject_type"`
	// SubjectID is nil for "owner"; the node/edge id otherwise.
	SubjectID *string `json:"subject_id,omitempty"`
	// Predicate is caller-controlled ("favorite_color", "status", …).
	Predicate string `json:"predicate"`
	// Value is the scalar assertion value (string, number, bool, or null).
	Value any `json:"value"`
	// Cardinality is "single" (default) or "multi".
	Cardinality string `json:"cardinality"`
	// ValidFrom is the semantic effective time, if known (RFC 3339), or nil.
	ValidFrom *string `json:"valid_from,omitempty"`
	// ObservedAt is the ingest time (RFC 3339).
	ObservedAt string `json:"observed_at"`
	// InvalidFrom is nil while active; set when superseded/retracted.
	InvalidFrom *string `json:"invalid_from,omitempty"`
	// SupersedesFactID is the prior active fact this one replaced, or nil.
	SupersedesFactID *string `json:"supersedes_fact_id,omitempty"`
	// CreatedAt is the RFC 3339 creation timestamp.
	CreatedAt string `json:"created_at"`
	// UpdatedAt is the RFC 3339 last-modified timestamp.
	UpdatedAt string `json:"updated_at"`
}

// ConsolidationReport is returned by Consolidate (POST /v1/memory/consolidate).
type ConsolidationReport struct {
	// ActiveFactsBefore is the count of active facts in scope before consolidation.
	ActiveFactsBefore int `json:"active_facts_before"`
	// ActiveFactsAfter is the count of active facts remaining after.
	ActiveFactsAfter int `json:"active_facts_after"`
	// Retracted is the count of redundant facts soft-retracted (kept in history).
	Retracted int `json:"retracted"`
}

// envelope wrappers for the list endpoints. The engine returns
// {<plural>: [...], count} — the count echo is dropped (callers use len).

type entitiesEnvelope struct {
	Entities []MemoryEntity `json:"entities"`
}

type relationshipsEnvelope struct {
	Relationships []MemoryRelationship `json:"relationships"`
}

type factsEnvelope struct {
	Facts []MemoryFact `json:"facts"`
}

// ── Transport ─────────────────────────────────────────────────────────────

// validSubjectTypes is the set of accepted fact subject types (§13).
var validSubjectTypes = map[string]bool{
	"owner":        true,
	"entity":       true,
	"relationship": true,
}

// graphRequest executes a /v1/memory/* graph request scoped to this Memory. It
// injects entity_id (the owner) and the owned client's partition, builds the
// query string, marshals the optional body, and reuses the raw client's
// retry/error transport unchanged (doJSON). body may be nil for GETs and the
// bodyless consolidate POST; build it as a map[string]any so unset optional
// keys are omitted exactly like the Python reference.
func (m *Memory) graphRequest(ctx context.Context, method, path string, params url.Values, body map[string]any, result any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("entity_id", m.entityID)
	m.client.applyPartitionParam(params)

	fullPath := path + "?" + params.Encode()

	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("aether: failed to encode request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	// A nil *bytes.Reader passed as io.Reader would be a non-nil interface, so
	// dispatch explicitly to keep doJSON's body==nil fast path.
	if reader == nil {
		return m.client.doJSON(ctx, method, fullPath, nil, result)
	}
	return m.client.doJSON(ctx, method, fullPath, reader, result)
}

// ── Entities ──────────────────────────────────────────────────────────────

// entityConfig holds optional fields for UpsertEntity.
type entityConfig struct {
	memoryEntityID string
	displayName    *string
	aliases        []string
	attributes     map[string]any
}

// EntityOption configures an UpsertEntity call.
type EntityOption func(*entityConfig)

// WithMemoryEntityID supplies the node id (an idempotency key) to update an
// existing entity; omit it to mint a new node.
func WithMemoryEntityID(id string) EntityOption {
	return func(c *entityConfig) { c.memoryEntityID = id }
}

// WithDisplayName sets the entity's optional display label.
func WithDisplayName(name string) EntityOption {
	return func(c *entityConfig) { c.displayName = &name }
}

// WithAliases sets the entity's alternate names.
func WithAliases(aliases []string) EntityOption {
	return func(c *entityConfig) { c.aliases = aliases }
}

// WithEntityAttributes sets the entity's scalar attributes.
func WithEntityAttributes(attrs map[string]any) EntityOption {
	return func(c *entityConfig) { c.attributes = attrs }
}

// UpsertEntity creates or updates a typed entity node in this owner's graph
// (POST /v1/memory/entities). Omit WithMemoryEntityID to mint a new node; pass an
// existing id (or an idempotency key) to update it. attributes values must be
// scalar. entityType must be non-empty/non-whitespace (validated client-side).
func (m *Memory) UpsertEntity(ctx context.Context, entityType string, opts ...EntityOption) (*MemoryEntity, error) {
	if strings.TrimSpace(entityType) == "" {
		return nil, fmt.Errorf("aether: entity_type cannot be empty")
	}
	var cfg entityConfig
	for _, o := range opts {
		o(&cfg)
	}
	body := map[string]any{"entity_type": entityType}
	if cfg.memoryEntityID != "" {
		body["memory_entity_id"] = cfg.memoryEntityID
	}
	if cfg.displayName != nil {
		body["display_name"] = *cfg.displayName
	}
	if cfg.aliases != nil {
		body["aliases"] = cfg.aliases
	}
	if cfg.attributes != nil {
		body["attributes"] = cfg.attributes
	}
	var entity MemoryEntity
	if err := m.graphRequest(ctx, http.MethodPost, "/memory/entities", nil, body, &entity); err != nil {
		return nil, err
	}
	return &entity, nil
}

// GetEntity fetches one entity node by id (GET /v1/memory/entities/{id}).
// memoryEntityID must be non-empty (validated client-side).
func (m *Memory) GetEntity(ctx context.Context, memoryEntityID string) (*MemoryEntity, error) {
	if memoryEntityID == "" {
		return nil, fmt.Errorf("aether: memory_entity_id cannot be empty")
	}
	path := "/memory/entities/" + url.PathEscape(memoryEntityID)
	var entity MemoryEntity
	if err := m.graphRequest(ctx, http.MethodGet, path, nil, nil, &entity); err != nil {
		return nil, err
	}
	return &entity, nil
}

// entityListConfig holds optional filters for ListEntities.
type entityListConfig struct {
	entityType string
	limit      *int
}

// EntityListOption configures a ListEntities call.
type EntityListOption func(*entityListConfig)

// WithEntityType filters listed entities by type.
func WithEntityType(entityType string) EntityListOption {
	return func(c *entityListConfig) { c.entityType = entityType }
}

// WithEntityLimit caps the number of entities returned.
func WithEntityLimit(limit int) EntityListOption {
	return func(c *entityListConfig) { c.limit = &limit }
}

// ListEntities lists this owner's entity nodes, optionally filtered by type and
// limit (GET /v1/memory/entities). Unset filters are absent from the query.
func (m *Memory) ListEntities(ctx context.Context, opts ...EntityListOption) ([]MemoryEntity, error) {
	var cfg entityListConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{}
	if cfg.entityType != "" {
		params.Set("entity_type", cfg.entityType)
	}
	if cfg.limit != nil {
		params.Set("limit", strconv.Itoa(*cfg.limit))
	}
	var env entitiesEnvelope
	if err := m.graphRequest(ctx, http.MethodGet, "/memory/entities", params, nil, &env); err != nil {
		return nil, err
	}
	return env.Entities, nil
}

// ── Relationships ─────────────────────────────────────────────────────────

// relationshipConfig holds optional fields for Relate.
type relationshipConfig struct {
	relationshipID string
	attributes     map[string]any
	validFrom      *string
}

// RelationshipOption configures a Relate call.
type RelationshipOption func(*relationshipConfig)

// WithRelationshipID supplies the edge id (an idempotency key) to update an
// existing relationship; omit it to mint a new edge.
func WithRelationshipID(id string) RelationshipOption {
	return func(c *relationshipConfig) { c.relationshipID = id }
}

// WithRelationshipAttributes sets the edge's scalar attributes.
func WithRelationshipAttributes(attrs map[string]any) RelationshipOption {
	return func(c *relationshipConfig) { c.attributes = attrs }
}

// WithRelationshipValidFrom sets when the edge became true (RFC 3339).
func WithRelationshipValidFrom(ts string) RelationshipOption {
	return func(c *relationshipConfig) { c.validFrom = &ts }
}

// Relate creates or updates a directed edge between two entity nodes
// (POST /v1/memory/relationships). fromEntityID, toEntityID, and relationshipType
// must each be non-empty (validated client-side).
func (m *Memory) Relate(ctx context.Context, fromEntityID, toEntityID, relationshipType string, opts ...RelationshipOption) (*MemoryRelationship, error) {
	if strings.TrimSpace(fromEntityID) == "" {
		return nil, fmt.Errorf("aether: from_entity_id cannot be empty")
	}
	if strings.TrimSpace(toEntityID) == "" {
		return nil, fmt.Errorf("aether: to_entity_id cannot be empty")
	}
	if strings.TrimSpace(relationshipType) == "" {
		return nil, fmt.Errorf("aether: relationship_type cannot be empty")
	}
	var cfg relationshipConfig
	for _, o := range opts {
		o(&cfg)
	}
	body := map[string]any{
		"from_entity_id":    fromEntityID,
		"to_entity_id":      toEntityID,
		"relationship_type": relationshipType,
	}
	if cfg.relationshipID != "" {
		body["relationship_id"] = cfg.relationshipID
	}
	if cfg.attributes != nil {
		body["attributes"] = cfg.attributes
	}
	if cfg.validFrom != nil {
		body["valid_from"] = *cfg.validFrom
	}
	var rel MemoryRelationship
	if err := m.graphRequest(ctx, http.MethodPost, "/memory/relationships", nil, body, &rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// relationshipListConfig holds optional filters for ListRelationships.
type relationshipListConfig struct {
	fromEntityID     string
	toEntityID       string
	relationshipType string
	includeInactive  bool
	asOf             string
	limit            *int
}

// RelationshipListOption configures a ListRelationships call.
type RelationshipListOption func(*relationshipListConfig)

// WithRelationshipsFrom filters edges by source entity id.
func WithRelationshipsFrom(id string) RelationshipListOption {
	return func(c *relationshipListConfig) { c.fromEntityID = id }
}

// WithRelationshipsTo filters edges by target entity id.
func WithRelationshipsTo(id string) RelationshipListOption {
	return func(c *relationshipListConfig) { c.toEntityID = id }
}

// WithRelationshipsType filters edges by relationship type.
func WithRelationshipsType(relationshipType string) RelationshipListOption {
	return func(c *relationshipListConfig) { c.relationshipType = relationshipType }
}

// WithIncludeInactiveRelationships includes retracted/superseded edges. The
// server defaults this false; the param is only sent when true.
func WithIncludeInactiveRelationships(include bool) RelationshipListOption {
	return func(c *relationshipListConfig) { c.includeInactive = include }
}

// WithRelationshipsAsOf returns edges active at the given RFC 3339 instant.
func WithRelationshipsAsOf(ts string) RelationshipListOption {
	return func(c *relationshipListConfig) { c.asOf = ts }
}

// WithRelationshipsLimit caps the number of edges returned.
func WithRelationshipsLimit(limit int) RelationshipListOption {
	return func(c *relationshipListConfig) { c.limit = &limit }
}

// ListRelationships lists edges, optionally filtered (GET /v1/memory/relationships).
// Unset filters are absent from the query; include_inactive is sent (=true) only
// when true.
func (m *Memory) ListRelationships(ctx context.Context, opts ...RelationshipListOption) ([]MemoryRelationship, error) {
	var cfg relationshipListConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{}
	if cfg.fromEntityID != "" {
		params.Set("from_entity_id", cfg.fromEntityID)
	}
	if cfg.toEntityID != "" {
		params.Set("to_entity_id", cfg.toEntityID)
	}
	if cfg.relationshipType != "" {
		params.Set("relationship_type", cfg.relationshipType)
	}
	if cfg.includeInactive {
		params.Set("include_inactive", "true")
	}
	if cfg.asOf != "" {
		params.Set("as_of", cfg.asOf)
	}
	if cfg.limit != nil {
		params.Set("limit", strconv.Itoa(*cfg.limit))
	}
	var env relationshipsEnvelope
	if err := m.graphRequest(ctx, http.MethodGet, "/memory/relationships", params, nil, &env); err != nil {
		return nil, err
	}
	return env.Relationships, nil
}

// ── Facts ─────────────────────────────────────────────────────────────────

// factConfig holds the resolved subject and optional fields for RememberFact.
type factConfig struct {
	subjectType      string
	subjectID        string
	cardinality      string
	validFrom        *string
	observedAt       *string
	supersedesFactID string
}

// FactOption configures a RememberFact call.
type FactOption func(*factConfig)

// WithFactSubjectEntity scopes the fact to an entity node (subject_type=entity,
// subject_id=id).
func WithFactSubjectEntity(id string) FactOption {
	return func(c *factConfig) {
		c.subjectType = "entity"
		c.subjectID = id
	}
}

// WithFactSubjectRelationship scopes the fact to a relationship edge
// (subject_type=relationship, subject_id=id).
func WithFactSubjectRelationship(id string) FactOption {
	return func(c *factConfig) {
		c.subjectType = "relationship"
		c.subjectID = id
	}
}

// WithCardinality sets the fact cardinality ("single" or "multi").
func WithCardinality(cardinality string) FactOption {
	return func(c *factConfig) { c.cardinality = cardinality }
}

// WithFactValidFrom sets the fact's semantic effective time (RFC 3339).
func WithFactValidFrom(ts string) FactOption {
	return func(c *factConfig) { c.validFrom = &ts }
}

// WithObservedAt sets the fact's ingest time override (RFC 3339).
func WithObservedAt(ts string) FactOption {
	return func(c *factConfig) { c.observedAt = &ts }
}

// WithSupersedesFactID names the prior active fact this assertion replaces.
func WithSupersedesFactID(id string) FactOption {
	return func(c *factConfig) { c.supersedesFactID = id }
}

// RememberFact asserts a temporal fact about the owner (default), an entity, or
// a relationship (POST /v1/memory/facts). A newer single-valued fact with the same
// (subject, predicate) supersedes the prior one server-side, keeping it in
// history (ADR-019). value must be scalar (string, number, bool, or nil) and is
// always sent, even when nil.
//
// Client-side validation: predicate non-empty; a non-owner subject requires a
// subject_id (set via WithFactSubjectEntity/WithFactSubjectRelationship);
// cardinality, when set, must be "single" or "multi".
func (m *Memory) RememberFact(ctx context.Context, predicate string, value any, opts ...FactOption) (*MemoryFact, error) {
	cfg := factConfig{subjectType: "owner"}
	for _, o := range opts {
		o(&cfg)
	}
	if strings.TrimSpace(predicate) == "" {
		return nil, fmt.Errorf("aether: predicate cannot be empty")
	}
	if err := validateSubject(cfg.subjectType, cfg.subjectID); err != nil {
		return nil, err
	}
	if err := validateCardinality(cfg.cardinality); err != nil {
		return nil, err
	}
	// value is ALWAYS sent, even when nil (engine surface requirement).
	body := map[string]any{
		"subject_type": cfg.subjectType,
		"predicate":    predicate,
		"value":        value,
	}
	if cfg.subjectType != "owner" {
		body["subject_id"] = cfg.subjectID
	}
	if cfg.cardinality != "" {
		body["cardinality"] = cfg.cardinality
	}
	if cfg.validFrom != nil {
		body["valid_from"] = *cfg.validFrom
	}
	if cfg.observedAt != nil {
		body["observed_at"] = *cfg.observedAt
	}
	if cfg.supersedesFactID != "" {
		body["supersedes_fact_id"] = cfg.supersedesFactID
	}
	var fact MemoryFact
	if err := m.graphRequest(ctx, http.MethodPost, "/memory/facts", nil, body, &fact); err != nil {
		return nil, err
	}
	return &fact, nil
}

// factListConfig holds optional filters for ListFacts.
type factListConfig struct {
	subjectType     string
	subjectID       string
	predicate       string
	includeInactive bool
	asOf            string
	limit           *int
}

// FactListOption configures a ListFacts call.
type FactListOption func(*factListConfig)

// WithFactsSubjectEntity filters facts to an entity subject (subject_type=entity,
// subject_id=id).
func WithFactsSubjectEntity(id string) FactListOption {
	return func(c *factListConfig) {
		c.subjectType = "entity"
		c.subjectID = id
	}
}

// WithFactsSubjectRelationship filters facts to a relationship subject
// (subject_type=relationship, subject_id=id).
func WithFactsSubjectRelationship(id string) FactListOption {
	return func(c *factListConfig) {
		c.subjectType = "relationship"
		c.subjectID = id
	}
}

// WithFactsPredicate filters facts by predicate.
func WithFactsPredicate(predicate string) FactListOption {
	return func(c *factListConfig) { c.predicate = predicate }
}

// WithIncludeInactiveFacts includes superseded/retracted facts. The server
// defaults this false; the param is only sent when true.
func WithIncludeInactiveFacts(include bool) FactListOption {
	return func(c *factListConfig) { c.includeInactive = include }
}

// WithFactsAsOf returns facts active at the given RFC 3339 instant.
func WithFactsAsOf(ts string) FactListOption {
	return func(c *factListConfig) { c.asOf = ts }
}

// WithFactsLimit caps the number of facts returned.
func WithFactsLimit(limit int) FactListOption {
	return func(c *factListConfig) { c.limit = &limit }
}

// ListFacts lists active facts (default), or includes superseded/retracted with
// WithIncludeInactiveFacts (GET /v1/memory/facts). When a subject is provided it
// must be valid and a non-owner subject requires a subject_id (the selector).
func (m *Memory) ListFacts(ctx context.Context, opts ...FactListOption) ([]MemoryFact, error) {
	var cfg factListConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{}
	if cfg.subjectType != "" {
		if err := validateSubject(cfg.subjectType, cfg.subjectID); err != nil {
			return nil, err
		}
		params.Set("subject_type", cfg.subjectType)
		if cfg.subjectType != "owner" {
			params.Set("subject_id", cfg.subjectID)
		}
	}
	if cfg.predicate != "" {
		params.Set("predicate", cfg.predicate)
	}
	if cfg.includeInactive {
		params.Set("include_inactive", "true")
	}
	if cfg.asOf != "" {
		params.Set("as_of", cfg.asOf)
	}
	if cfg.limit != nil {
		params.Set("limit", strconv.Itoa(*cfg.limit))
	}
	var env factsEnvelope
	if err := m.graphRequest(ctx, http.MethodGet, "/memory/facts", params, nil, &env); err != nil {
		return nil, err
	}
	return env.Facts, nil
}

// factHistoryConfig holds the resolved subject for FactHistory.
type factHistoryConfig struct {
	subjectType string
	subjectID   string
}

// FactHistoryOption configures a FactHistory call.
type FactHistoryOption func(*factHistoryConfig)

// WithHistorySubjectEntity scopes the history to an entity subject
// (subject_type=entity, subject_id=id).
func WithHistorySubjectEntity(id string) FactHistoryOption {
	return func(c *factHistoryConfig) {
		c.subjectType = "entity"
		c.subjectID = id
	}
}

// WithHistorySubjectRelationship scopes the history to a relationship subject
// (subject_type=relationship, subject_id=id).
func WithHistorySubjectRelationship(id string) FactHistoryOption {
	return func(c *factHistoryConfig) {
		c.subjectType = "relationship"
		c.subjectID = id
	}
}

// FactHistory returns the full assertion chain (active + superseded) for one
// (subject, predicate) (GET /v1/memory/facts?history=true). It always sends
// history=true, subject_type, and predicate (plus subject_id for a non-owner
// subject). predicate must be non-empty.
func (m *Memory) FactHistory(ctx context.Context, predicate string, opts ...FactHistoryOption) ([]MemoryFact, error) {
	cfg := factHistoryConfig{subjectType: "owner"}
	for _, o := range opts {
		o(&cfg)
	}
	if strings.TrimSpace(predicate) == "" {
		return nil, fmt.Errorf("aether: predicate cannot be empty")
	}
	if err := validateSubject(cfg.subjectType, cfg.subjectID); err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Set("history", "true")
	params.Set("subject_type", cfg.subjectType)
	params.Set("predicate", predicate)
	if cfg.subjectType != "owner" {
		params.Set("subject_id", cfg.subjectID)
	}
	var env factsEnvelope
	if err := m.graphRequest(ctx, http.MethodGet, "/memory/facts", params, nil, &env); err != nil {
		return nil, err
	}
	return env.Facts, nil
}

// ── Consolidation ─────────────────────────────────────────────────────────

// Consolidate soft-retracts redundant facts in this scope and returns a report
// (POST /v1/memory/consolidate). It sends no body.
func (m *Memory) Consolidate(ctx context.Context) (*ConsolidationReport, error) {
	var report ConsolidationReport
	// No body: send entity_id (+ partition) on the query, POST with no payload.
	if err := m.graphRequest(ctx, http.MethodPost, "/memory/consolidate", nil, nil, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// ── Client-side validation ────────────────────────────────────────────────

// validateSubject checks (subjectType, subjectID) client-side: subjectType must
// be one of owner|entity|relationship, and a non-owner subject requires a
// non-empty subjectID.
func validateSubject(subjectType, subjectID string) error {
	if !validSubjectTypes[subjectType] {
		return fmt.Errorf("aether: subject_type must be 'owner', 'entity', or 'relationship'")
	}
	if subjectType != "owner" && subjectID == "" {
		return fmt.Errorf("aether: subject_id is required when subject_type is '%s'", subjectType)
	}
	return nil
}

// validateCardinality checks that a set cardinality is "single" or "multi".
// An empty string means unset and is allowed.
func validateCardinality(cardinality string) error {
	if cardinality == "" {
		return nil
	}
	if cardinality != "single" && cardinality != "multi" {
		return fmt.Errorf("aether: cardinality must be 'single' or 'multi'")
	}
	return nil
}
