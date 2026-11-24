package acpdriver

import (
	"regexp"
	"strings"
)

// limitError matches provider errors for usage, rate, quota and context
// limits, as Claude Code, Codex and OpenCode report them.
var limitError = regexp.MustCompile(`(?i)usage limit|rate.?limit|quota|too many requests|\b429\b|context (window|length)|maximum context|context_length_exceeded|prompt is too long|too many tokens|out of credits|insufficient credit|limit reached`)

// LimitReason says why a turn stopped at a limit, or "" when it did not: the
// model's token limit, the adapter's request limit, or a provider error about
// usage, rate or context limits.
func LimitReason(stopReason string, err error) string {
	switch stopReason {
	case "max_tokens":
		return "it reached its token limit"
	case "max_turn_requests":
		return "it reached its request limit for one turn"
	}
	if err != nil {
		// Adapter stderr, appended to some errors, is no provider report: a
		// stack frame at line 429 is not a rate limit.
		text, _, _ := strings.Cut(err.Error(), stderrLabel)
		if limitError.MatchString(text) {
			line, _, _ := strings.Cut(text, "\n")
			return line
		}
	}
	return ""
}
