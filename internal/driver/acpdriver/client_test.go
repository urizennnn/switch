package acpdriver

import (
	"context"
	"io"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func permissionRequest() acp.RequestPermissionRequest {
	title := "Write file"
	return acp.RequestPermissionRequest{
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: "tool-1",
			Title:      &title,
		},
		Options: []acp.PermissionOption{
			{OptionId: "allow-once", Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: "reject-once", Name: "Reject", Kind: acp.PermissionOptionKindRejectOnce},
		},
	}
}

func TestPermissionDeny(t *testing.T) {
	client := newProbeClient(io.Discard, strings.NewReader(""), false, false, PermissionDeny)
	resp, err := client.RequestPermission(context.Background(), permissionRequest())
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if resp.Outcome.Selected == nil {
		t.Fatal("expected a selected outcome")
	}
	if got := resp.Outcome.Selected.OptionId; got != "reject-once" {
		t.Fatalf("selected %q, want reject-once", got)
	}
}

func TestPermissionAllow(t *testing.T) {
	client := newProbeClient(io.Discard, strings.NewReader(""), false, false, PermissionAllow)
	resp, err := client.RequestPermission(context.Background(), permissionRequest())
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if resp.Outcome.Selected == nil {
		t.Fatal("expected a selected outcome")
	}
	if got := resp.Outcome.Selected.OptionId; got != "allow-once" {
		t.Fatalf("selected %q, want allow-once", got)
	}
}

func TestPermissionAskWithoutTTYDenies(t *testing.T) {
	client := newProbeClient(io.Discard, strings.NewReader(""), false, false, PermissionAsk)
	resp, err := client.RequestPermission(context.Background(), permissionRequest())
	if err != nil {
		t.Fatalf("RequestPermission: %v", err)
	}
	if resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != "reject-once" {
		t.Fatalf("unexpected outcome: %#v", resp.Outcome)
	}
}
