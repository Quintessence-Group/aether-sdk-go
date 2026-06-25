package aether

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestUserAgentHeader(t *testing.T) {
	var gotUA string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_ = json.NewEncoder(w).Encode(NodeStatus{})
	})
	_, _ = client.Status(context.Background())
	if !strings.HasPrefix(gotUA, "aether-sdk-go/") {
		t.Errorf("expected aether-sdk-go User-Agent, got %q", gotUA)
	}
}

func TestIdempotencyKeyOnPost(t *testing.T) {
	var gotKey string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d1"})
	})
	_, err := client.InsertText(context.Background(), "hello", "t.txt")
	if err != nil {
		t.Fatalf("InsertText: %v", err)
	}
	if !uuidV4Re.MatchString(gotKey) {
		t.Errorf("expected v4 UUID idempotency key, got %q", gotKey)
	}
}

func TestIdempotencyKeyStableAcrossRetries(t *testing.T) {
	var keys []string
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 -> retried
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d1"})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(time.Millisecond)) // tiny backoff to keep the test fast

	if _, err := client.InsertText(context.Background(), "hello", "t.txt"); err != nil {
		t.Fatalf("InsertText: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(keys))
	}
	if keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("expected the same idempotency key across retries, got %q and %q", keys[0], keys[1])
	}
}

func TestTinyRetryBackoffDoesNotPanic(t *testing.T) {
	// A sub-2ns backoff makes int64(delay)/2 == 0; the jitter computation must
	// not panic on rand.Int63n(0). Regression test for the backoff guard.
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // force one retry through sleepBackoff
			return
		}
		_ = json.NewEncoder(w).Encode(DocumentRecord{DocID: "d1"})
	}))
	t.Cleanup(srv.Close)
	client := NewClient(srv.URL, WithRetryBackoff(1)) // 1ns backoff

	if _, err := client.InsertText(context.Background(), "hello", "t.txt"); err != nil {
		t.Fatalf("InsertText with 1ns backoff: %v", err)
	}
	if n < 2 {
		t.Fatalf("expected a retry to exercise sleepBackoff, got %d attempts", n)
	}
}

func TestNoIdempotencyKeyOnGet(t *testing.T) {
	var gotKey string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		_ = json.NewEncoder(w).Encode(NodeStatus{})
	})
	_, _ = client.Status(context.Background())
	if gotKey != "" {
		t.Errorf("expected no idempotency key on GET, got %q", gotKey)
	}
}

func TestValidateBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		apiKey  string
		wantErr bool
	}{
		{"http remote with key", "http://api.aetherdb.ai", "secret", true},
		{"http localhost with key", "http://localhost:9000", "secret", false},
		{"http loopback ip with key", "http://127.0.0.1:9000", "secret", false},
		{"https remote with key", "https://api.aetherdb.ai", "secret", false},
		{"http remote no key", "http://api.aetherdb.ai", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateBaseURL(tc.baseURL, tc.apiKey); (err != nil) != tc.wantErr {
				t.Errorf("validateBaseURL(%q, key=%v) err = %v, wantErr = %v",
					tc.baseURL, tc.apiKey != "", err, tc.wantErr)
			}
		})
	}
}

func TestInsecureConfigFailsRequest(t *testing.T) {
	client := New(WithBaseURL("http://api.aetherdb.ai"), WithAPIKey("secret"))
	_, err := client.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "insecure HTTP") {
		t.Errorf("expected insecure-HTTP error from request, got %v", err)
	}
}
