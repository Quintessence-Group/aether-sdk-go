package aether

import (
	"context"
	"errors"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ingestContentTypes maps a lowercased file extension to the MIME type the
// batch-ingest helpers send for it. The map is explicit so common
// document types resolve the same way on every OS regardless of the local mime
// database (e.g. ".md" is not always registered). Anything not listed falls
// back to mime.TypeByExtension, then "application/octet-stream".
var ingestContentTypes = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".text":     "text/plain",
	".pdf":      "application/pdf",
	".csv":      "text/csv",
	".json":     "application/json",
	".html":     "text/html",
	".htm":      "text/html",
}

// resolveIngestContentType returns the content type IngestFiles uses for a
// path: the explicit ingest map first, then mime.TypeByExtension, then
// "application/octet-stream". mime.TypeByExtension may return a value with a
// charset parameter (e.g. "text/plain; charset=utf-8"); the bare media type is
// kept so it round-trips cleanly through the documents API.
func resolveIngestContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ct, ok := ingestContentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = strings.TrimSpace(ct[:i])
		}
		return ct
	}
	return "application/octet-stream"
}

// IngestFiles ingests many files in one call.
//
// Each path is read and inserted independently via Insert; the content type is
// resolved from the extension (.md/.markdown → text/markdown, .txt/.text →
// text/plain, .pdf → application/pdf, .csv → text/csv, .json →
// application/json, .html/.htm → text/html; otherwise mime.TypeByExtension,
// otherwise application/octet-stream). Chunking uses the server defaults unless
// WithChunking is passed; the same InsertOptions (WithTags/WithEntityID/
// WithSource/...) apply to every file in the batch.
//
// A file the engine rejects — an APIError whose StatusCode is 413 (too large),
// 415 (unsupported media), or 422 (unprocessable — an unknown or binary type
// the parser can't handle) — is reported as Status "skipped" with the error
// text rather than aborting the batch or being silently dropped. A file that
// cannot be read is reported as Status "error". Either way the returned error
// stays nil: per-file outcomes live in the results. A non-nil error is reserved
// for a fatal condition (see IngestDirectory).
//
// Returns one IngestResult per input path, in the same order.
func (c *Client) IngestFiles(ctx context.Context, paths []string, opts ...InsertOption) ([]IngestResult, error) {
	results := make([]IngestResult, 0, len(paths))
	for _, path := range paths {
		contentType := resolveIngestContentType(path)
		data, err := os.ReadFile(path)
		if err != nil {
			results = append(results, IngestResult{
				Path:   path,
				Status: "error",
				Error:  err.Error(),
			})
			continue
		}
		doc, err := c.Insert(ctx, data, filepath.Base(path), contentType, opts...)
		if err != nil {
			results = append(results, IngestResult{
				Path:        path,
				Status:      classifyIngestStatus(err),
				ContentType: contentType,
				Error:       err.Error(),
			})
			continue
		}
		results = append(results, IngestResult{
			Path:        path,
			Status:      "ingested",
			DocID:       doc.DocID,
			ContentType: contentType,
		})
	}
	return results, nil
}

// classifyIngestStatus maps an insert error to an IngestResult status. A
// per-file rejection the caller can't fix by retrying — an *APIError with
// StatusCode 413/415/422 — becomes "skipped"; everything else is "error".
func classifyIngestStatus(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 413, 415, 422:
			return "skipped"
		}
	}
	return "error"
}

// ingestConfig holds the directory-specific knobs for IngestDirectory plus the
// InsertOptions that get forwarded to each insert.
type ingestConfig struct {
	recursive  bool
	extensions map[string]struct{}
	insertOpts []InsertOption
}

// IngestOption configures IngestDirectory. The directory-specific options
// (WithRecursive, WithExtensions) live alongside WithInsertOptions, which
// forwards the standard InsertOptions (WithTags/WithChunking/WithEntityID/
// WithSource/...) to every file in the walk. Using one option type keeps the
// directory call a single variadic.
type IngestOption func(*ingestConfig)

// WithRecursive controls whether IngestDirectory descends into subdirectories.
// The default is recursive (true); pass WithRecursive(false) to ingest only the
// files directly inside the directory.
func WithRecursive(recursive bool) IngestOption {
	return func(c *ingestConfig) { c.recursive = recursive }
}

// WithExtensions restricts IngestDirectory to files whose extension is one of
// those given (e.g. ".md", ".txt", ".pdf"). Leading dots and case are optional:
// "md", ".MD", and ".md" all match a Markdown file. With no WithExtensions
// option every regular file is ingested.
func WithExtensions(extensions ...string) IngestOption {
	return func(c *ingestConfig) {
		if c.extensions == nil {
			c.extensions = make(map[string]struct{}, len(extensions))
		}
		for _, e := range extensions {
			if e == "" {
				continue
			}
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			c.extensions[strings.ToLower(e)] = struct{}{}
		}
	}
}

// WithInsertOptions forwards the standard InsertOptions (WithTags,
// WithChunking, WithEntityID, WithSource, ...) to every file IngestDirectory
// inserts, so callers configure tags/chunking/etc. the same way they would for
// a single Insert.
func WithInsertOptions(opts ...InsertOption) IngestOption {
	return func(c *ingestConfig) { c.insertOpts = append(c.insertOpts, opts...) }
}

// IngestDirectory ingests every file under dir.
//
// It walks dir — recursively by default; pass WithRecursive(false) to stay at
// the top level — and ingests each matching file via IngestFiles. Pass
// WithExtensions to restrict which files are loaded and WithInsertOptions to
// forward the standard InsertOptions (WithTags/WithChunking/WithEntityID/
// WithSource/...) to each insert. Matched files are ingested in lexical path
// order so results are deterministic.
//
// It returns an error only if dir is not a directory (or cannot be walked);
// per-file rejections are reported in the results exactly as in IngestFiles.
func (c *Client) IngestDirectory(ctx context.Context, dir string, opts ...IngestOption) ([]IngestResult, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "ingest", Path: dir, Err: errors.New("not a directory")}
	}

	cfg := ingestConfig{recursive: true}
	for _, o := range opts {
		o(&cfg)
	}

	var files []string
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip subdirectories (but not dir itself) when not recursive.
			if !cfg.recursive && path != dir {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if cfg.extensions != nil {
			ext := strings.ToLower(filepath.Ext(path))
			if _, ok := cfg.extensions[ext]; !ok {
				return nil
			}
		}
		files = append(files, path)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(files)

	return c.IngestFiles(ctx, files, cfg.insertOpts...)
}
