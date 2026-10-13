package main

import "testing"

func TestParseCommand(t *testing.T) {
	tests := []struct {
		text, name, arg string
		ok              bool
	}{
		{"/rerun 17", "rerun", "17", true},
		{"/status@opsora_bot", "status", "", true},
		{"/rollback bionte4/opsora", "rollback", "bionte4/opsora", true},
		{"  /Logs  4 ", "logs", "4", true},
		{"hello", "", "", false},
		{"/", "", "", false},
	}
	for _, tt := range tests {
		name, arg, ok := parseCommand(tt.text)
		if name != tt.name || arg != tt.arg || ok != tt.ok {
			t.Errorf("parseCommand(%q) = %q %q %v, want %q %q %v", tt.text, name, arg, ok, tt.name, tt.arg, tt.ok)
		}
	}
}
