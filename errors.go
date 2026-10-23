package aether

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Machine-readable error codes returned by the Aether API in the `code`
// field of a 4xx/5xx response. The Go SDK exposes these as untyped
// constants so callers can branch on them via errors.Is + ErrorCode.
const (
	// CodeCreditExhausted is returned with HTTP 402 when a paid tenant's
	// prepaid credit balance has hit zero. ADR-015. Not retryable; the
	// operation is permanently denied until credit is added via the
	// Portal billing page.
	CodeCreditExhausted = "credit_exhausted"

	// CodeFreeLimitExceeded is returned with HTTP 402 when a Free-tier
	// tenant exceeds a hard plan limit. Distinct from CodeCreditExhausted
	// so dashboards can separate abuse signal from billing failures.
	// Resolution is a plan upgrade, not a top-up.
	CodeFreeLimitExceeded = "free_limit_exceeded"

	// CodeTenantPaused is returned with HTTP 403 when an operator has
	// paused a tenant via the spike detector or admin console. Not
	// retryable; the tenant must be un-paused out-of-band.
	CodeTenantPaused = "tenant_paused"

	// CodePartitionRequired is returned with HTTP 400 when a multi-tenant key
	// makes a call without naming a partition. The key requires every
	// read/write to be scoped; scope the call through Partition("<id>")
	// instead of the top-level client. Keys minted with strict scoping
	// additionally get it on any doc_id-addressed call made without the
	// partition guard. Not retryable — it is a programming error, not a
	// transient failure.
	CodePartitionRequired = "partition_required"

	// CodeSessionInvalid is returned with HTTP 400 when a connect-session
	// token is unknown, already consumed, or expired. The three cases are
	// deliberately indistinguishable — mint a new session with
	// CreateConnectSession rather than retrying the same token.
	CodeSessionInvalid = "session_invalid"

	// CodePartitionMismatch is returned with HTTP 400 when
	// CreateConnectSession's asserted partition (from a partition handle)
	// disagrees with the partition the session would actually resolve to.
	// Mint on a handle scoped to the same externalUserID, or omit the
	// handle.
	CodePartitionMismatch = "partition_mismatch"
)

