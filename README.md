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
- `Remember(ctx, text, metadata)` stores the memory and writes `metadata` (a
  `map[string]string`) as searchable `key:value` tags.
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
