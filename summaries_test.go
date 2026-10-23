package aether

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readySummaryBody() map[string]any {
	return map[string]any{
		"doc_id": "doc-1", "state": "ready", "reason": nil, "error": nil,
		"summary_doc_id": "sum-1", "content": "## Summary\n- Net 30 terms.\n", "revision": "r3f9a1c2b7d4e5f60",
		"authored_by": "aether", "generator": "aether:model-x:2026-10-10.v1", "prompt_version": "2026-10-10.v1",
		"generated_at": "2026-10-10T09:00:00Z", "source_cid": "cid-1", "stale": true, "truncated_input": true,
	}
}

// ── Summaries ─────────────────────────────────────────────────────────────

func TestGetSummaryParsesReady(t *testing.T) {
	s, client := newFileServer(t, okReply(readySummaryBody()))
	scoped, _ := client.Partition(fileHandle)
	summary, err := scoped.GetSummary(context.Background(), "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodGet || req.Path != "/v1/documents/doc-1/summary" || req.Query.Get("partition") != fileHandle {
		t.Errorf("get summary request %s %s %v", req.Method, req.Path, req.Query)
	}
	want := DocumentSummary{
		DocID: "doc-1", State: "ready", SummaryDocID: strPtr("sum-1"),
		Content: strPtr("## Summary\n- Net 30 terms.\n"), Revision: strPtr("r3f9a1c2b7d4e5f60"),
		AuthoredBy: strPtr("aether"), Generator: strPtr("aether:model-x:2026-10-10.v1"),
		PromptVersion: strPtr("2026-10-10.v1"), GeneratedAt: strPtr("2026-10-10T09:00:00Z"),
		SourceCID: strPtr("cid-1"), Stale: true, TruncatedInput: true,
	}
	if !reflect.DeepEqual(*summary, want) {
		t.Errorf("summary = %+v\nwant %+v", *summary, want)
	}
}

func TestGetSummaryParsesNone(t *testing.T) {
	s, client := newFileServer(t, okReply(map[string]any{
		"doc_id": "doc-1", "state": "none", "reason": nil, "error": nil, "summary_doc_id": nil,
		"content": nil, "revision": nil, "authored_by": nil, "generator": nil, "prompt_version": nil,
		"generated_at": nil, "source_cid": nil, "stale": false, "truncated_input": false,
	}))
	summary, err := client.GetSummary(context.Background(), "doc-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (DocumentSummary{DocID: "doc-1", State: "none"}); !reflect.DeepEqual(*summary, want) {
		t.Errorf("summary = %+v", *summary)
	}
	if s.last(t).Query.Has("partition") {
		t.Error("an unscoped client must not send partition")
	}

	// An older server omitting the optional fields still parses.
	_, client = newFileServer(t, okReply(map[string]any{"doc_id": "doc-1", "state": "skipped", "reason": "below_min_chars"}))
	summary, err = client.GetSummary(context.Background(), "doc-1")
	if err != nil || summary.State != "skipped" || summary.Reason == nil || *summary.Reason != "below_min_chars" || summary.Stale {
		t.Errorf("sparse summary: %v %+v", err, summary)
	}
}

func TestPutSummary(t *testing.T) {
	ctx := context.Background()
	s, client := newFileServer(t, fileReply{status: http.StatusCreated, body: readySummaryBody()})
	scoped, _ := client.Partition(fileHandle)

	summary, err := scoped.PutSummary(ctx, "doc-1", "## Summary\n- mine\n")
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodPut || req.Path != "/v1/documents/doc-1/summary" || req.Query.Get("partition") != fileHandle {
		t.Errorf("put summary request %s %s %v", req.Method, req.Path, req.Query)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
	if got, want := req.jsonBody(t), map[string]any{"content": "## Summary\n- mine\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("body %v, want %v", got, want)
	}
	for _, h := range []string{"If-Match", "If-None-Match", "Idempotency-Key"} {
		if _, has := req.Header[h]; has {
			t.Errorf("%s must not be sent", h)
		}
	}
	if summary.State != "ready" || summary.Revision == nil {
		t.Errorf("unexpected summary %+v", summary)
	}

	if _, err := scoped.PutSummary(ctx, "doc-1", "x", WithExpectedRevision("r3f9a1c2b7d4e5f60")); err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).Header.Get("If-Match"); got != `"r3f9a1c2b7d4e5f60"` {
		t.Errorf("If-Match %q", got)
	}
}

