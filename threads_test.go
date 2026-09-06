package aether

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendThreadUsesVersionedPathAndCallerIdempotencyKey(t *testing.T) {
	var gotPath, gotKey, gotContentType string
	var got ThreadAppendInput
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotKey = r.Header.Get("Idempotency-Key")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode append body: %v", err)
		}
		threadID := "chat/42"
		turnIndex := uint64(0)
		_ = json.NewEncoder(w).Encode(DocumentRecord{
			DocID: "turn-1", ThreadID: &threadID, TurnIndex: &turnIndex,
		})
	})

	doc, err := client.AppendThread(context.Background(), "chat/42", ThreadAppendInput{
		Text:           "hello",
		Tags:           []string{"support"},
		IdempotencyKey: "thread-turn-42-0",
	})
	if err != nil {
		t.Fatalf("AppendThread: %v", err)
	}
	if gotPath != "/v1/threads/chat%2F42/append" {
		t.Errorf("path = %q, want versioned escaped thread path", gotPath)
	}
	if gotKey != "thread-turn-42-0" {
		t.Errorf("idempotency key = %q", gotKey)
	}
	if gotContentType != "application/json" {
		t.Errorf("content type = %q", gotContentType)
	}
	if got.Text != "hello" || len(got.Tags) != 1 || got.Tags[0] != "support" {
		t.Errorf("unexpected append body: %#v", got)
	}
	if doc.TurnIndex == nil || *doc.TurnIndex != 0 {
		t.Errorf("turn index = %v", doc.TurnIndex)
	}
}

func TestGetThreadIncludesWindowAndPartition(t *testing.T) {
	var gotPath string
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		_ = json.NewEncoder(w).Encode(ConversationThread{
			ThreadID:  "chat-1",
			Documents: []DocumentRecord{{DocID: "turn-2"}},
		})
	})
	client, err := base.Partition("tenant-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}

	thread, err := client.GetThread(
		context.Background(),
		"chat-1",
		WithLastNThreadTurns(3),
		WithRecentThreadTurns(),
	)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if gotPath != "/v1/threads/chat-1?last_n_turns=3&partition=tenant-a&recent_first=true" &&
		gotPath != "/v1/threads/chat-1?last_n_turns=3&recent_first=true&partition=tenant-a" {
		t.Errorf("unexpected thread query path %q", gotPath)
	}
	if len(thread.Documents) != 1 || thread.Documents[0].DocID != "turn-2" {
		t.Errorf("unexpected thread response: %#v", thread)
	}
}

func TestThreadValidationFailsBeforeTransport(t *testing.T) {
	requests := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(ConversationThread{})
	})
	if _, err := client.AppendThread(context.Background(), " ", ThreadAppendInput{Text: "hello"}); err == nil {
		t.Fatal("expected empty thread id error")
	}
	if _, err := client.AppendThread(context.Background(), "chat", ThreadAppendInput{Text: " "}); err == nil {
		t.Fatal("expected empty text error")
	}
	if _, err := client.GetThread(context.Background(), ""); err == nil {
		t.Fatal("expected empty thread id error")
	}
	for _, threadID := range []string{".", ".."} {
		if _, err := client.AppendThread(context.Background(), threadID, ThreadAppendInput{Text: "hello"}); err == nil {
			t.Fatalf("expected reserved dot-segment error for %q", threadID)
		}
	}
	invalidUTF8 := string([]byte{'b', 'a', 'd', 0xff, 'i', 'd'})
	if _, err := client.AppendThread(context.Background(), invalidUTF8, ThreadAppendInput{Text: "hello"}); err == nil {
		t.Fatal("expected invalid UTF-8 thread id error from append")
	}
	if _, err := client.GetThread(context.Background(), invalidUTF8); err == nil {
		t.Fatal("expected invalid UTF-8 thread id error from get")
	}
	if err := validateThreadID("safe\x00id"); err == nil {
		t.Fatal("expected control-character thread id error")
	}
	if err := validateThreadID(strings.Repeat("😀", 256)); err != nil {
		t.Fatalf("256-scalar thread id: %v", err)
	}
	if err := validateThreadID(strings.Repeat("😀", 257)); err == nil {
		t.Fatal("expected oversized Unicode-scalar thread id error")
	}
	if _, err := client.GetThread(context.Background(), "chat", WithLastNThreadTurns(0)); err == nil {
		t.Fatal("expected zero thread window error")
	}
	if _, err := client.GetThread(context.Background(), "chat", WithLastNThreadTurns(1001)); err == nil {
		t.Fatal("expected oversized thread window error")
	}
	if requests != 0 {
		t.Fatalf("thread validation made %d transport requests", requests)
	}
}

