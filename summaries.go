package aether

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// DocumentSummary is a document's summary and its state, returned by
// GetSummary, PutSummary and RegenerateSummary.
//
// State is one of:
//
//   - "none": the document can be summarized, but nothing is stored or pending.
//   - "pending": a generation is queued or running.
//   - "ready": a summary is stored (Content).
//   - "failed": the latest generation failed (Reason, Error); any previously
//     stored summary is still in Content.
//   - "skipped": automatic generation was skipped by policy (Reason), e.g.
//     the text is too short. RegenerateSummary still generates.
//   - "unsupported": the document can never be summarized (Reason).
//
// Content is independent of State: a stored summary stays readable while a
// regeneration is pending or after one failed. Pending and failed states are
// not durable across a server restart; a summary you requested that reads
// "none" was interrupted, so call RegenerateSummary again.
type DocumentSummary struct {
	// DocID is the summarized (parent) document.
	DocID string `json:"doc_id"`
	// State is "none", "pending", "ready", "failed", "skipped" or
	// "unsupported".
	State string `json:"state"`
	// Reason is a machine-readable reason for "failed", "skipped" or
	// "unsupported", or nil.
	Reason *string `json:"reason"`
	// Error is a human-readable message for "failed", or nil.
	Error *string `json:"error"`
	// SummaryDocID is the document holding the stored summary, or nil.
	SummaryDocID *string `json:"summary_doc_id"`
	// Content is the stored summary markdown, or nil when none is stored.
	Content *string `json:"content"`
	// Revision is the stored summary's opaque revision, or nil. It changes on
	// every stored change; pass it to WithExpectedRevision for a conditional
	// PutSummary. Compare revisions only for equality.
	Revision *string `json:"revision"`
	// AuthoredBy is "aether" (generated) or "user" (written with PutSummary),
	// or nil.
	AuthoredBy *string `json:"authored_by"`
	// Generator identifies what wrote the summary, or nil.
	Generator *string `json:"generator"`
	// PromptVersion is the generation prompt version for a generated summary,
	// or nil.
	PromptVersion *string `json:"prompt_version"`
	// GeneratedAt is the RFC 3339 time the summary was stored, or nil.
	GeneratedAt *string `json:"generated_at"`
	// SourceCID is the document CID the summary was written against, or nil.
	SourceCID *string `json:"source_cid"`
	// Stale is true when a summary is stored but the document has changed
	// since it was written.
	Stale bool `json:"stale"`
	// TruncatedInput is true when the document was too long and the generator
	// saw only its outline and beginning.
	TruncatedInput bool `json:"truncated_input"`
}

type summaryConfig struct {
	expectedRevision *string
}

// SummaryOption configures PutSummary.
type SummaryOption func(*summaryConfig)

// WithExpectedRevision makes PutSummary conditional: the write succeeds only
// while the stored summary's Revision equals rev (sent as If-Match: "rev"). On
// a mismatch the server answers 412 with code CodeSummaryRevisionConflict.
func WithExpectedRevision(rev string) SummaryOption {
	return func(c *summaryConfig) { c.expectedRevision = &rev }
}

// summaryPutRequest is the wire body of PUT /documents/{id}/summary.
type summaryPutRequest struct {
	Content string `json:"content"`
}

// summaryPath is the summary route of a document, with the handle's partition
// guard when the client is scoped.
func (c *Client) summaryPath(docID string, params url.Values) string {
	if params == nil {
		params = url.Values{}
	}
	c.applyPartitionParam(params)
	path := "/documents/" + url.PathEscape(docID) + "/summary"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return path
}

// GetSummary reads a document's summary and its state. A document that exists
// always answers with a state ("none" when nothing was generated); a 404 means
// the document is not visible, or (code CodeFeatureNotEnabled) that summaries
// are not enabled on your deployment. Under a partition handle the partition
// is sent as a guard, exactly as in Get.
func (c *Client) GetSummary(ctx context.Context, docID string) (*DocumentSummary, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	var out DocumentSummary
	if err := c.doJSON(ctx, http.MethodGet, c.summaryPath(docID, nil), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutSummary stores your own summary for a document (1-16384 bytes of
// markdown), replacing any stored one. It is recorded as user-authored and is
// not replaced by RegenerateSummary unless forced. Re-writing the content of a
// stored user-authored summary is a no-op. WithExpectedRevision makes the
// write conditional. Under a partition handle the partition is sent as a
// guard.
func (c *Client) PutSummary(ctx context.Context, docID, content string, opts ...SummaryOption) (*DocumentSummary, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	var cfg summaryConfig
	for _, o := range opts {
		o(&cfg)
	}
	var headers http.Header
	if cfg.expectedRevision != nil {
		if *cfg.expectedRevision == "" {
			return nil, fmt.Errorf("aether: expected revision cannot be empty")
		}
		headers = http.Header{}
		headers.Set("If-Match", quoteETag(*cfg.expectedRevision))
	}
	payload, err := json.Marshal(summaryPutRequest{Content: content})
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode request: %w", err)
	}
	var out DocumentSummary
	if _, err := c.doBodyWithHeaders(
		ctx, http.MethodPut, c.summaryPath(docID, nil), "application/json",
		bytes.NewReader(payload), &out, "", headers,
	); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegenerateSummary queues a fresh generated summary for a document, even one
// below the tenant's minimum length, and returns at once with State "pending";
// poll GetSummary for the result. A call while one is pending joins it. A
// user-authored summary is replaced only when force is true; otherwise the
// server answers 409 with code CodeSummaryUserAuthored. Under a partition
// handle the partition is sent as a guard.
func (c *Client) RegenerateSummary(ctx context.Context, docID string, force bool) (*DocumentSummary, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	params := url.Values{}
	if force {
		params.Set("force", "true")
	}
	var out DocumentSummary
	if err := c.doJSON(ctx, http.MethodPost, c.summaryPath(docID, params), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
