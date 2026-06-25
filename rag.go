package aether

import (
	"regexp"
	"strconv"
	"strings"
)

// DefaultContextTemplate is the per-source template used by FormatContext
// when no WithTemplate option is supplied.
const DefaultContextTemplate = "[Source {i}]\n{text}"

// DefaultContextSeparator is the string joined between formatted sources
// when no WithSeparator option is supplied.
const DefaultContextSeparator = "\n\n"

// formatContextConfig holds resolved options for FormatContext.
type formatContextConfig struct {
	template      string
	separator     string
	preferContent bool
}

// FormatContextOption configures FormatContext.
type FormatContextOption func(*formatContextConfig)

// WithTemplate sets the per-source format string. Available placeholders:
// {i} (1-based source number), {doc_id}, {title} (falls back to {doc_id}
// when nil), {text} (passage or content depending on preference), {score}.
// Numeric placeholders accept a Python-style fixed-precision spec, e.g.
// {score:.1f}.
func WithTemplate(t string) FormatContextOption {
	return func(c *formatContextConfig) { c.template = t }
}

// WithSeparator sets the string joined between formatted sources.
func WithSeparator(s string) FormatContextOption {
	return func(c *formatContextConfig) { c.separator = s }
}

// WithPreferContent makes FormatContext use the full document Content when
// available, falling back to Passage. By default FormatContext prefers the
// matched Passage (the right choice for chunked long-form documents).
func WithPreferContent() FormatContextOption {
	return func(c *formatContextConfig) { c.preferContent = true }
}

var placeholderRe = regexp.MustCompile(`\{(\w+)(?::([^}]+))?\}`)

// renderTemplate substitutes {name} and {name:spec} placeholders from values.
// Supports a tiny subset of Python's format spec: a fixed decimal precision
// (e.g. ".2f") for numbers. Unknown placeholders are left untouched so a
// template typo doesn't silently drop a label.
func renderTemplate(template string, values map[string]any) string {
	return placeholderRe.ReplaceAllStringFunc(template, func(match string) string {
		groups := placeholderRe.FindStringSubmatch(match)
		name := groups[1]
		spec := groups[2]
		raw, ok := values[name]
		if !ok {
			return match
		}
		if spec != "" {
			if f, isFloat := raw.(float64); isFloat {
				if strings.HasPrefix(spec, ".") && strings.HasSuffix(spec, "f") {
					if n, err := strconv.Atoi(spec[1 : len(spec)-1]); err == nil {
						return strconv.FormatFloat(f, 'f', n, 64)
					}
				}
			}
		}
		switch v := raw.(type) {
		case string:
			return v
		case int:
			return strconv.Itoa(v)
		case float64:
			return strconv.FormatFloat(v, 'g', -1, 64)
		default:
			return match
		}
	})
}

// FormatContext formats Retrieve() results into an LLM-ready context string.
//
// The default output looks like:
//
//	[Source 1]
//	<matched passage 1>
//
//	[Source 2]
//	<matched passage 2>
//
// Example:
//
//	results, _ := client.Retrieve(ctx, "How many vacation days do I get?", 3)
//	context := aether.FormatContext(results)
func FormatContext(results []RetrievalResult, opts ...FormatContextOption) string {
	cfg := formatContextConfig{
		template:  DefaultContextTemplate,
		separator: DefaultContextSeparator,
	}
	for _, o := range opts {
		o(&cfg)
	}

	chunks := make([]string, 0, len(results))
	for i, r := range results {
		var passage, content string
		if r.Passage != nil {
			passage = *r.Passage
		}
		content = r.Content
		var text string
		if cfg.preferContent {
			if content != "" {
				text = content
			} else {
				text = passage
			}
		} else {
			if passage != "" {
				text = passage
			} else {
				text = content
			}
		}
		title := r.DocID
		if r.Title != nil && *r.Title != "" {
			title = *r.Title
		}
		chunks = append(chunks, renderTemplate(cfg.template, map[string]any{
			"i":      i + 1,
			"doc_id": r.DocID,
			"title":  title,
			"text":   text,
			"score":  r.Score,
		}))
	}
	return strings.Join(chunks, cfg.separator)
}
