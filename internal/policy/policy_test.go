package policy

import "testing"

func TestModes(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		kind string
		want Decision
	}{
		{Build, "read", Allow}, {Build, "execute", Ask}, {Build, "", Ask},
		{Plan, "search", Allow}, {Plan, "execute", Deny}, {Plan, "", Deny},
		{Full, "execute", Allow}, {Full, "", Allow},
	} {
		if got := Decide(tc.mode, tc.kind); got != tc.want {
			t.Errorf("%s %q: got %v, want %v", tc.mode, tc.kind, got, tc.want)
		}
	}
	if _, ok := Parse("unsafe"); ok {
		t.Fatal("unknown mode accepted")
	}
}