func TestSearchForwardsThreadFilter(t *testing.T) {
	var gotThreadID string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotThreadID = r.URL.Query().Get("thread_id")
		_ = json.NewEncoder(w).Encode(searchResponse{Query: "hello", Results: []SearchResult{}})
	})

	if _, err := client.Search(context.Background(), "hello", 5, WithSearchThreadID("chat/42")); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotThreadID != "chat/42" {
		t.Errorf("thread_id = %q", gotThreadID)
	}
}

func TestAppendThreadPreservesACLReadersOmittedVersusEmpty(t *testing.T) {
	var bodies []map[string]any
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode append body: %v", err)
		}
		bodies = append(bodies, body)
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "turn"})
	})

	if _, err := client.AppendThread(
		context.Background(), "chat", ThreadAppendInput{Text: "tenant visible"},
	); err != nil {
		t.Fatal(err)
	}
	empty := []string{}
	if _, err := client.AppendThread(
		context.Background(), "chat", ThreadAppendInput{Text: "quarantined", ACLReaders: &empty},
	); err != nil {
		t.Fatal(err)
	}

	if _, exists := bodies[0]["acl_readers"]; exists {
		t.Fatalf("omitted ACLReaders serialized unexpectedly: %#v", bodies[0])
	}
	readers, exists := bodies[1]["acl_readers"]
	if !exists {
		t.Fatalf("explicit empty ACLReaders was omitted: %#v", bodies[1])
	}
	if values, ok := readers.([]any); !ok || len(values) != 0 {
		t.Fatalf("acl_readers = %#v, want []", readers)
	}
}

