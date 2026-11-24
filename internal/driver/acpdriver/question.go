package acpdriver

import "context"

// KindQuestion marks a request that puts an agent's question to the user,
// switch_question, rather than asking leave for a tool call.
const KindQuestion = "question"

// NewQuestion is a request for the user's answer to an agent's question. Its
// options, when it has any, are the answers to pick from, each its own ID;
// without them the user's own words are the answer.
func NewQuestion(agent, question string, options []string) *PermissionRequest {
	request := &PermissionRequest{Agent: agent, Title: oneLine(question), Kind: KindQuestion, response: make(chan PermissionDecision, 1)}
	for _, option := range options {
		if option = oneLine(option); option != "" {
			request.Options = append(request.Options, PermissionOption{ID: option, Name: option})
		}
	}
	return request
}

// Answer waits for the answer to a question until ctx ends; ok is false when
// the user skipped it or it was cancelled. Once ctx ends nobody waits for it,
// and the request says so: see Withdrawn.
func (p *PermissionRequest) Answer(ctx context.Context) (answer string, ok bool) {
	select {
	case decision := <-p.response:
		return decision.OptionID, !decision.Cancel && decision.OptionID != ""
	case <-ctx.Done():
		p.Cancel()
		return "", false
	}
}

// Withdrawn reports whether a request still waiting for the user's answer is
// no longer wanted, such as a question whose asker's turn ended: a decision
// is there that nobody will read.
func (p *PermissionRequest) Withdrawn() bool { return len(p.response) > 0 }
