package tui

import (
	"strings"
	"testing"
	"time"
)

// toastText is every live toast's title and body, for assertions.
func (m model) toastText() string {
	var b strings.Builder
	for _, t := range m.toasts {
		b.WriteString(t.title + "\n" + t.body + "\n")
	}
	return b.String()
}

func TestToastsStackExpireAndKeepInput(t *testing.T) {
	m := model{width: 100, height: 30}
	for i := 0; i < 5; i++ {
		m.fail("error", "")
	}
	if len(m.toasts) != maxToasts {
		t.Fatalf("toasts = %d, want %d", len(m.toasts), maxToasts)
	}
	m.notify("saved", "rules updated")
	if !strings.Contains(m.withToasts(m.screen(), 100), "rules updated") {
		t.Fatal("toast not drawn")
	}
	if m.modal != "" {
		t.Fatal("a toast opened a modal")
	}
	for i := range m.toasts {
		m.toasts[i].until = time.Now().Add(-time.Second)
	}
	m.pruneToasts()
	if len(m.toasts) != 0 {
		t.Fatalf("expired toasts kept: %d", len(m.toasts))
	}
}
