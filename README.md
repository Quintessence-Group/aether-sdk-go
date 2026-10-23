# aether-go

Go SDK for the [Aether](https://aetherdb.ai) decentralized RAG API.

## Installation

```bash
go get github.com/quintessence-group/aether-sdk-go
```

## Memory — the fastest way to build agent memory

For per-user or per-agent memory, reach for the `Memory` facade. Construct it once
with an entity id and every call is automatically scoped to that entity — no tags or
filters to manage:

```go
package main

import (
	"context"
	"fmt"

	aether "github.com/quintessence-group/aether-sdk-go"
)

func main() {
	ctx := context.Background()

	mem, _ := aether.NewMemory("patient-john", aether.WithAPIKey("aether_your_key_here"))

	// Store a memory
	mem.Remember(ctx, "Anxious about flying; uses 4-7-8 breathing", nil)

	// Recall the most relevant memories for this entity
	hits, _ := mem.Recall(ctx, "anxiety coping")
	for _, item := range hits {
		fmt.Printf("%.2f %s\n", *item.Score, item.Text)
	}

	// Newest-first history, or wipe the slate
	mem.List(ctx, aether.WithListLimit(20))
	mem.ForgetAll(ctx)
}
```

- `Recall(ctx, query, WithRecallK(5), WithRecencyWeight(0.0), WithRecallSince(...), WithRecallUntil(...))`
  blends relevance (a calibrated `score`, 0–100) with optional exponential recency decay.
- `Remember(ctx, text, metadata)` stores the memory and attaches `metadata` as
  structured metadata; string-safe entries are also mirrored into searchable
  `key:value` tags.
- `Forget(ctx, id)` deletes one memory; `ForgetAll(ctx)` clears the entity.

The raw `Client` below is the lower-level API — use it when you need direct control
over documents, search, and batch operations rather than entity-scoped memory.

## Quick Start

```go
package main

import (
	"context"
	"fmt"
	"os"

	aether "github.com/quintessence-group/aether-sdk-go"
)

func main() {
	client := aether.New() // reads AETHER_API_KEY from env; defaults to the hosted API at https://api.aetherdb.ai

	// Insert a file — pass empty string for contentType to auto-detect
	data, _ := os.ReadFile("report.pdf")
	doc, _ := client.Insert(context.Background(), data, "report.pdf", "")
	fmt.Printf("Inserted: %s\n", doc.DocID)

	// Insert raw text
	client.InsertText(context.Background(), "Some text content to index", "notes.txt")

	// Search
	results, _ := client.Search(context.Background(), "machine learning", 5)
	for _, r := range results {
		fmt.Printf("  %s (score: %d)\n", r.DocID, r.Score)
	}
}
```

## Content Type Detection

When `contentType` is empty, the SDK auto-detects it from the filename extension using `GuessContentType()`:

```go
aether.GuessContentType("report.pdf")   // "application/pdf"
aether.GuessContentType("data.xlsx")    // "application/vnd.openxmlformats-..."
aether.GuessContentType("notes.txt")    // "text/plain"
aether.GuessContentType("unknown.bin")  // "application/octet-stream"
```

## Supported File Formats

| Format | Extensions |
|--------|-----------|
| PDF | .pdf |
| Word | .docx, .doc |
| PowerPoint | .pptx, .ppt |
| Excel | .xlsx, .xls |
| HTML | .html, .htm |
| CSV | .csv |
| Plain text | .txt, .md, .json, .xml |

Binary-format parsing is handled automatically server-side — no setup required.

## License

MIT

### Memory files

Memory files are **named, versioned markdown files** that belong to the entity — a
running `memory.md` of agreed terms, a `preferences.md`, a project log. Read a file
whole, by section or page by page; replace it with a version precondition or edit
it section by section; list its versions, diff them and grep across files. Every
write creates a new version and keeps the previous ones.

```go
files := mem.Files()

// Create the file (fails with *VersionConflictError if it already exists).
files.Write(ctx, "memory.md", "# Acme memory\n\n## Agreed terms\n", aether.WithCreateOnly())

// Append and AppendSection need no precondition: they apply atomically to
// the current version, so concurrent writers never lose an entry.
files.AppendSection(ctx, "memory.md", "Agreed terms", "- 2026-10-10 — Net 45 for EU entities")
files.Append(ctx, "memory.md", "- Follow up next week")

// Read the whole file, one section, or only the outline.
file, _ := files.Read(ctx, "memory.md")
terms, _ := files.Read(ctx, "memory.md", aether.WithFileSection("Agreed terms"))
outline, _ := files.Read(ctx, "memory.md", aether.WithFileOutline())

// Replace one section's body.
files.ReplaceSection(ctx, "memory.md", "Agreed terms", "- Net 60",
	aether.WithExpectedVersion(file.Version))

// History, diffs and search.
versions, _ := files.Versions(ctx, "memory.md")
diff, _ := files.Diff(ctx, "memory.md") // current version vs the one before
hits, _ := files.Grep(ctx, `net\s+\d+`, aether.WithRegex(), aether.WithIgnoreCase())

// Erase every version.
files.Delete(ctx, "memory.md", aether.WithHardDelete())
```

To rewrite a file safely, read it, edit the content, and write it back with the
version you read; if another writer got there first, read again and retry:

```go
for {
	file, err := files.Read(ctx, "memory.md")
	if err != nil {
		return err
	}
	edited := strings.ReplaceAll(file.Content, "Net 30", "Net 45")
	_, err = files.Write(ctx, "memory.md", edited, aether.WithExpectedVersion(file.Version))
	var conflict *aether.VersionConflictError
	if errors.As(err, &conflict) {
		continue // conflict.CurrentVersion is the newer version
	}
	return err
}
```

- **Names** are 1–128 letters, digits, `.`, `_` or `-`, starting with a letter or
  digit, without `..`, and not `grep` (case-sensitive). `aether.ValidateMemoryFileName`
  checks one; every method rejects an invalid name before sending a request.
- **`Write` and `ReplaceSection` need exactly one precondition**:
  `WithExpectedVersion(v)` to replace version `v`, or `WithCreateOnly()` to create the
  file. `Append` and `AppendSection` need none (pass `WithExpectedVersion` to make
  them conditional anyway). A stale version fails with `*aether.VersionConflictError`
  (`errors.Is(err, aether.ErrVersionConflict)` also works).
- **Writes are safe to retry.** Each write sends an `Idempotency-Key` that is reused
  across the client's automatic retries, so a retried write is applied at most once
  (`WithIdempotencyKey` sets your own). Writing content identical to the current
  version writes nothing and returns `Unchanged: true`.
- **Across entities**, the raw client lists and searches every entity under an id
  prefix: `client.ListMemoryFiles(ctx, aether.WithFileEntityPrefix("acme:customer:"))`
  and `client.GrepMemoryFiles(ctx, "Net 45", aether.WithGrepEntityPrefix("acme:customer:"))`.
- **Delete is soft by default**: the file disappears from reads, lists, grep and
  search, and writing the name again continues its numbering. `WithHardDelete()`
  irreversibly purges every version.
- Search hits on a memory file carry `MemoryFile` (name, version and section).
- A 404 whose `ErrorCode` is `aether.CodeFeatureNotEnabled` means memory files
  aren't enabled on your deployment.

### Reading a document's text

`GetText` returns the parsed, readable text of any stored document — the original
for text types, the caption or transcript for media, and text rebuilt from the
parsed passages for PDF/Office — paged and addressable by section:

```go
outline, _ := client.GetText(ctx, docID, aether.WithOutline())
for _, s := range outline.Sections {
	fmt.Println(s.Heading)
}

terms, _ := client.GetText(ctx, docID, aether.WithTextSection("Payment terms"))
fmt.Println(terms.Text)

page, _ := client.GetText(ctx, docID, aether.WithMaxBytes(65536))
for page.Truncated {
	page, _ = client.GetText(ctx, docID, aether.WithOffset(*page.NextOffset), aether.WithMaxBytes(65536))
}
```

### Document summaries

Aether can keep a short markdown summary next to a document. Ask for one at insert
with `WithSummarize(true)` (`false` suppresses it; without the option the tenant
default applies). It is generated in the background after the insert commits, so it
never fails or slows the insert: `DocumentRecord.Summary` reports `"pending"` or
`"skipped"`. `WithSummarize` also applies to `Insert`, `InsertStream`, `InsertAsync`
(`JobStatus.SummaryState`) and `BatchInsert` (plus `BatchInsertItem.Summarize` per
document).

```go
doc, _ := client.InsertText(ctx, contract, "msa.md", aether.WithSummarize(true))

summary, _ := client.GetSummary(ctx, doc.DocID)
if summary.State == "ready" {
	fmt.Println(*summary.Content)
}

// Write your own (user-authored) summary. WithExpectedRevision(rev) makes it
// conditional on the stored summary's Revision.
client.PutSummary(ctx, doc.DocID, "## Summary\n- Net 30, renews yearly.\n")

// Queue a fresh generated summary; force replaces a user-authored one.
client.RegenerateSummary(ctx, doc.DocID, true)
```

`State` is `none`, `pending`, `ready`, `failed`, `skipped` (by policy, e.g. a short
document) or `unsupported`. `Content` stays readable while a regeneration is pending
or after one failed, and `Stale` is true once the document has changed since the
summary was written. A 404 whose `ErrorCode` is `aether.CodeFeatureNotEnabled` means
summaries aren't enabled on your deployment.
