package hme

import (
	"errors"
	"strings"
	"testing"
)

func TestAppleSessionHeaderClassification(t *testing.T) {
	for _, name := range []string{
		"X-APPLE-WEBAUTH-TOKEN",
		"X-APPLE-WEBAUTH-USER",
		"X-APPLE-WEBAUTH-HSA-TRUST",
		"X-APPLE-DS-WEB-SESSION-TOKEN",
		"X-APPLE-UNIQUE-CLIENT-ID",
		"x-apple-group",
	} {
		if !isAppleSessionHeader(name) {
			t.Fatalf("expected %s to be mirrored as a header", name)
		}
	}
	if isAppleSessionHeader("session") {
		t.Fatal("ordinary cookie must not be mirrored as a request header")
	}
}

func TestApplyTrustChallengeUpdatesWithoutExposingToken(t *testing.T) {
	token := strings.Repeat("Ab1+", 40)
	client := &Client{Cookies: map[string]string{"X-APPLE-WEBAUTH-HSA-TRUST": "old-token-value-that-is-long-enough-123"}}
	if !client.applyTrustChallenge(`{"success":false,"trustTokens":["` + token + `"]}`) {
		t.Fatal("expected challenge to update trust token")
	}
	if client.Cookies["X-APPLE-WEBAUTH-HSA-TRUST"] != token {
		t.Fatal("challenge token was not stored")
	}
	err := safeAppleHTTPError(421, `{"trustTokens":["`+token+`"]}`)
	if strings.Contains(err.Error(), token) {
		t.Fatal("HTTP error exposed trust token")
	}
}

func TestTrustChallengeSentinel(t *testing.T) {
	err := errors.Join(ErrTrustSessionRequired, errors.New("refresh required"))
	if !errors.Is(err, ErrTrustSessionRequired) {
		t.Fatal("trust session sentinel must remain detectable")
	}
}