// Error codes returned by the memory-file, document-text and summary routes.
// Every one surfaces as a plain *APIError carrying the code, except
// CodeMemoryFileVersionConflict, which surfaces as *VersionConflictError.
// None is retryable except CodeMemoryFileBusy, a 503 the client already
// retries automatically.
const (
	// CodeMemoryFileNameInvalid (400): the memory-file name is not 1-128
	// letters, digits, '.', '_' or '-' starting with a letter or digit,
	// contains "..", or is the reserved "grep". See ValidateMemoryFileName.
	CodeMemoryFileNameInvalid = "memory_file_name_invalid"
	// CodeInvalidPattern (400): a memory-file grep regex is invalid or too
	// large.
	CodeInvalidPattern = "invalid_pattern"
	// CodeReservedMetadataKey (400): a write's metadata used a key the
	// engine reserves for memory files or summaries.
	CodeReservedMetadataKey = "reserved_metadata_key"
	// CodeFeatureNotEnabled (404): memory files or summaries are not
	// enabled on this deployment.
	CodeFeatureNotEnabled = "feature_not_enabled"
	// CodeMemoryFileNotFound (404): no live memory file has this name.
	CodeMemoryFileNotFound = "memory_file_not_found"
	// CodeMemoryFileVersionNotFound (404): the requested version is not
	// retained, or is newer than the current version.
	CodeMemoryFileVersionNotFound = "memory_file_version_not_found"
	// CodeMemoryFileSectionNotFound (404): the section selector matched no
	// heading.
	CodeMemoryFileSectionNotFound = "memory_file_section_not_found"
	// CodeDocumentSectionNotFound (404): GetText's section selector matched
	// no heading.
	CodeDocumentSectionNotFound = "document_section_not_found"
	// CodeMemoryFileSectionAmbiguous (409): the section selector matched
	// several headings; select by slug instead.
	CodeMemoryFileSectionAmbiguous = "memory_file_section_ambiguous"
	// CodeDocumentSectionAmbiguous (409): GetText's section selector matched
	// several headings; select by slug instead.
	CodeDocumentSectionAmbiguous = "document_section_ambiguous"
	// CodeIdempotencyKeyReused (409): a memory-file write reused an
	// Idempotency-Key with a different request.
	CodeIdempotencyKeyReused = "idempotency_key_reused"
	// CodeMemoryFileUseMemoryRoutes (409): a generic document update, move or
	// restore targeted a memory-file version; use the MemoryFiles methods.
	CodeMemoryFileUseMemoryRoutes = "memory_file_use_memory_routes"
	// CodeSummaryUseSummaryRoutes (409): a generic document update, move or
	// restore targeted a stored summary; use the summary methods.
	CodeSummaryUseSummaryRoutes = "summary_use_summary_routes"
	// CodeSummaryUserAuthored (409): regenerating would replace a
	// user-written summary; call RegenerateSummary with force=true.
	CodeSummaryUserAuthored = "summary_user_authored"
	// CodeMemoryFileVersionConflict (412): a memory-file write's expected
	// version did not match the current one (or a create-only write found an
	// existing file). Surfaces as *VersionConflictError.
	CodeMemoryFileVersionConflict = "memory_file_version_conflict"
	// CodeSummaryRevisionConflict (412): PutSummary's expected revision did
	// not match the stored summary.
	CodeSummaryRevisionConflict = "summary_revision_conflict"
	// CodeMemoryFileTooLarge (413): the resulting memory file would exceed
	// 512 KiB.
	CodeMemoryFileTooLarge = "memory_file_too_large"
	// CodeSummaryTooLarge (413): the summary exceeds 16 KiB.
	CodeSummaryTooLarge = "summary_too_large"
	// CodeDocumentTextUnavailable (422): the document has no readable text.
	CodeDocumentTextUnavailable = "document_text_unavailable"
	// CodeSummaryUnsupported (422): the document can never be summarized
	// (a fact, a summary, a memory file, a conversation turn, or no text).
	CodeSummaryUnsupported = "summary_unsupported"
	// CodeSummarizerNotConfigured (422): no summarizer is configured on this
	// deployment.
	CodeSummarizerNotConfigured = "summarizer_not_configured"
	// CodePreconditionRequired (428): a whole-file write or section replace
	// was sent without a version precondition.
	CodePreconditionRequired = "precondition_required"
	// CodeMemoryFileBusy (503): another write held the file; nothing was
	// written. Retried automatically.
	CodeMemoryFileBusy = "memory_file_busy"
)

// Sentinel errors. Use with errors.Is to branch on the specific failure
// without unwrapping APIError manually:
//
//	if errors.Is(err, aether.ErrCreditExhausted) { ... }
var (
	ErrCreditExhausted   = errors.New("aether: credit exhausted")
	ErrFreeLimitExceeded = errors.New("aether: free plan limit exceeded")
	ErrTenantPaused      = errors.New("aether: tenant paused by operator")
	// ErrPartitionRequired matches a 400 from a multi-tenant key that made an
	// unscoped call. Scope the call through Partition("<id>") and retry.
	ErrPartitionRequired = errors.New("aether: partition required — scope the call through Partition(id)")
	// ErrSessionInvalid matches a 400 from a connect-session token that is
	// unknown, already consumed, or expired.
	ErrSessionInvalid = errors.New("aether: connect session invalid, consumed, or expired")
	// ErrPartitionMismatch matches a 400 from CreateConnectSession when the
	// handle's asserted partition disagrees with the resolved placement.
	ErrPartitionMismatch = errors.New("aether: asserted partition does not match the session's resolved partition")
	// ErrVersionConflict matches a 412 from a memory-file write whose expected
	// version is no longer current. Use errors.As with *VersionConflictError to
	// read the current version, then re-read and retry.
	ErrVersionConflict = errors.New("aether: memory file version conflict — re-read the file and retry")
)

// APIError is returned when the Aether API responds with a non-2xx status code.
type APIError struct {
	StatusCode int
	Message    string
	ErrorCode  string
}

