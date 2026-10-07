package hme

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewRandomLabelIsAlphanumericAndUnique(t *testing.T) {
	pattern := regexp.MustCompile(`^[a-z0-9]{10}$`)
	existing := make([]Alias, 0, 128)
	for index := 0; index < 128; index++ {
		label, err := NewRandomLabel(existing)
		if err != nil {
			t.Fatal(err)
		}
		if !pattern.MatchString(label) {
			t.Fatalf("label %q is not ten lowercase ASCII letters/digits", label)
		}
		if !strings.ContainsAny(label, "abcdefghijklmnopqrstuvwxyz") || !strings.ContainsAny(label, "0123456789") {
			t.Fatalf("label %q must contain both a letter and a digit", label)
		}
		for _, item := range existing {
			if strings.EqualFold(item.Label, label) {
				t.Fatalf("duplicate label %q", label)
			}
		}
		existing = append(existing, Alias{Label: label})
	}
}

func TestParseAliasCreatedAtSupportsAppleTimestampShapes(t *testing.T) {
	for name, input := range map[string]string{
		"seconds":      "1704067200",
		"milliseconds": "1704067200000",
		"microseconds": "1704067200000000",
		"nanoseconds":  "1704067200000000000",
		"iso":          "2024-01-01T08:00:00+08:00",
	} {
		t.Run(name, func(t *testing.T) {
			parsed, ok := ParseAliasCreatedAt(input)
			if !ok || parsed.Format(time.RFC3339) != "2024-01-01T00:00:00Z" {
				t.Fatalf("ParseAliasCreatedAt(%q) = %s, %v", input, parsed, ok)
			}
		})
	}
	if _, ok := ParseAliasCreatedAt("not-a-date"); ok {
		t.Fatal("invalid timestamp was accepted")
	}
}

func TestOperationErrorRecognizesServiceLimit(t *testing.T) {
	err := operationError("reserve", `{"success":false,"error":{"errorCode":"HME_LIMIT_REACHED","errorMessage":"Maximum number of addresses reached"}}`)
	if !errors.Is(err, ErrAliasLimitReached) {
		t.Fatalf("error %v does not wrap ErrAliasLimitReached", err)
	}
}

func TestOperationErrorSeparatesTemporaryRateLimit(t *testing.T) {
	err := operationError("reserve", `{"success":false,"error":{"errorCode":"HME_LIMIT_REACHED","errorMessage":"You have reached the limit of addresses you can create right now","retryAfter":1800}}`)
	if !errors.Is(err, ErrAliasRateLimited) {
		t.Fatalf("error %v does not wrap ErrAliasRateLimited", err)
	}
	if errors.Is(err, ErrAliasLimitReached) {
		t.Fatalf("temporary rate limit was misclassified as total capacity: %v", err)
	}
	if got := AliasRetryAfter(err); got != 30*time.Minute {
		t.Fatalf("retry after = %s, want 30m", got)
	}
}

func TestOperationErrorTreatsTooManyRequestsAsRateLimit(t *testing.T) {
	err := operationError("generate", `{"success":false,"error":{"errorMessage":"Too many requests. Try again later."}}`)
	if !errors.Is(err, ErrAliasRateLimited) || errors.Is(err, ErrAliasLimitReached) {
		t.Fatalf("unexpected classification: %v", err)
	}
}

func TestOperationErrorPreservesOrdinaryFailure(t *testing.T) {
	err := operationError("delete", `{"success":false,"error":{"errorMessage":"Address is still active"}}`)
	if errors.Is(err, ErrAliasLimitReached) || !strings.Contains(err.Error(), "Address is still active") {
		t.Fatalf("unexpected error: %v", err)
	}
}
