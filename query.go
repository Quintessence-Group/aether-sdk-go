package aether

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// FieldSchema is a declared typed field for the structured-query layer, returned
// by the schema methods with its live coverage / mismatch stats.
type FieldSchema struct {
	Name string `json:"name"`
	// One of: string, int, float, bool, datetime, string_list.
	Type string `json:"type"`
	// Where the value comes from: {"metadata": "<key>"} or {"regex": "<pattern>"}.
	Source map[string]any `json:"source,omitempty"`
	// Hard-partition scope, or nil for a tenant-wide field.
	PartitionScope *string `json:"partition_scope,omitempty"`
	// Active documents whose source value coerced to the declared type.
	Coverage int `json:"coverage"`
	// Active documents whose source value was present but failed to coerce.
	MismatchCount int `json:"mismatch_count"`
	// Backfill state; "complete" in v1 (synchronous on declare).
	Backfill string `json:"backfill,omitempty"`
}

// FieldInput declares (or replaces) one typed field via DeclareFields. Source is
// {"metadata": "<key>"} or {"regex": "<pattern>"}; Type is one of string, int,
// float, bool, datetime, string_list.
type FieldInput struct {
	Name           string         `json:"name"`
	Type           string         `json:"type"`
	Source         map[string]any `json:"source"`
	PartitionScope *string        `json:"partition_scope,omitempty"`
}

// QuerySort orders a query by a field (Mode A) or an aggregate output / group key
// (Mode B). Dir is "asc" or "desc"; absent values sort last.
type QuerySort struct {
	By  string `json:"by"`
	Dir string `json:"dir,omitempty"`
}

// QueryRequest is the body of a structured analytical query (POST /v1/query).
// The presence of Aggregate selects Mode B (aggregation); otherwise it is Mode A
// (a document page).
type QueryRequest struct {
	// Filter is the unified filter grammar ({and|or|not} over {field, op, value}
	// leaves) or the metadata shorthand map; nil matches every doc in scope.
	Filter any
	// GroupBy names up to two fields to group by (Mode B).
	GroupBy []string
	// Aggregate lists {op, field?, as?} specs; its presence selects Mode B.
	Aggregate []map[string]any
	// Sort orders the results (a field in Mode A; an aggregate output or group
	// key in Mode B).
	Sort []QuerySort
	// Limit caps documents (Mode A, <= 1000) or groups (Mode B).
	Limit *int
	// Offset skips documents (Mode A only).
	Offset int
	// Partition scopes the query; a partition-scoped client (Partition) overrides
	// this field.
	Partition string
}

// QueryPage is the Mode A result: a page of matching documents plus the total
// matched and whether more pages remain.
type QueryPage struct {
	Documents []DocumentRecord `json:"documents"`
	Total     int              `json:"total"`
	HasMore   bool             `json:"has_more"`
}

// QueryGroup is one group in a Mode B aggregation result.
type QueryGroup struct {
	// Keys are the group-by values by field name; empty for a whole-population
	// aggregate.
	Keys map[string]any `json:"keys"`
	// Aggregates are the computed values by output name (the "as" alias or a
	// default).
	Aggregates map[string]any `json:"aggregates"`
}

// AggregateResult is the Mode B result: the matching documents grouped and folded
// into the requested aggregates.
type AggregateResult struct {
	Groups      []QueryGroup `json:"groups"`
	TotalGroups int          `json:"total_groups"`
	Scanned     int          `json:"scanned"`
}

// QueryResponse holds exactly one of Page (Mode A) or Aggregate (Mode B),
// selected by whether the request carried an Aggregate.
type QueryResponse struct {
	// Page is set for a Mode A (no-aggregate) query.
	Page *QueryPage
	// Aggregate is set for a Mode B (aggregation) query.
	Aggregate *AggregateResult
}

// IsAggregate reports whether this response is a Mode B aggregation result.
func (r *QueryResponse) IsAggregate() bool { return r != nil && r.Aggregate != nil }

// Query runs a structured analytical query over the tenant's declared typed
// fields + the built-in record fields (created_at, updated_at, source,
// content_type, tags, entity_id). It is exact and deterministic — it never
// consults an embedding.
//
// Mode A (req.Aggregate empty) returns QueryResponse.Page, a paginated document
// page. Mode B (req.Aggregate set) returns QueryResponse.Aggregate. Guardrail
// violations (the candidate-scan cap, the max-groups cap, an unknown field, a
// type-mismatched literal, or a non-numeric numeric aggregate) fail loud as a
// 400 *APIError, never a truncated success.
func (c *Client) Query(ctx context.Context, req QueryRequest) (*QueryResponse, error) {
	body := map[string]any{}
	if req.Filter != nil {
		body["filter"] = req.Filter
	}
	if len(req.GroupBy) > 0 {
		body["group_by"] = req.GroupBy
	}
	if len(req.Aggregate) > 0 {
		body["aggregate"] = req.Aggregate
	}
	if len(req.Sort) > 0 {
		body["sort"] = req.Sort
	}
	if req.Limit != nil {
		body["limit"] = *req.Limit
	}
	if req.Offset != 0 {
		body["offset"] = req.Offset
	}
	// A partition-scoped handle wins over an explicit request field.
	scope := c.partition
	if scope == "" {
		scope = req.Partition
	}
	if scope != "" {
		body["partition"] = scope
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}

	if len(req.Aggregate) > 0 {
		var out AggregateResult
		if err := c.doJSON(ctx, http.MethodPost, "/query", bytes.NewReader(payload), &out); err != nil {
			return nil, err
		}
		return &QueryResponse{Aggregate: &out}, nil
	}
	var page QueryPage
	if err := c.doJSON(ctx, http.MethodPost, "/query", bytes.NewReader(payload), &page); err != nil {
		return nil, err
	}
	return &QueryResponse{Page: &page}, nil
}

// schemaFieldsResponse is the {fields:[...]} envelope every schema route returns.
type schemaFieldsResponse struct {
	Fields []FieldSchema `json:"fields"`
}

// DeclareFields declares (or replaces) typed fields and returns the declared set.
// Re-declaring a name replaces its type/source and re-backfills. On a
// partition-scoped client the declaration is pinned to that partition.
func (c *Client) DeclareFields(ctx context.Context, fields []FieldInput) ([]FieldSchema, error) {
	payload, err := json.Marshal(map[string]any{"fields": fields})
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var out schemaFieldsResponse
	if err := c.doJSON(ctx, http.MethodPut, c.appendPartitionParam("/schema/fields"), bytes.NewReader(payload), &out); err != nil {
		return nil, err
	}
	return out.Fields, nil
}

// ListFields returns the tenant's declared fields with their live coverage /
// mismatch / backfill stats, scoped to the client's partition when set.
func (c *Client) ListFields(ctx context.Context) ([]FieldSchema, error) {
	var out schemaFieldsResponse
	if err := c.doJSON(ctx, http.MethodGet, c.appendPartitionParam("/schema/fields"), nil, &out); err != nil {
		return nil, err
	}
	return out.Fields, nil
}

// DeleteField removes a declared field and returns the remaining fields, scoped
// to the client's partition when set.
func (c *Client) DeleteField(ctx context.Context, name string) ([]FieldSchema, error) {
	if name == "" {
		return nil, fmt.Errorf("aether: field name cannot be empty")
	}
	var out schemaFieldsResponse
	if err := c.doJSON(ctx, http.MethodDelete, c.appendPartitionParam("/schema/fields/"+url.PathEscape(name)), nil, &out); err != nil {
		return nil, err
	}
	return out.Fields, nil
}
