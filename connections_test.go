package aether

// The Connections API surface: mint / list /
// get / delete / resync / browse / update selection / get purge receipt,
// the typed errors, and the offline redirect-signature verifier.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// ── CreateConnectSession ──────────────────────────────────────────

func TestCreateConnectSessionMintsAndParsesResponse(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_token": "acs_deadbeef",
			"connect_url":   "https://connect.example.com/connect/acs_deadbeef",
			"client_secret": "acsec_secretsecret",
			"expires_at":    "2026-08-15T00:00:00Z",
		})
	})

	session, err := client.CreateConnectSession(context.Background(), CreateConnectSessionOptions{
		ExternalUserID: "priya",
		ReturnURL:      "https://acme.example.com/cb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/connections/sessions" {
		t.Errorf("expected /v1/connections/sessions, got %s", gotPath)
	}
	if gotBody["provider"] != "dropbox" {
		t.Errorf("expected default provider dropbox, got %v", gotBody["provider"])
	}
	if gotBody["external_user_id"] != "priya" {
		t.Errorf("expected external_user_id priya, got %v", gotBody["external_user_id"])
	}
	if session.SessionToken != "acs_deadbeef" {
		t.Errorf("unexpected session token: %s", session.SessionToken)
	}
	if session.ClientSecret != "acsec_secretsecret" {
		t.Errorf("unexpected client secret: %s", session.ClientSecret)
	}
}

func TestCreateConnectSessionOnAHandleAssertsPartition(t *testing.T) {
	var gotPartition string
	_, base := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPartition = r.URL.Query().Get("partition")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_token": "acs_x",
			"connect_url":   "https://connect.example.com/connect/acs_x",
			"client_secret": "acsec_x",
			"expires_at":    "2026-08-15T00:00:00Z",
		})
	})
	client, err := base.Partition("priya")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateConnectSession(context.Background(), CreateConnectSessionOptions{
		ExternalUserID: "priya",
		ReturnURL:      "https://acme.example.com/cb",
	}); err != nil {
		t.Fatal(err)
	}
	if gotPartition != "priya" {
		t.Errorf("expected partition=priya, got %q", gotPartition)
	}
}

func TestCreateConnectSessionRejectsEmptyArgs(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{}))
	if _, err := client.CreateConnectSession(context.Background(), CreateConnectSessionOptions{ReturnURL: "https://x.example.com"}); err == nil {
		t.Error("expected error for empty ExternalUserID")
	}
	if _, err := client.CreateConnectSession(context.Background(), CreateConnectSessionOptions{ExternalUserID: "priya"}); err == nil {
		t.Error("expected error for empty ReturnURL")
	}
}

func TestCreateConnectSessionPartitionMismatchIsTyped(t *testing.T) {
	_, client := jsonServer(t, errorCodeHandler(400, "mismatch", CodePartitionMismatch))
	base, err := client.Partition("someone-else")
	if err != nil {
		t.Fatal(err)
	}
	_, err = base.CreateConnectSession(context.Background(), CreateConnectSessionOptions{
		ExternalUserID: "priya",
		ReturnURL:      "https://acme.example.com/cb",
	})
	if !errors.Is(err, ErrPartitionMismatch) {
		t.Errorf("expected errors.Is(err, ErrPartitionMismatch), got %v", err)
	}
}

func TestCreateConnectSessionInvalidIsTyped(t *testing.T) {
	_, client := jsonServer(t, errorCodeHandler(400, "already used", CodeSessionInvalid))
	_, err := client.CreateConnectSession(context.Background(), CreateConnectSessionOptions{
		ExternalUserID: "priya",
		ReturnURL:      "https://acme.example.com/cb",
	})
	if !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("expected errors.Is(err, ErrSessionInvalid), got %v", err)
	}
}

// ── ListConnections ───────────────────────────────────────────────

func TestListConnectionsParsesEveryField(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/connections" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connections": []map[string]any{
				{
					"connection_id":        "11111111-1111-1111-1111-111111111111",
					"provider":             "dropbox",
					"owner_type":           "external_user",
					"owner_id":             "priya",
					"provider_account_id":  "dbid:priya",
					"account_display_name": "Priya",
					"target_partition":     "priya",
					"status":               "active",
					"granted_scopes":       []string{"files.metadata.read"},
					"created_at":           "2026-08-15T00:00:00Z",
					"files_synced":         3,
					"files_skipped":        0,
					"files_deleted":        0,
					"selected_paths":       []string{},
					"purge_state":          "not_started",
					"credential_deleted":   false,
				},
			},
		})
	})

	conns, err := client.ListConnections(context.Background(), ListConnectionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conns))
	}
	c := conns[0]
	if c.OwnerType != "external_user" || c.OwnerID == nil || *c.OwnerID != "priya" {
		t.Errorf("unexpected owner: %+v", c)
	}
	if c.TargetPartition == nil || *c.TargetPartition != "priya" {
		t.Errorf("unexpected target partition: %+v", c.TargetPartition)
	}
	if c.FilesSynced != 3 {
		t.Errorf("expected files_synced=3, got %d", c.FilesSynced)
	}
}

