package main

import (
	"testing"
)

func TestIsAllowedOrigin(t *testing.T) {
	tests := []struct {
		origin  string
		allowed bool
	}{
		{"https://xeroxgo.vercel.app", true},
		{"https://campusprint.vercel.app", true},
		{"https://campusprint-preview-123.vercel.app", true},
		{"https://xeroxgo-preview-456.vercel.app", true},
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