func TestRegenerateSummary(t *testing.T) {
	ctx := context.Background()
	pending := readySummaryBody()
	pending["state"] = "pending"
	s, client := newFileServer(t, fileReply{status: http.StatusAccepted, body: pending})
	scoped, _ := client.Partition(fileHandle)

	summary, err := scoped.RegenerateSummary(ctx, "doc-1", true)
	if err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Method != http.MethodPost || req.Path != "/v1/documents/doc-1/summary" ||
		req.Query.Get("force") != "true" || req.Query.Get("partition") != fileHandle {
		t.Errorf("regenerate request %s %s %v", req.Method, req.Path, req.Query)
	}
	if len(req.Body) != 0 {
		t.Errorf("regenerate must send no body, got %q", req.Body)
	}
	if summary.State != "pending" || summary.Content == nil {
		t.Errorf("unexpected 202 body %+v", summary)
	}

	if _, err := client.RegenerateSummary(ctx, "doc-1", false); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; q.Has("force") || q.Has("partition") {
		t.Errorf("unforced, unscoped regenerate must send no params: %v", q)
	}
}

func TestSummaryArgumentErrors(t *testing.T) {
	ctx := context.Background()
	s, client := newFileServer(t)
	cases := map[string]func() error{
		"get empty id": func() error { _, err := client.GetSummary(ctx, ""); return err },
		"put empty id": func() error { _, err := client.PutSummary(ctx, "", "x"); return err },
		"put empty revision": func() error {
			_, err := client.PutSummary(ctx, "doc-1", "x", WithExpectedRevision(""))
			return err
		},
		"regenerate empty id": func() error { _, err := client.RegenerateSummary(ctx, "", true); return err },
	}
	for name, call := range cases {
		if err := call(); err == nil {
			t.Errorf("%s: expected a client-side error", name)
		}
	}
	expectNoRequests(t, s)
}

func TestSummaryErrorsStayBaseAPIError(t *testing.T) {
	ctx := context.Background()
	_, client := newFileServer(t, errorReply(http.StatusPreconditionFailed, CodeSummaryRevisionConflict,
		map[string]any{"current_revision": "r2"}))
	_, err := client.PutSummary(ctx, "doc-1", "x", WithExpectedRevision("r1"))
	var apiErr *APIError
	var conflict *VersionConflictError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 412 || apiErr.ErrorCode != CodeSummaryRevisionConflict {
		t.Errorf("expected a 412 summary_revision_conflict APIError, got %v", err)
	}
	if errors.As(err, &conflict) || errors.Is(err, ErrVersionConflict) {
		t.Error("a summary revision conflict is not a memory-file version conflict")
	}

	_, client = newFileServer(t, errorReply(http.StatusConflict, CodeSummaryUserAuthored, nil))
	_, err = client.RegenerateSummary(ctx, "doc-1", false)
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != CodeSummaryUserAuthored {
		t.Errorf("expected summary_user_authored, got %v", err)
	}

	_, client = newFileServer(t, errorReply(http.StatusNotFound, CodeFeatureNotEnabled, map[string]any{"feature": "summaries"}))
	_, err = client.GetSummary(ctx, "doc-1")
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.ErrorCode != CodeFeatureNotEnabled {
		t.Errorf("expected feature_not_enabled, got %v", err)
	}
}

// ── summarize at ingest ───────────────────────────────────────────────────

func pendingRecord() map[string]any {
	return map[string]any{
		"doc_id": "doc-1", "cid": "c", "content_type": "text/plain", "size_bytes": 4,
		"chunks": 1, "vectors": 1, "version": 1, "summary": "pending",
	}
}

func TestInsertSummarizeIsTriState(t *testing.T) {
	ctx := context.Background()
	s, client := newFileServer(t, okReply(pendingRecord()))

	doc, err := client.InsertText(ctx, "long text", "a.txt", WithSummarize(true))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).Query.Get("summarize"); got != "true" {
		t.Errorf("summarize=%q, want true", got)
	}
	if doc.Summary == nil || *doc.Summary != "pending" {
		t.Errorf("DocumentRecord.Summary = %v", doc.Summary)
	}

	if _, err := client.InsertText(ctx, "long text", "a.txt", WithSummarize(false)); err != nil {
		t.Fatal(err)
	}
	if got := s.last(t).Query.Get("summarize"); got != "false" {
		t.Errorf("summarize=%q, want false", got)
	}

	if _, err := client.InsertText(ctx, "long text", "a.txt"); err != nil {
		t.Fatal(err)
	}
	if s.last(t).Query.Has("summarize") {
		t.Error("summarize must be omitted when unset")
	}

	// A record without the field parses with a nil Summary.
	_, client = newFileServer(t, okReply(map[string]any{"doc_id": "doc-2", "cid": "c", "content_type": "text/plain"}))
	doc, err = client.InsertText(ctx, "x", "a.txt")
	if err != nil || doc.Summary != nil {
		t.Errorf("absent summary: %v %+v", err, doc)
	}
}