func TestMemoryThreadAppendScopesEntityAndMetadata(t *testing.T) {
	var got ThreadAppendInput
	createdAt := "2026-07-10T12:00:00Z"
	entityID := "patient-42"
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode append: %v", err)
		}
		_ = json.NewEncoder(w).Encode(DocumentRecord{
			DocID: "turn-1", EntityID: &entityID, CreatedAt: &createdAt,
			Metadata: Metadata{"role": "patient"},
		})
	})
	memory, err := NewMemoryWithClient(entityID, client)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := NewThread(memory, "care/42")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.Thread("care/42"); err != nil {
		t.Fatalf("Memory.Thread: %v", err)
	}

	item, err := direct.Append(
		context.Background(), "I slept better", Metadata{"role": "patient"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.EntityID != entityID || got.Metadata["role"] != "patient" {
		t.Fatalf("append scope = %#v", got)
	}
	if item.ID != "turn-1" || item.Text != "I slept better" ||
		item.EntityID == nil || *item.EntityID != entityID {
		t.Fatalf("append item = %#v", item)
	}
}

func TestMemoryThreadContextRecentThenSemanticAndDeduplicates(t *testing.T) {
	entityID := "patient-42"
	otherEntityID := "patient-99"
	threadID := "care/42"
	duplicate := "duplicate"
	semanticText := "earlier coping plan"
	var (
		mu          sync.Mutex
		threadQuery map[string]string
		searchQuery map[string]string
	)
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/threads/care/42":
			mu.Lock()
			threadQuery = map[string]string{
				"last_n_turns": r.URL.Query().Get("last_n_turns"),
				"recent_first": r.URL.Query().Get("recent_first"),
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(ConversationThread{
				ThreadID: threadID,
				Documents: []DocumentRecord{
					{DocID: "recent-1", EntityID: &entityID},
					{DocID: "other-owner", EntityID: &otherEntityID},
				},
			})
		case "/v1/search":
			mu.Lock()
			searchQuery = map[string]string{
				"k":         r.URL.Query().Get("k"),
				"entity_id": r.URL.Query().Get("entity_id"),
				"thread_id": r.URL.Query().Get("thread_id"),
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(searchResponse{
				Query: "what helped?",
				Results: []SearchResult{
					{DocID: "recent-1", Score: 95, Content: &duplicate},
					{DocID: "semantic-1", Score: 75, Content: &semanticText, Metadata: Metadata{"kind": "plan"}},
				},
			})
		case "/v1/documents/recent-1/download":
			_, _ = w.Write([]byte("latest check-in"))
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	})
	memory, err := NewMemoryWithClient(entityID, client)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := memory.Thread(threadID)
	if err != nil {
		t.Fatal(err)
	}

	items, err := thread.Context(context.Background(), "what helped?", 2, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != "recent-1" || items[0].Text != "latest check-in" ||
		items[1].ID != "semantic-1" || items[1].Text != semanticText {
		t.Fatalf("context = %#v", items)
	}
	if items[1].Score == nil || *items[1].Score != 0.75 {
		t.Fatalf("semantic score = %v", items[1].Score)
	}
	mu.Lock()
	defer mu.Unlock()
	if threadQuery["last_n_turns"] != "2" || threadQuery["recent_first"] != "true" {
		t.Errorf("thread query = %#v", threadQuery)
	}
	if searchQuery["k"] != "5" || searchQuery["entity_id"] != entityID || searchQuery["thread_id"] != threadID {
		t.Errorf("search query = %#v", searchQuery)
	}
}

func TestMemoryThreadContextRetriesPendingOriginProjection(t *testing.T) {
	entityID := "patient-42"
	threadID := "care/42"
	downloadAttempts := 0
	var mu sync.Mutex
	srv, _ := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/threads/care/42":
			_ = json.NewEncoder(w).Encode(ConversationThread{
				ThreadID:  threadID,
				Documents: []DocumentRecord{{DocID: "pending-1", EntityID: &entityID}},
			})
		case "/v1/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Query: "history", Results: nil})
		case "/v1/documents/pending-1/download":
			mu.Lock()
			downloadAttempts++
			attempt := downloadAttempts
			mu.Unlock()
			if attempt == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error": "Thread turn is committed but its origin projection is still pending",
					"code":  "thread_projection_pending",
				})
				return
			}
			_, _ = w.Write([]byte("projected turn"))
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	})
	client := NewClient(srv.URL, WithMaxRetries(1), WithRetryBackoff(time.Nanosecond))
	memory, err := NewMemoryWithClient(entityID, client)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := memory.Thread(threadID)
	if err != nil {
		t.Fatal(err)
	}

	items, err := thread.Context(context.Background(), "history", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Text != "projected turn" {
		t.Fatalf("context = %#v", items)
	}
	mu.Lock()
	defer mu.Unlock()
	if downloadAttempts != 2 {
		t.Fatalf("download attempts = %d, want 2", downloadAttempts)
	}
}

func TestMemoryThreadContextBoundsConcurrentDownloads(t *testing.T) {
	entityID := "patient-42"
	threadID := "care/42"
	activeDownloads := 0
	peakDownloads := 0
	var mu sync.Mutex
	srv, _ := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/threads/care/42":
			documents := make([]DocumentRecord, 17)
			for index := range documents {
				documents[index] = DocumentRecord{
					DocID: fmt.Sprintf("turn-%d", index), EntityID: &entityID,
				}
			}
			_ = json.NewEncoder(w).Encode(ConversationThread{
				ThreadID: threadID, Documents: documents,
			})
		case r.URL.Path == "/v1/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Query: "history", Results: nil})
		case strings.HasSuffix(r.URL.Path, "/download"):
			mu.Lock()
			activeDownloads++
			if activeDownloads > peakDownloads {
				peakDownloads = activeDownloads
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			activeDownloads--
			mu.Unlock()
			_, _ = w.Write([]byte("turn text"))
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	})
	client := NewClient(srv.URL, WithMaxRetries(0))
	memory, err := NewMemoryWithClient(entityID, client)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := memory.Thread(threadID)
	if err != nil {
		t.Fatal(err)
	}

	items, err := thread.Context(context.Background(), "history", 17, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 17 {
		t.Fatalf("items = %d, want 17", len(items))
	}
	mu.Lock()
	defer mu.Unlock()
	if peakDownloads != 8 {
		t.Fatalf("peak downloads = %d, want 8", peakDownloads)
	}
}

