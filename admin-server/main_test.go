package main

import "testing"

func TestIsLoopbackAddress(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8081", true},
		{"localhost:8081", true},
		{"[::1]:8081", true},
		{":8081", false},
		{"0.0.0.0:8081", false},
		{"192.168.1.20:8081", false},
		{"invalid", false},
	}
	for _, test := range tests {
		if got := isLoopbackAddress(test.addr); got != test.want {
			t.Errorf("isLoopbackAddress(%q) = %v, want %v", test.addr, got, test.want)
		}
	}
}

func TestNormalizePublicBaseURL(t *testing.T) {
	tests := []struct {
		raw     string
		want    string
		wantErr bool
	}{
		{"https://mail.example.com/", "https://mail.example.com", false},
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081", false},
		{"http://mail.example.com", "", true},
		{"javascript:alert(1)", "", true},
		{"https://mail.example.com/icloud/", "https://mail.example.com/icloud", false},
		{"https://mail.example.com/icloud?x=1", "", true},
	}
	for _, test := range tests {
		got, err := normalizePublicBaseURL(test.raw)
		if (err != nil) != test.wantErr || got != test.want {
			t.Errorf("normalizePublicBaseURL(%q) = %q, %v; want %q, error=%v", test.raw, got, err, test.want, test.wantErr)
		}
	}
}
