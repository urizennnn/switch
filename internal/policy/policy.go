// Package policy defines Switch's session-level responses to adapter requests.
// It is not an OS sandbox: adapters may act without sending a request.
package policy

import "strings"

type Mode string

const (
	Build Mode = "build"
	Plan  Mode = "plan"
	Full  Mode = "full"
)

type Decision int

const (
	Ask Decision = iota
	Allow
	Deny
)

func Parse(s string) (Mode, bool) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case Build:
		return Build, true
	case Plan:
		return Plan, true
	case Full:
		return Full, true
	default:
		return "", false
	}
}

// Decide only auto-allows explicitly identified reads in build and plan.
// Unknown operations always prompt in build and are denied in plan.
func Decide(mode Mode, kind string) Decision {
	read := strings.EqualFold(kind, "read") || strings.EqualFold(kind, "search")
	switch mode {
	case Plan:
		if read {
			return Allow
		}
		return Deny
	case Full:
		return Allow
	default:
		if read {
			return Allow
		}
		return Ask
	}
}