func TestMemoryThreadContextStopsBeforeNextBatchAtByteBudget(t *testing.T) {
	entityID := "patient-42"
	threadID := "care/42"
	largeTurn := strings.Repeat("x", 2*1024*1024+1)
	downloadAttempts := 0
	var mu sync.Mutex
	srv, _ := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/threads/care/42":
			documents := make([]DocumentRecord, 9)
			for index := range documents {
				documents[index] = DocumentRecord{
					DocID: fmt.Sprintf("turn-%d", index), EntityID: &entityID,
				}
			}
			_ = json.NewEncoder(w).Encode(ConversationThread{
				ThreadID: threadID, Documents: documents,
			})
		case r.URL.Path == "/v1/search":
			_ = json.NewEncoder(w).Encode(searchResponse{Query: "history", Results: nil})
		case strings.HasSuffix(r.URL.Path, "/download"):
			mu.Lock()
			downloadAttempts++
			mu.Unlock()
			_, _ = w.Write([]byte(largeTurn))
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	})
	client := NewClient(srv.URL, WithMaxRetries(0))
	memory, err := NewMemoryWithClient(entityID, client)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := memory.Thread(threadID)
	if err != nil {
		t.Fatal(err)
	}

	_, err = thread.Context(context.Background(), "history", 9, false)
	if err == nil || !strings.Contains(err.Error(), "byte safety limit") {
		t.Fatalf("budget error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if downloadAttempts != 8 {
		t.Fatalf("download attempts = %d, want 8", downloadAttempts)
	}
}

func TestMemoryThreadValidationFailsBeforeTransport(t *testing.T) {
	_, memory := memoryServer(t, "patient-42", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request: %s", r.URL)
	})
	if _, err := memory.Thread(" "); err == nil {
		t.Fatal("expected invalid thread id")
	}
	invalidUTF8 := string([]byte{'b', 'a', 'd', 0xff, 'i', 'd'})
	if _, err := memory.Thread(invalidUTF8); err == nil {
		t.Fatal("expected invalid UTF-8 thread id from Memory.Thread")
	}
	if _, err := NewThread(memory, invalidUTF8); err == nil {
		t.Fatal("expected invalid UTF-8 thread id from NewThread")
	}
	thread, err := memory.Thread("care")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := thread.Append(context.Background(), " ", nil); err == nil {
		t.Fatal("expected empty text error")
	}
	if _, err := thread.Context(context.Background(), " ", 10, false); err == nil {
		t.Fatal("expected empty query error")
	}
	if _, err := thread.Context(context.Background(), "query", 1001, false); err == nil {
		t.Fatal("expected invalid lastNTurns error")
	}
}

// ── Thread lifecycle ──────────────────────────────────────────────

func TestThreadRestoreUsesVersionedPostPathAndIdempotencyKey(t *testing.T) {
	var gotMethod, gotPath, gotKey string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotKey = r.Header.Get("Idempotency-Key")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "restored"})
	})

	res, err := client.ThreadRestore(context.Background(), "chat/42")
	if err != nil {
		t.Fatalf("ThreadRestore: %v", err)
	}
	// The engine's {status, thread_id, turns} body is parsed and returned.
	if res.Status != "restored" {
		t.Errorf("status = %q, want restored", res.Status)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/threads/chat%2F42/restore" {
		t.Errorf("path = %q, want versioned escaped restore path", gotPath)
	}
	if gotKey == "" {
		t.Error("restore did not send an Idempotency-Key header")
	}
}

