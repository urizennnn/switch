package acpdriver

import "testing"

func TestAutomaticPermissionDoesNotGrantPersistentApproval(t *testing.T) {
	r := &PermissionRequest{response: make(chan PermissionDecision, 1), Options: []PermissionOption{
		{ID: "always", Kind: "allow_always"}, {ID: "once", Kind: "allow_once"},
	}}
	r.Resolve(PermissionAllow)
	if got := <-r.response; got.OptionID != "once" {
		t.Fatalf("decision = %+v", got)
	}
	r = &PermissionRequest{response: make(chan PermissionDecision, 1), Options: []PermissionOption{{ID: "always", Kind: "allow_always"}}}
	r.Resolve(PermissionAllow)
	if got := <-r.response; !got.Cancel {
		t.Fatalf("persistent-only request should cancel: %+v", got)
	}
}
