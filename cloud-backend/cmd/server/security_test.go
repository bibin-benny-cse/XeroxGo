package main

import (
	"testing"
)

func TestIsAllowedOrigin(t *testing.T) {
	tests := []struct {
		origin  string
		allowed bool
	}{
		{"https://xeroxgo-frontend.vercel.app", true},
		{"https://untrusted.example", false},
		{"https://campusprint.vercel.app", false},
		{"https://campusprint-preview-123.vercel.app", false},
		{"https://xeroxgo-preview-456.vercel.app", false},
		{"https://xeroxgo-malicious.vercel.app", false},
		{"http://localhost:3000", true},
		{"http://localhost:8080", true},
		{"http://localhost:5173", true},
		{"http://127.0.0.1:3000", true},
		{"http://127.0.0.1:8080", true},
		{"http://127.0.0.1:5173", true},
		{"", false},
		{"https://malicious-site.com", false},
		{"https://attacker.vercel.app", false},
		{"https://evilcampusprint.vercel.app", false},
		{"http://evil.com", false},
	}

	for _, tt := range tests {
		got := isAllowedOrigin(tt.origin)
		if got != tt.allowed {
			t.Errorf("isAllowedOrigin(%q) = %v; want %v", tt.origin, got, tt.allowed)
		}
	}

	// Verify XEROXGO_ALLOWED_ORIGINS environment variable overrides
	t.Setenv("XEROXGO_ALLOWED_ORIGINS", "https://custom-preview.example.com, https://staging.example.org")
	if !isAllowedOrigin("https://custom-preview.example.com") {
		t.Errorf("expected custom env origin to be allowed")
	}
	if !isAllowedOrigin("https://staging.example.org") {
		t.Errorf("expected custom env origin to be allowed")
	}
	if isAllowedOrigin("https://other.example.com") {
		t.Errorf("expected unlisted origin to be rejected")
	}
}

func TestSanitizeCSVField(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", ""},
		{"Normal Text", "Normal Text"},
		{"12345", "12345"},
		{"=1+1", "'=1+1"},
		{"-SUM(A1:A10)", "'-SUM(A1:A10)"},
		{"+cmd|' /C calc'!A0", "'+cmd|' /C calc'!A0"},
		{"@SUM(1,2)", "'@SUM(1,2)"},
		{"\tmalicious", "'\tmalicious"},
		{"\rmalicious", "'\rmalicious"},
	}

	for _, tt := range tests {
		got := sanitizeCSVField(tt.input)
		if got != tt.expected {
			t.Errorf("sanitizeCSVField(%q) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}
