package acpdriver

import (
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestResolveAdapterOpenCode(t *testing.T) {
	adapter, err := ResolveAdapter("opencode")
	if err != nil {
		t.Fatalf("ResolveAdapter: %v", err)
	}
	if got := strings.Join(adapter.Command, " "); got != "opencode acp" {
		t.Fatalf("command = %q, want %q", got, "opencode acp")
	}
}

func TestResolveAdapterUnknown(t *testing.T) {
	if _, err := ResolveAdapter("nope"); err == nil {
		t.Fatal("expected an error for an unknown adapter")
	}
}

func TestParsePermissionMode(t *testing.T) {
	tests := []struct {
		in      string
		want    PermissionMode
		wantErr bool
	}{
		{"ask", PermissionAsk, false},
		{"ALLOW", PermissionAllow, false},
		{"deny", PermissionDeny, false},
		{"yolo", "", true},
	}
	for _, test := range tests {
		got, err := ParsePermissionMode(test.in)
		if test.wantErr {
			if err == nil {
				t.Fatalf("ParsePermissionMode(%q) expected error", test.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParsePermissionMode(%q): %v", test.in, err)
		}
		if got != test.want {
			t.Fatalf("ParsePermissionMode(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestConfigOptions(t *testing.T) {
	options := []acp.SessionConfigOption{
		{Select: &acp.SessionConfigOptionSelect{
			Id:           "model",
			Name:         "Model",
			CurrentValue: "sonnet",
			Options: acp.SessionConfigSelectOptions{
				Ungrouped: &acp.SessionConfigSelectOptionsUngrouped{
					{Name: "Sonnet", Value: "sonnet"},
					{Name: "Opus", Value: "opus"},
				},
			},
		}},
		{Boolean: &acp.SessionConfigOptionBoolean{
			Id:           "fast",
			Name:         "Fast",
			CurrentValue: true,
		}},
	}

	got := configOptions(options)
	if len(got) != 2 {
		t.Fatalf("len(configOptions) = %d, want 2", len(got))
	}
	if got[0].ID != "model" || got[0].Current != "sonnet" {
		t.Fatalf("model option = %#v", got[0])
	}
	if len(got[0].Values) != 2 || got[0].Values[1] != "opus" {
		t.Fatalf("model values = %#v", got[0].Values)
	}
	if got[1].Current != "true" {
		t.Fatalf("boolean current = %q, want true", got[1].Current)
	}
	if configValue(got, "model") != "sonnet" {
		t.Fatalf("configValue(model) = %q", configValue(got, "model"))
	}
}
