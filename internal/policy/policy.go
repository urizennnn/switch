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

// Decide only auto-allows, in build and plan, explicitly identified reads
// whose every path is inside the requester's own directory. Anything else
// prompts in build and is denied in plan. A worker's edits ask too: a task
// worktree separates git state, not processes, and verify runs what a worker
// wrote on the host before anyone approves it.
func Decide(mode Mode, kind string, inside bool) Decision {
	kind = strings.ToLower(kind)
	read := inside && (kind == "read" || kind == "search")
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