func TestThreadRestoreSendsPartitionGuard(t *testing.T) {
	var gotQuery string
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "restored"})
	})
	scoped, err := base.Partition("tenant-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	if _, err := scoped.ThreadRestore(context.Background(), "chat-1"); err != nil {
		t.Fatalf("ThreadRestore: %v", err)
	}
	if gotQuery != "partition=tenant-a" {
		t.Errorf("query = %q, want partition guard", gotQuery)
	}
}

func TestThreadSetACLPutBodyAndIdempotencyKey(t *testing.T) {
	// null (unlabel), [] (quarantine), and [..] (restrict) must all serialize
	// the always-present acl_readers field, and every call must send a PUT with
	// an Idempotency-Key header.
	type capture struct {
		method, path, key, contentType string
		body                           map[string]json.RawMessage
	}
	var got capture
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.EscapedPath()
		got.key = r.Header.Get("Idempotency-Key")
		got.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Case 1: nil pointer → explicit null (unlabel).
	res, err := client.ThreadSetACL(context.Background(), "chat/42", nil)
	if err != nil {
		t.Fatalf("ThreadSetACL(nil): %v", err)
	}
	if res.Status != "ok" {
		t.Errorf("status = %q, want ok", res.Status)
	}
	if got.method != http.MethodPut {
		t.Errorf("method = %q, want PUT", got.method)
	}
	if got.path != "/v1/threads/chat%2F42/acl" {
		t.Errorf("path = %q, want versioned escaped acl path", got.path)
	}
	if got.contentType != "application/json" {
		t.Errorf("content type = %q", got.contentType)
	}
	if got.key == "" {
		t.Error("set-ACL did not send an Idempotency-Key header")
	}
	raw, ok := got.body["acl_readers"]
	if !ok {
		t.Fatalf("acl_readers key omitted for nil: %#v", got.body)
	}
	if string(raw) != "null" {
		t.Errorf("acl_readers = %s, want null", raw)
	}

	// Case 2: non-nil empty slice → [] (admin-only quarantine).
	empty := []string{}
	if _, err := client.ThreadSetACL(context.Background(), "chat", &empty); err != nil {
		t.Fatalf("ThreadSetACL([]): %v", err)
	}
	if string(got.body["acl_readers"]) != "[]" {
		t.Errorf("acl_readers = %s, want []", got.body["acl_readers"])
	}

	// Case 3: non-empty slice → restrict to listed readers.
	readers := []string{"role:admin", "svc:auditor"}
	if _, err := client.ThreadSetACL(context.Background(), "chat", &readers); err != nil {
		t.Fatalf("ThreadSetACL([..]): %v", err)
	}
	if string(got.body["acl_readers"]) != `["role:admin","svc:auditor"]` {
		t.Errorf("acl_readers = %s, want restricted list", got.body["acl_readers"])
	}
}

func TestThreadMoveSendsBothWireFieldsAndIsNotAutoScoped(t *testing.T) {
	var gotMethod, gotPath, gotKey, gotRawQuery string
	var gotBody map[string]json.RawMessage
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotKey = r.Header.Get("Idempotency-Key")
		gotRawQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "moved"})
	})
	// A partition handle must never auto-scope a move: both partitions come only
	// from the explicit body, and no partition query param is sent.
	scoped, err := base.Partition("client-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}

	from, to := "client-a", "client-b"
	res, err := scoped.ThreadMove(context.Background(), "chat/42", &from, &to)
	if err != nil {
		t.Fatalf("ThreadMove: %v", err)
	}
	if res.Status != "moved" {
		t.Errorf("status = %q, want moved", res.Status)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/threads/chat%2F42/move" {
		t.Errorf("path = %q, want versioned escaped move path", gotPath)
	}
	if gotKey == "" {
		t.Error("move did not send an Idempotency-Key header")
	}
	if gotRawQuery != "" {
		t.Errorf("move sent a query %q, want no partition auto-scoping", gotRawQuery)
	}
	// Both keys must be PRESENT on the wire; nil serializes as an explicit null.
	if string(gotBody["expect_partition"]) != `"client-a"` {
		t.Errorf("expect_partition = %s, want \"client-a\"", gotBody["expect_partition"])
	}
	if string(gotBody["to_partition"]) != `"client-b"` {
		t.Errorf("to_partition = %s, want \"client-b\"", gotBody["to_partition"])
	}
}

