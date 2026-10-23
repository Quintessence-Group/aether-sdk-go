package aether

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxTextPageBytes is the largest page GetText can return (1 MiB). Hydration
// reads up to this much of a document's text.
const maxTextPageBytes = 1_048_576

// MemoryFileSection is one section of a markdown outline: a top-level ATX
// heading and the lines it spans. Line numbers are 1-based and inclusive;
// Bytes counts lines StartLine..EndLine including their line terminators.
type MemoryFileSection struct {
	// Heading is the heading text; "" for a bare "#".
	Heading string `json:"heading"`
	// Level is the heading level, 1-6.
	Level int `json:"level"`
	// Slug is unique within the text and usable as a section selector.
	Slug      string `json:"slug"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Bytes     int    `json:"bytes"`
}

// DocumentText is the result of GetText (GET /v1/documents/{id}/text).
type DocumentText struct {
	DocID       string `json:"doc_id"`
	ContentType string `json:"content_type"`
	// Source is "original" (a text document's own bytes), "derived" (a media
	// memory's caption/transcript) or "passages" (rebuilt from a parsed
	// document's passages, best effort); nil when State is "unsupported".
	Source *string `json:"source"`
	// State is "ready", or "unsupported" when the document has no readable
	// text (Text is then "").
	State string `json:"state"`
	// Text is this page of the selected text ("" with WithOutline).
	Text string `json:"text"`
	// Offset is the byte offset of this page within the selected text.
	Offset int `json:"offset"`
	// NextOffset is the offset of the next page, or nil on the last page.
	NextOffset *int `json:"next_offset"`
	// TotalBytes is the UTF-8 byte length of the selected text (the whole
	// text, or the section).
	TotalBytes int `json:"total_bytes"`
	// Truncated reports whether more pages follow (NextOffset != nil).
	Truncated bool `json:"truncated"`
	// Section is the selected section when WithTextSection was given.
	Section *MemoryFileSection `json:"section"`
	// Sections is the text's outline when WithOutline was given.
	Sections []MemoryFileSection `json:"sections"`
}

type textConfig struct {
	section  *string
	outline  bool
	offset   *int
	maxBytes *int
}

// TextOption configures GetText.
type TextOption func(*textConfig)

// WithTextSection narrows the text to one markdown section, selected by its
// heading text (case- and whitespace-insensitive) or its exact slug.
func WithTextSection(section string) TextOption {
	return func(c *textConfig) { c.section = &section }
}

// WithOutline returns only the outline (Sections), with Text "".
func WithOutline() TextOption {
	return func(c *textConfig) { c.outline = true }
}

// WithOffset starts the page at this byte offset — a previous page's
// NextOffset.
func WithOffset(offset int) TextOption {
	return func(c *textConfig) { c.offset = &offset }
}

// WithMaxBytes sets the page budget in bytes, 1-1048576 (server default
// 65536).
func WithMaxBytes(maxBytes int) TextOption {
	return func(c *textConfig) { c.maxBytes = &maxBytes }
}

// GetText reads a document's text: the original for text documents, the
// caption or transcript for media, and text rebuilt from the parsed passages
// for PDF, Office and other parsed types.
//
// The result is paged: pass NextOffset back through WithOffset while
// Truncated. WithTextSection narrows the text to one markdown section and
// WithOutline returns only the outline. A document with no readable text
// comes back with State "unsupported". Under a partition handle the partition
// is sent as a guard, exactly as in Get.
func (c *Client) GetText(ctx context.Context, docID string, opts ...TextOption) (*DocumentText, error) {
	if docID == "" {
		return nil, fmt.Errorf("aether: docID cannot be empty")
	}
	var cfg textConfig
	for _, o := range opts {
		o(&cfg)
	}
	params := url.Values{}
	if cfg.section != nil {
		params.Set("section", *cfg.section)
	}
	if cfg.outline {
		params.Set("outline", "true")
	}
	if cfg.offset != nil {
		params.Set("offset", strconv.Itoa(*cfg.offset))
	}
	if cfg.maxBytes != nil {
		params.Set("max_bytes", strconv.Itoa(*cfg.maxBytes))
	}
	c.applyPartitionParam(params)
	path := "/documents/" + url.PathEscape(docID) + "/text"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out DocumentText
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// isTextLikeContentType reports whether a document of this content type can be
// read by decoding its original bytes as UTF-8. text/* qualifies except HTML,
// CSV, TSV and RTF (the server parses those), plus a few structured
// application types; everything else — PDF, Office, octet-stream, a missing
// type — is read through GetText.
func isTextLikeContentType(contentType string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	if strings.HasPrefix(base, "text/") {
		switch base {
		case "text/html", "text/csv", "text/tab-separated-values", "text/rtf":
			return false
		}
		return true
	}
	switch base {
	case "application/json", "application/xml", "application/javascript",
		"application/typescript", "application/x-yaml":
		return true
	}
	return false
}

// hydrateDocumentText reads a non-media document's text for Retrieve,
// Memory.Recall and Memory.List: text-like types are downloaded, anything else
// goes through GetText (up to 1 MiB). When that text is unavailable (State
// "unsupported") or the server answers 404 (an older server, or the document
// vanished), fallback is returned. Other errors propagate.
func (c *Client) hydrateDocumentText(ctx context.Context, docID, contentType, fallback string) (string, error) {
	if isTextLikeContentType(contentType) {
		return c.DownloadText(ctx, docID)
	}
	text, err := c.GetText(ctx, docID, WithMaxBytes(maxTextPageBytes))
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return fallback, nil
		}
		return "", err
	}
	if text.State == "ready" {
		return text.Text, nil
	}
	return fallback, nil
}