func (e *APIError) Error() string {
	if e.ErrorCode != "" {
		return fmt.Sprintf("aether api error (%d) [%s]: %s", e.StatusCode, e.ErrorCode, e.Message)
	}
	return fmt.Sprintf("aether api error (%d): %s", e.StatusCode, e.Message)
}

// Is implements errors.Is to enable matching against the sentinel errors
// declared above. An APIError with ErrorCode == CodeCreditExhausted matches
// ErrCreditExhausted; the same pattern holds for the other two codes. This
// keeps the public surface small (one struct + three sentinels) while still
// letting callers branch with idiomatic errors.Is.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrCreditExhausted:
		return e.ErrorCode == CodeCreditExhausted
	case ErrFreeLimitExceeded:
		return e.ErrorCode == CodeFreeLimitExceeded
	case ErrTenantPaused:
		return e.ErrorCode == CodeTenantPaused
	case ErrPartitionRequired:
		return e.ErrorCode == CodePartitionRequired
	case ErrSessionInvalid:
		return e.ErrorCode == CodeSessionInvalid
	case ErrPartitionMismatch:
		return e.ErrorCode == CodePartitionMismatch
	case ErrVersionConflict:
		return e.ErrorCode == CodeMemoryFileVersionConflict
	}
	return false
}

// IsRetryable reports whether this error represents a transient failure
// that the SDK's automatic retry loop should re-attempt. 402/403 are
// terminal — the caller's branch logic must handle them, not the retry
// loop.
func (e *APIError) IsRetryable() bool {
	return e.StatusCode == 429 || e.StatusCode == 502 || e.StatusCode == 503 || e.StatusCode == 504
}

// NetworkError is returned when a network-level failure prevents the request
// from completing (connection refused, DNS failure, timeout, etc.).
type NetworkError struct {
	Err error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("aether network error: %s", e.Err)
}

func (e *NetworkError) Unwrap() error {
	return e.Err
}

// VersionConflictError is returned when a memory-file write (Write, Append,
// AppendSection, ReplaceSection or Delete) carried an expected version that is
// no longer current, or a create-only write found an existing file (HTTP 412,
// code CodeMemoryFileVersionConflict). CurrentVersion is the file's current
// version, or nil when no file exists.
//
// It wraps the *APIError, so errors.As(err, &apiErr) still yields the status
// and code, and errors.Is(err, ErrVersionConflict) is true:
//
//	var conflict *aether.VersionConflictError
//	if errors.As(err, &conflict) {
//		// re-read the file, re-apply the edit, write again
//	}
type VersionConflictError struct {
	*APIError
	// CurrentVersion is the current version of the file, or nil when the file
	// does not exist.
	CurrentVersion *int
}

// Unwrap returns the underlying *APIError.
func (e *VersionConflictError) Unwrap() error { return e.APIError }

// apiErrorFromResponse builds the error for a non-2xx response. Every transport
// path goes through it, so a 412 memory-file version conflict is typed as
// *VersionConflictError wherever it arrives; any other status/code is a plain
// *APIError.
func apiErrorFromResponse(resp *http.Response, body []byte) error {
	var errResp errorResponse
	msg := resp.Status
	if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
		msg = errResp.Error
	}
	apiErr := &APIError{StatusCode: resp.StatusCode, Message: msg, ErrorCode: errResp.Code}
	if resp.StatusCode == http.StatusPreconditionFailed && errResp.Code == CodeMemoryFileVersionConflict {
		return &VersionConflictError{APIError: apiErr, CurrentVersion: parseCurrentVersion(body)}
	}
	return apiErr
}

// parseCurrentVersion reads current_version from a version-conflict body. A
// missing, null or non-integer value yields nil.
func parseCurrentVersion(body []byte) *int {
	var payload struct {
		CurrentVersion json.RawMessage `json:"current_version"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.CurrentVersion) == 0 {
		return nil
	}
	var version *int
	if json.Unmarshal(payload.CurrentVersion, &version) != nil {
		return nil
	}
	return version
}
