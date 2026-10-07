package hme

import "testing"

func TestParseForwardingSettings(t *testing.T) {
	body := `{"success":true,"result":{"selectedForwardTo":"primary@example.com","forwardToEmails":["backup@example.com",{"emailAddress":"relay@example.com"},"primary@example.com"],"hmeEmails":[{"hme":"alias1@icloud.com","anonymousId":"a1","isActive":true,"forwardToEmail":"backup@example.com","createTimestamp":1704067200000},{"hme":"alias2@icloud.com","anonymousId":"a2","state":"inactive","metaData":{"createdAt":"2024-01-02T03:04:05Z"}}]}}`
	settings := parseForwardingSettings(body)
	if settings.SelectedForwardTo != "primary@example.com" {
		t.Fatalf("selected forwarding address = %q", settings.SelectedForwardTo)
	}
	if len(settings.ForwardToEmails) != 3 || len(settings.Aliases) != 2 {
		t.Fatalf("unexpected settings: %#v", settings)
	}
	if settings.Aliases[0].ForwardToEmail != "backup@example.com" {
		t.Fatalf("alias forwarding address = %q", settings.Aliases[0].ForwardToEmail)
	}
	if settings.Aliases[1].ForwardToEmail != settings.SelectedForwardTo {
		t.Fatalf("missing alias forwarding address should use selected default: %#v", settings.Aliases[1])
	}
	if settings.Aliases[0].CreatedAt != "2024-01-01T00:00:00Z" || settings.Aliases[1].CreatedAt != "2024-01-02T03:04:05Z" {
		t.Fatalf("alias creation times were not normalized: %#v", settings.Aliases)
	}
}
