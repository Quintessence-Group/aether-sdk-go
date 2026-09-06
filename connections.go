package aether

// Pure, offline helper for the Connections
// API's connect-session redirect signature. No network calls; safe to run
// in whatever request handler the application's backend uses for the OAuth
// callback.

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
)

// VerifyConnectRedirectSignature verifies a CreateConnectSession redirect's
// signature (docs/SDK_API_CONTRACT.md §4.18).
//
// clientSecret is the value returned exactly once by CreateConnectSession.
// The comparison is constant-time (crypto/subtle) so this function itself
// never becomes a timing oracle on the signature.
//
// Returns true iff sig matches the recomputed signature:
//
//	sig = hex(HMAC-SHA256(
//	        key = SHA-256(clientSecret),
//	        message = "<session>|<status>|<connectionID>"))
func VerifyConnectRedirectSignature(clientSecret, session, status, connectionID, sig string) bool {
	key := sha256.Sum256([]byte(clientSecret))
	message := fmt.Sprintf("%s|%s|%s", session, status, connectionID)
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(message))
	expected := mac.Sum(nil)

	actual, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(expected, actual) == 1
}
