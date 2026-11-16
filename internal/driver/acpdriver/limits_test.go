package acpdriver

import (
	"errors"
	"testing"
)

func TestLimitReason(t *testing.T) {
	for _, c := range []struct {
		stop string
		err  error
		hit  bool
	}{
		{"max_tokens", nil, true},
		{"max_turn_requests", nil, true},
		{"end_turn", nil, false},
		{"", errors.New("Claude AI usage limit reached|1731600000"), true},
		{"", errors.New("429 Too Many Requests"), true},
		{"", errors.New("prompt is too long: 210000 tokens > 200000 maximum"), true},
		{"", errors.New("adapter exited"), false},
	} {
		if got := LimitReason(c.stop, c.err) != ""; got != c.hit {
			t.Errorf("LimitReason(%q, %v) hit = %v", c.stop, c.err, got)
		}
	}
}