func TestListConnectionsSendsOwnerFiltersAndIncludePurged(t *testing.T) {
	var q url.Values
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"connections": []any{}})
	})
	_, err := client.ListConnections(context.Background(), ListConnectionsOptions{
		OwnerType:        "external_user",
		OwnerID:          "priya",
		IncludePurged:    false,
		IncludePurgedSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("owner_type") != "external_user" || q.Get("owner_id") != "priya" {
		t.Errorf("unexpected owner filters: %v", q)
	}
	if q.Get("include_purged") != "false" {
		t.Errorf("expected include_purged=false, got %v", q.Get("include_purged"))
	}
}

// ── GetConnection ─────────────────────────────────────────────────

func connectionFixtureJSON(id string) map[string]any {
	return map[string]any{
		"connection_id":        id,
		"provider":             "dropbox",
		"owner_type":           "tenant",
		"owner_id":             nil,
		"provider_account_id":  "dbid:acme",
		"account_display_name": nil,
		"target_partition":     nil,
		"status":               "active",
		"granted_scopes":       []string{},
		"created_at":           "2026-08-15T00:00:00Z",
		"files_synced":         0,
		"files_skipped":        0,
		"files_deleted":        0,
		"selected_paths":       []string{},
		"purge_state":          "not_started",
		"credential_deleted":   false,
	}
}

func TestGetConnectionFetchesByID(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	var gotPath string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(connectionFixtureJSON(id))
	})
	conn, err := client.GetConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/connections/"+id {
		t.Errorf("unexpected path: %s", gotPath)
	}
	if conn.ConnectionID != id {
		t.Errorf("unexpected connection id: %s", conn.ConnectionID)
	}
	if conn.TargetPartition != nil {
		t.Errorf("expected nil target partition for mode A, got %v", conn.TargetPartition)
	}
}

func TestGetConnectionWrongPartitionIsThePlainNotFound(t *testing.T) {
	_, client := jsonServer(t, errorCodeHandler(404, "unknown connection", "connection_not_found"))
	base, err := client.Partition("someone-else")
	if err != nil {
		t.Fatal(err)
	}
	_, err = base.GetConnection(context.Background(), "11111111-1111-1111-1111-111111111111")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 404 || apiErr.ErrorCode != "connection_not_found" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGetConnectionRejectsEmptyID(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{}))
	if _, err := client.GetConnection(context.Background(), ""); err == nil {
		t.Error("expected error for empty connectionID")
	}
}

// ── DeleteConnection ──────────────────────────────────────────────

func TestDeleteConnectionParsesPurgeSummary(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	var gotMethod string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connection_id": id,
			"status":        "revoked",
			"purge": map[string]any{
				"receipt_id":       "r1",
				"documents_purged": 5,
				"merkle_root":      "deadbeef",
				"completed_at":     "2026-08-15T00:00:00Z",
				"signer_node_id":   "node-1",
			},
		})
	})
	result, err := client.DeleteConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if result.Status != "revoked" {
		t.Errorf("unexpected status: %s", result.Status)
	}
	if result.Purge == nil || result.Purge.DocumentsPurged != 5 {
		t.Errorf("unexpected purge summary: %+v", result.Purge)
	}
}

func TestDeleteConnectionIdempotentNoOpHasNoPurge(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"connection_id": id, "status": "revoked", "purge": nil})
	})
	result, err := client.DeleteConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purge != nil {
		t.Errorf("expected nil purge, got %+v", result.Purge)
	}
}

func TestDeleteConnectionRejectsEmptyID(t *testing.T) {
	_, client := jsonServer(t, jsonHandler(map[string]any{}))
	if _, err := client.DeleteConnection(context.Background(), ""); err == nil {
		t.Error("expected error for empty connectionID")
	}
}

// ── ResyncConnection ──────────────────────────────────────────────

func TestResyncConnectionPostsThenRefetches(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	var calls []string
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/connections/"+id+"/resync" {
			_ = json.NewEncoder(w).Encode(map[string]any{"connection_id": id, "status": "active"})
			return
		}
		_ = json.NewEncoder(w).Encode(connectionFixtureJSON(id))
	})
	conn, err := client.ResyncConnection(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if conn.Status != "active" {
		t.Errorf("unexpected status: %s", conn.Status)
	}
	foundResync, foundGet := false, false
	for _, c := range calls {
		if c == "POST /v1/connections/"+id+"/resync" {
			foundResync = true
		}
		if c == "GET /v1/connections/"+id {
			foundGet = true
		}
	}
	if !foundResync || !foundGet {
		t.Errorf("expected both resync POST and GET follow-up, got %v", calls)
	}
}