func TestThreadMoveNilPartitionsSerializeNull(t *testing.T) {
	var gotBody map[string]json.RawMessage
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "moved"})
	})

	if _, err := client.ThreadMove(context.Background(), "chat", nil, nil); err != nil {
		t.Fatalf("ThreadMove: %v", err)
	}
	for _, key := range []string{"expect_partition", "to_partition"} {
		raw, ok := gotBody[key]
		if !ok {
			t.Fatalf("%s omitted: %#v", key, gotBody)
		}
		if string(raw) != "null" {
			t.Errorf("%s = %s, want null", key, raw)
		}
	}
}

func TestThreadDeleteSoftAndHard(t *testing.T) {
	var gotMethod, gotPath, gotKey, gotQuery string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotKey = r.Header.Get("Idempotency-Key")
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "tombstoned"})
	})

	// Soft delete: DELETE, no hard flag, Idempotency-Key present.
	res, err := client.ThreadDelete(context.Background(), "chat/42", false)
	if err != nil {
		t.Fatalf("ThreadDelete(soft): %v", err)
	}
	if res.Status != "tombstoned" {
		t.Errorf("status = %q, want tombstoned", res.Status)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/v1/threads/chat%2F42" {
		t.Errorf("path = %q, want versioned escaped thread path", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("soft delete query = %q, want none", gotQuery)
	}
	if gotKey == "" {
		t.Error("soft delete did not send an Idempotency-Key header")
	}

	// Hard delete: DELETE with ?hard=true.
	if _, err := client.ThreadDelete(context.Background(), "chat", true); err != nil {
		t.Fatalf("ThreadDelete(hard): %v", err)
	}
	if gotQuery != "hard=true" {
		t.Errorf("hard delete query = %q, want hard=true", gotQuery)
	}
	if gotKey == "" {
		t.Error("hard delete did not send an Idempotency-Key header")
	}
}

func TestThreadDeleteHardSendsPartitionGuard(t *testing.T) {
	var gotQuery string
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "hard_deleted"})
	})
	scoped, err := base.Partition("tenant-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	if _, err := scoped.ThreadDelete(context.Background(), "chat", true); err != nil {
		t.Fatalf("ThreadDelete: %v", err)
	}
	// appendPartitionParam must join the guard with & since hard=true is present.
	if gotQuery != "hard=true&partition=tenant-a" {
		t.Errorf("query = %q, want hard flag plus partition guard", gotQuery)
	}
}

func TestThreadLifecycleValidatesThreadIDBeforeTransport(t *testing.T) {
	requests := 0
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	ctx := context.Background()
	if _, err := client.ThreadRestore(ctx, " "); err == nil {
		t.Error("expected empty thread id error from ThreadRestore")
	}
	if _, err := client.ThreadSetACL(ctx, " ", nil); err == nil {
		t.Error("expected empty thread id error from ThreadSetACL")
	}
	if _, err := client.ThreadMove(ctx, " ", nil, nil); err == nil {
		t.Error("expected empty thread id error from ThreadMove")
	}
	if _, err := client.ThreadDelete(ctx, " ", false); err == nil {
		t.Error("expected empty thread id error from ThreadDelete")
	}
	// ThreadMove also validates non-nil partition ids client-side.
	empty := ""
	if _, err := client.ThreadMove(ctx, "chat", &empty, nil); err == nil {
		t.Error("expected empty expect-partition error from ThreadMove")
	}
	long := strings.Repeat("p", 257)
	if _, err := client.ThreadMove(ctx, "chat", nil, &long); err == nil {
		t.Error("expected oversized to-partition error from ThreadMove")
	}
	if requests != 0 {
		t.Fatalf("thread lifecycle validation made %d transport requests", requests)
	}
}

