package aether

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MediaMemoryOptions configures Client.RememberMedia. The raw client accepts
// already-loaded bytes only; use Memory.RememberImage or Memory.RememberAudio
// when the input is a local path or an HTTP(S) URL.
type MediaMemoryOptions struct {
	// Modality is required and must be "image" or "audio".
	Modality string
	// ContentType is required and must describe the supplied media bytes. It is
	// normalized to its lower-case MIME type without parameters before sending.
	ContentType string
	// EntityID is required and scopes the memory to one entity.
	EntityID string
	// Filename is optional display metadata for the original media bytes.
	Filename string
	// Caption is an optional caller-provided image caption. When empty, the
	// server's configured image processor derives the indexed text.
	Caption string
	// Transcript is an optional caller-provided audio transcript. When empty,
	// the server's configured audio processor derives the indexed text.
	Transcript string
	// Tags are optional legacy metadata tags.
	Tags []string
	// Metadata is optional structured metadata.
	Metadata Metadata
	// Source is an optional origin label, such as "upload" or "camera".
	Source string
	// Readers is an optional read-ACL allowlist. It is sent in the query because
	// it scopes the write rather than becoming document metadata.
	Readers []string
}

// rememberMediaRequest is the JSON wire shape for POST /v1/memory/media.
// It deliberately carries original bytes as standard base64 so every SDK uses
// the same authenticated request body. Caller URLs are fetched by the SDK and
// are never forwarded to the Aether server.
type rememberMediaRequest struct {
	Modality    string   `json:"modality"`
	DataBase64  string   `json:"data_base64"`
	ContentType string   `json:"content_type"`
	Filename    string   `json:"filename,omitempty"`
	Caption     *string  `json:"caption,omitempty"`
	Transcript  *string  `json:"transcript,omitempty"`
	Tags        []string `json:"tags"`
	Metadata    Metadata `json:"metadata,omitempty"`
	Source      string   `json:"source,omitempty"`
}

// RememberMedia stores validated image or audio bytes as an entity-scoped
// memory. The server preserves the original bytes through the normal encrypted
// document workflow and indexes either the supplied caption/transcript or text
// derived by its configured processor.
//
// This raw method accepts bytes only. It never dereferences a caller-supplied
// URL. Use Memory.RememberImage or Memory.RememberAudio for bytes, local paths,
// or HTTP(S) URL normalization in the caller process.
func (c *Client) RememberMedia(ctx context.Context, media []byte, opts MediaMemoryOptions) (*MediaMemoryRecord, error) {
	if len(media) == 0 {
		return nil, fmt.Errorf("aether: media cannot be empty")
	}

	modality := strings.ToLower(strings.TrimSpace(opts.Modality))
	if modality != "image" && modality != "audio" {
		return nil, fmt.Errorf("aether: modality must be 'image' or 'audio'")
	}
	contentType := normalizeMediaContentType(opts.ContentType)
	if contentType == "" {
		return nil, fmt.Errorf("aether: contentType cannot be empty")
	}
	if err := validateEntityID(opts.EntityID); err != nil {
		return nil, err
	}
	if modality == "image" && opts.Transcript != "" {
		return nil, fmt.Errorf("aether: transcript is valid only for audio memories")
	}
	if modality == "audio" && opts.Caption != "" {
		return nil, fmt.Errorf("aether: caption is valid only for image memories")
	}

	params := url.Values{"entity_id": {opts.EntityID}}
	c.applyPartitionParam(params)
	if len(opts.Readers) > 0 {
		params.Set("readers", strings.Join(opts.Readers, ","))
	}

	tags := opts.Tags
	if tags == nil {
		tags = []string{}
	}
	body := rememberMediaRequest{
		Modality:    modality,
		DataBase64:  base64.StdEncoding.EncodeToString(media),
		ContentType: contentType,
		Filename:    opts.Filename,
		Tags:        tags,
		Metadata:    opts.Metadata,
		Source:      opts.Source,
	}
	if opts.Caption != "" {
		caption := opts.Caption
		body.Caption = &caption
	}
	if opts.Transcript != "" {
		transcript := opts.Transcript
		body.Transcript = &transcript
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("aether: failed to encode media request: %w", err)
	}
	var record MediaMemoryRecord
	if err := c.doJSON(ctx, http.MethodPost, "/memory/media?"+params.Encode(), bytes.NewReader(payload), &record); err != nil {
		return nil, err
	}
	return &record, nil
}

// normalizeMediaContentType drops optional MIME parameters and normalizes the
// media type used by the server-side magic validation.
func normalizeMediaContentType(contentType string) string {
	bare, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(bare))
}