// ── BrowseConnection / UpdateSelection ────────────────────────────

func TestBrowseConnectionSendsPathAndCursorAndParsesPage(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	var gotBody map[string]any
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/connections/"+id+"/browse" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"entries": []map[string]any{
				{"name": "q3.txt", "path_display": "/Reports/q3.txt", "is_folder": false, "size_bytes": 42},
			},
			"next_cursor": "cursor-2",
		})
	})
	page, err := client.BrowseConnection(context.Background(), id, "/Reports", nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["path"] != "/Reports" {
		t.Errorf("unexpected path in body: %v", gotBody["path"])
	}
	if len(page.Entries) != 1 || page.Entries[0].Name != "q3.txt" {
		t.Errorf("unexpected entries: %+v", page.Entries)
	}
	if page.NextCursor == nil || *page.NextCursor != "cursor-2" {
		t.Errorf("unexpected next cursor: %v", page.NextCursor)
	}
}

func TestUpdateSelectionReplacesPathsAndReturnsNormalized(t *testing.T) {
	id := "11111111-1111-1111-1111-111111111111"
	var gotMethod string
	var gotBody map[string]any
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"connection_id":  id,
			"selected_paths": []string{"/Reports"},
		})
	})
	paths, err := client.UpdateSelection(context.Background(), id, []string{"/Reports"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("expected PUT, got %s", gotMethod)
	}
	if len(paths) != 1 || paths[0] != "/Reports" {
		t.Errorf("unexpected selected paths: %v", paths)
	}
}

// ── GetPurgeReceipt ───────────────────────────────────────────────

func TestGetPurgeReceiptParsesFullShape(t *testing.T) {
	_, client := jsonServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/connections/purge-receipts/r1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":                   "1",
			"receipt_id":                "r1",
			"tenant_id":                 "t1",
			"connection_id":             "c1",
			"provider":                  "dropbox",
			"owner":                     "external_user:priya",
			"provider_account_id":       "dbid:priya",
			"documents_purged":          5,
			"documents_failed":          0,
			"merkle_root":               "deadbeef",
			"merkle_leaf_count":         5,
			"purged_document_ids":       []string{"d1", "d2"},
			"partitions_touched":        []string{"priya"},
			"default_partition_touched": false,
			"credential_revocation":     "revoked",
			"credential_deleted":        true,
			"started_at":                "2026-08-15T00:00:00Z",
			"completed_at":              "2026-08-15T00:00:01Z",
			"signer_node_id":            "node-1",
			"signer_public_key":         "pub-1",
			"signature":                 "sig-1",
			"verified":                  true,
		})
	})
	receipt, err := client.GetPurgeReceipt(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.DocumentsPurged != 5 {
		t.Errorf("unexpected documents purged: %d", receipt.DocumentsPurged)
	}
	if !receipt.Verified {
		t.Error("expected verified=true")
	}
}

// ── VerifyConnectRedirectSignature (pure, offline) ────────────────

func referenceSig(clientSecret, session, status, connectionID string) string {
	key := sha256.Sum256([]byte(clientSecret))
	message := fmt.Sprintf("%s|%s|%s", session, status, connectionID)
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyConnectRedirectSignatureAcceptsACorrectSignature(t *testing.T) {
	secret := "acsec_the-real-secret"
	sig := referenceSig(secret, "acs_tok", "active", "conn-1")
	if !VerifyConnectRedirectSignature(secret, "acs_tok", "active", "conn-1", sig) {
		t.Error("expected signature to verify")
	}
}

func TestVerifyConnectRedirectSignatureRejectsATamperedParam(t *testing.T) {
	secret := "acsec_the-real-secret"
	sig := referenceSig(secret, "acs_tok", "active", "conn-1")
	if VerifyConnectRedirectSignature(secret, "acs_tok", "error", "conn-1", sig) {
		t.Error("expected signature verification to fail on tampered status")
	}
}

func TestVerifyConnectRedirectSignatureRejectsTheWrongSecret(t *testing.T) {
	sig := referenceSig("acsec_the-real-secret", "acs_tok", "active", "conn-1")
	if VerifyConnectRedirectSignature("acsec_a-different-secret", "acs_tok", "active", "conn-1", sig) {
		t.Error("expected signature verification to fail with the wrong secret")
	}
}

// errorCodeHandler mirrors errorHandler but also sets the machine-readable
// `code` field, which is what the typed-error factory branches on.
func errorCodeHandler(status int, msg, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
	}
}