func TestMemoryThreadRestoreSetACLMoveDelete(t *testing.T) {
	entityID := "patient-42"
	threadID := "care/42"
	type capture struct {
		method, path, key string
		rawQuery          string
		body              map[string]json.RawMessage
	}
	var calls []capture
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		c := capture{method: r.Method, path: r.URL.Path, key: r.Header.Get("Idempotency-Key"), rawQuery: r.URL.RawQuery}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&c.body)
		}
		calls = append(calls, c)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	// Scope the underlying client so Thread.Move asserts the current partition
	// as expect_partition.
	scoped, err := base.Partition("tenant-a")
	if err != nil {
		t.Fatalf("Partition: %v", err)
	}
	memory, err := NewMemoryWithClient(entityID, scoped)
	if err != nil {
		t.Fatal(err)
	}
	thread, err := memory.Thread(threadID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// The facade forwards the parsed ThreadLifecycleResult through.
	restoreRes, err := thread.Restore(ctx)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restoreRes.Status != "ok" {
		t.Errorf("restore status = %q, want ok", restoreRes.Status)
	}
	readers := []string{"role:admin"}
	if _, err := thread.SetACL(ctx, &readers); err != nil {
		t.Fatalf("SetACL: %v", err)
	}
	to := "tenant-b"
	if _, err := thread.Move(ctx, &to); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := thread.Delete(ctx, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(calls))
	}
	restore, acl, move, del := calls[0], calls[1], calls[2], calls[3]

	if restore.method != http.MethodPost || restore.path != "/v1/threads/care/42/restore" {
		t.Errorf("restore = %s %s", restore.method, restore.path)
	}
	if restore.rawQuery != "partition=tenant-a" {
		t.Errorf("restore query = %q, want partition guard", restore.rawQuery)
	}

	if acl.method != http.MethodPut || acl.path != "/v1/threads/care/42/acl" {
		t.Errorf("acl = %s %s", acl.method, acl.path)
	}
	if string(acl.body["acl_readers"]) != `["role:admin"]` {
		t.Errorf("acl body = %s", acl.body["acl_readers"])
	}
	if acl.rawQuery != "partition=tenant-a" {
		t.Errorf("acl query = %q, want partition guard", acl.rawQuery)
	}

	if move.method != http.MethodPost || move.path != "/v1/threads/care/42/move" {
		t.Errorf("move = %s %s", move.method, move.path)
	}
	// The facade supplies the Memory's current partition as expect_partition.
	if string(move.body["expect_partition"]) != `"tenant-a"` {
		t.Errorf("move expect_partition = %s, want \"tenant-a\"", move.body["expect_partition"])
	}
	if string(move.body["to_partition"]) != `"tenant-b"` {
		t.Errorf("move to_partition = %s, want \"tenant-b\"", move.body["to_partition"])
	}
	// A move is never partition-auto-scoped, even from a scoped Memory.
	if move.rawQuery != "" {
		t.Errorf("move query = %q, want no partition auto-scoping", move.rawQuery)
	}

	if del.method != http.MethodDelete || del.path != "/v1/threads/care/42" {
		t.Errorf("delete = %s %s", del.method, del.path)
	}
	if del.rawQuery != "partition=tenant-a" {
		t.Errorf("delete query = %q, want partition guard", del.rawQuery)
	}

	for _, c := range calls {
		if c.key == "" {
			t.Errorf("%s %s sent no Idempotency-Key header", c.method, c.path)
		}
	}
}

func TestMemoryThreadDeleteHardFlagsRoute(t *testing.T) {
	var gotMethod, gotQuery string
	_, mem := memoryServer(t, "patient-42", func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "hard_deleted"})
	})
	thread, err := mem.Thread("care/42")
	if err != nil {
		t.Fatal(err)
	}
	res, err := thread.Delete(context.Background(), true)
	if err != nil {
		t.Fatalf("Delete(hard): %v", err)
	}
	if res.Status != "hard_deleted" {
		t.Errorf("status = %q, want hard_deleted", res.Status)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if gotQuery != "hard=true" {
		t.Errorf("query = %q, want hard=true", gotQuery)
	}
}

// Compile-time assertion that the test server receives a normal HTTP route,
// not an SDK-specific hidden transport type.
var _ http.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
