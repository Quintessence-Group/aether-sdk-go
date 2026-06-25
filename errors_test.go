package aether

import (
	"errors"
	"testing"
)

// Phase 8 / ADR-015: APIError must match the sentinel errors when the
// returned `code` field carries one of the well-known values, and must
// stay un-matchable otherwise. errors.Is is the idiomatic Go way to do
// this branch in caller code.

func TestAPIErrorMatchesCreditExhaustedSentinel(t *testing.T) {
	err := &APIError{StatusCode: 402, Message: "top up", ErrorCode: CodeCreditExhausted}
	if !errors.Is(err, ErrCreditExhausted) {
		t.Fatalf("expected errors.Is(err, ErrCreditExhausted) to be true")
	}
	if errors.Is(err, ErrFreeLimitExceeded) {
		t.Fatalf("CreditExhausted must not match FreeLimitExceeded sentinel")
	}
	if errors.Is(err, ErrTenantPaused) {
		t.Fatalf("CreditExhausted must not match TenantPaused sentinel")
	}
}

func TestAPIErrorMatchesFreeLimitExceededSentinel(t *testing.T) {
	err := &APIError{StatusCode: 402, Message: "free quota", ErrorCode: CodeFreeLimitExceeded}
	if !errors.Is(err, ErrFreeLimitExceeded) {
		t.Fatalf("expected errors.Is(err, ErrFreeLimitExceeded) to be true")
	}
	if errors.Is(err, ErrCreditExhausted) {
		t.Fatalf("FreeLimitExceeded must not match CreditExhausted sentinel")
	}
}

func TestAPIErrorMatchesTenantPausedSentinel(t *testing.T) {
	err := &APIError{StatusCode: 403, Message: "paused", ErrorCode: CodeTenantPaused}
	if !errors.Is(err, ErrTenantPaused) {
		t.Fatalf("expected errors.Is(err, ErrTenantPaused) to be true")
	}
}

func TestAPIErrorWithoutCodeMatchesNothing(t *testing.T) {
	err := &APIError{StatusCode: 402, Message: "generic"}
	if errors.Is(err, ErrCreditExhausted) ||
		errors.Is(err, ErrFreeLimitExceeded) ||
		errors.Is(err, ErrTenantPaused) {
		t.Fatalf("APIError with no code must not match any sentinel")
	}
}

func TestAPIErrorIsRetryable(t *testing.T) {
	cases := []struct {
		code int
		want bool
	}{
		{200, false}, // 2xx isn't an APIError in practice; sanity check
		{402, false}, // credit_exhausted — never retry
		{403, false}, // tenant_paused — never retry
		{404, false},
		{429, true},
		{502, true},
		{503, true},
		{504, true},
		{500, false}, // 500 is intentionally not retried — likely a real bug
	}
	for _, c := range cases {
		err := &APIError{StatusCode: c.code}
		if got := err.IsRetryable(); got != c.want {
			t.Errorf("status %d: IsRetryable=%v, want %v", c.code, got, c.want)
		}
	}
}
