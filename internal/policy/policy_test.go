package policy

import "testing"

func TestModes(t *testing.T) {
	for _, tc := range []struct {
		mode   Mode
		kind   string
		inside bool
		want   Decision
	}{
		{Build, "read", true, Allow}, {Build, "execute", true, Ask}, {Build, "", true, Ask},
		{Plan, "search", true, Allow}, {Plan, "execute", true, Deny}, {Plan, "", true, Deny},
		{Full, "execute", false, Allow}, {Full, "", false, Allow},
		// Reads outside the requester's directory, such as ~/.ssh, are not
		// automatic.
		{Build, "read", false, Ask}, {Build, "search", false, Ask}, {Plan, "read", false, Deny},
		// Edits ask even inside a worktree: verify later runs what they wrote.
		{Build, "edit", true, Ask}, {Build, "delete", true, Ask}, {Build, "move", true, Ask},
		{Plan, "edit", true, Deny},
	} {
		if got := Decide(tc.mode, tc.kind, tc.inside); got != tc.want {
			t.Errorf("%s %q inside=%v: got %v, want %v", tc.mode, tc.kind, tc.inside, got, tc.want)
		}
	}
	if _, ok := Parse("unsafe"); ok {
		t.Fatal("unknown mode accepted")
	}
}
