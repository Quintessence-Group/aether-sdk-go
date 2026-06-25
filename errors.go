package aether

import (
	"errors"
	"fmt"
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
)

// Sentinel errors. Use with errors.Is to branch on the specific failure
// without unwrapping APIError manually:
//
//	if errors.Is(err, aether.ErrCreditExhausted) { ... }
var (
	ErrCreditExhausted   = errors.New("aether: credit exhausted")
	ErrFreeLimitExceeded = errors.New("aether: free plan limit exceeded")
	ErrTenantPaused      = errors.New("aether: tenant paused by operator")
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