func TestSummarizeForwardedByInsertFamily(t *testing.T) {
	ctx := context.Background()
	s, client := newFileServer(t, okReply(pendingRecord()))
	scoped, _ := client.Partition(fileHandle)

	if _, err := scoped.Insert(ctx, []byte("%PDF"), "a.pdf", "", WithSummarize(true)); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; q.Get("summarize") != "true" || s.last(t).Path != "/v1/documents" {
		t.Errorf("insert: %s %v", s.last(t).Path, q)
	}

	if _, err := scoped.InsertStream(ctx, strings.NewReader("streamed"), "a.txt", "", WithSummarize(false)); err != nil {
		t.Fatal(err)
	}
	if q := s.last(t).Query; q.Get("summarize") != "false" {
		t.Errorf("insert stream: %v", q)
	}
}

func TestInsertAsyncForwardsSummarizeAndJobParsesSummaryState(t *testing.T) {
	ctx := context.Background()
	s, client := newFileServer(t,
		fileReply{status: http.StatusAccepted, body: map[string]any{"job_id": "j-1", "status": "pending", "poll_url": "/v1/documents/jobs/j-1"}},
		okReply(map[string]any{"status": "completed", "doc_id": "doc-1", "summary_state": "skipped"}),
	)
	if _, err := client.InsertAsync(ctx, []byte("x"), "a.txt", "", WithSummarize(true)); err != nil {
		t.Fatal(err)
	}
	req := s.last(t)
	if req.Path != "/v1/documents/async" || req.Query.Get("summarize") != "true" {
		t.Errorf("insert async: %s %v", req.Path, req.Query)
	}
	status, err := client.WaitForJob(ctx, "j-1", time.Second, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if status.SummaryState == nil || *status.SummaryState != "skipped" {
		t.Errorf("SummaryState = %v", status.SummaryState)
	}
}

func TestBatchInsertSummarize(t *testing.T) {
	ctx := context.Background()
	s, client := newFileServer(t, okReply(map[string]any{"results": []any{pendingRecord(), pendingRecord()}}))
	off := false
	items := []BatchInsertItem{
		{Filename: "a.txt", Content: "a", Summarize: &off},
		{Filename: "b.txt", Content: "b"},
	}
	docs, err := client.BatchInsert(ctx, items, WithSummarize(true))
	if err != nil {
		t.Fatal(err)
	}
	body := s.last(t).jsonBody(t)
	if body["summarize"] != true {
		t.Errorf("batch-level summarize %v", body["summarize"])
	}
	documents := body["documents"].([]any)
	if v, has := documents[0].(map[string]any)["summarize"]; !has || v != false {
		t.Errorf("item 0 summarize %v", documents[0])
	}
	if _, has := documents[1].(map[string]any)["summarize"]; has {
		t.Errorf("item 1 must not carry summarize: %v", documents[1])
	}
	if docs[0].Summary == nil || *docs[0].Summary != "pending" {
		t.Errorf("batch record summary %v", docs[0].Summary)
	}

	if _, err := client.BatchInsert(ctx, []BatchInsertItem{{Filename: "b.txt", Content: "b"}}); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(s.last(t).Body, []byte("summarize")) {
		t.Errorf("summarize must be omitted when unset: %s", s.last(t).Body)
	}
}

func TestUpdateRejectsSummarize(t *testing.T) {
	s, client := newFileServer(t)
	_, err := client.Update(context.Background(), "doc-1", []byte("x"), "a.txt", "", WithSummarize(true))
	if err == nil || !strings.Contains(err.Error(), "WithSummarize") {
		t.Errorf("Update must reject WithSummarize, got %v", err)
	}
	expectNoRequests(t, s)
}

func TestMemorySummarizationDefault(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		opts []MemoryOption
		want string // "" = omitted
	}{
		{[]MemoryOption{WithSummarization(true)}, "true"},
		{[]MemoryOption{WithSummarization(false)}, "false"},
		{nil, ""},
	} {
		s, client := newFileServer(t, okReply(pendingRecord()))
		mem, err := NewMemoryWithClient(fileOwner, client, tc.opts...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mem.Remember(ctx, "Agreed Net 30 with Acme", nil); err != nil {
			t.Fatal(err)
		}
		q := s.last(t).Query
		if tc.want == "" {
			if q.Has("summarize") {
				t.Errorf("summarize must be omitted by default: %v", q)
			}
		} else if q.Get("summarize") != tc.want {
			t.Errorf("summarize=%q, want %q", q.Get("summarize"), tc.want)
		}
		if q.Get("entity_id") != fileOwner {
			t.Errorf("entity_id %q", q.Get("entity_id"))
		}
	}
}

func TestBatchInsertItemSummarizeRoundTrips(t *testing.T) {
	on := true
	data, err := json.Marshal(BatchInsertItem{Filename: "a.txt", Content: "a", Summarize: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"summarize":true`)) {
		t.Errorf("BatchInsertItem JSON %s", data)
	}
}
