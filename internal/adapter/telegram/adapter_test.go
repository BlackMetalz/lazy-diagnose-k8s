package telegram

import (
	"testing"
)

func TestParseMessage_SlashCommand(t *testing.T) {
	tests := []struct {
		input   string
		cmd     string
		target  string
	}{
		{"/check checkout", "check", "checkout"},
		{"/check checkout -n staging", "check", "checkout"},
		{"/check checkout -c lazy-diag-2", "check", "checkout"},
		{"/deploy checkout", "deploy", "checkout"},
		{"/help", "help", ""},
		{"/start", "start", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			parsed := ParseMessage(tt.input)
			if parsed.Command != tt.cmd {
				t.Errorf("command: got %q, want %q", parsed.Command, tt.cmd)
			}
			if parsed.Target != tt.target {
				t.Errorf("target: got %q, want %q", parsed.Target, tt.target)
			}
		})
	}
}

func TestParseMessage_FreeText(t *testing.T) {
	parsed := ParseMessage("checkout bị crash liên tục")
	if parsed.Target != "checkout" {
		t.Errorf("expected target 'checkout', got %q", parsed.Target)
	}
}

func TestParseMessage_Empty(t *testing.T) {
	parsed := ParseMessage("")
	if parsed.Target != "" {
		t.Errorf("expected empty target, got %q", parsed.Target)
	}
}
