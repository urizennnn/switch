package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/policy"
)

// permissionPause is how long a prompt must be on screen, with no typing,
// before a key can answer it.
const permissionPause = 500 * time.Millisecond

type pendingPermission struct {
	request *acpdriver.PermissionRequest
	job     bool // a run worker's: it goes when the run ends
}

// askPermission settles what the session policy covers and shows the rest one
// at a time: a request that arrives while another is on screen waits behind
// it. Its source keeps listening meanwhile, so no agent's output waits on
// another agent's prompt. A request that comes back withdrawn, such as a
// question whose turn ended, leaves the screen and the queue.
func (m *model) askPermission(request *acpdriver.PermissionRequest, mode policy.Mode, job bool) {
	if request == nil {
		return
	}
	if request.Withdrawn() {
		m.permissionQueue = slices.DeleteFunc(m.permissionQueue, func(p pendingPermission) bool { return p.request == request })
		if m.permission == request {
			m.nextPermission()
		}
		return
	}
	decision := policy.Decide(mode, request.Kind, request.Inside)
	if request.Kind == acpdriver.KindQuestion {
		decision = policy.Ask // only the user answers an agent's question, whatever the mode
	}
	switch decision {
	case policy.Allow:
		request.Resolve(acpdriver.PermissionAllow)
	case policy.Deny:
		request.Resolve(acpdriver.PermissionDeny)
	default:
		m.permissionQueue = append(m.permissionQueue, pendingPermission{request, job})
		if m.permission == nil {
			m.nextPermission()
		}
	}
}

// nextPermission puts the first waiting prompt on screen, if there is one.
func (m *model) nextPermission() {
	m.permission = nil
	m.answer.Reset()
	m.permissionQueue = slices.DeleteFunc(m.permissionQueue, func(p pendingPermission) bool { return p.request.Withdrawn() })
	if len(m.permissionQueue) > 0 {
		next := m.permissionQueue[0]
		m.permissionQueue = m.permissionQueue[1:]
		m.permission, m.permissionJob, m.permissionHold = next.request, next.job, time.Now().Add(permissionPause)
	}
}

// dropWithdrawn takes a prompt nobody waits for any more off the screen,
// such as one whose turn ended or was stopped.
func (m *model) dropWithdrawn() {
	if m.permission != nil && m.permission.Withdrawn() {
		m.nextPermission()
	}
}

// withdrawPrompts answers an agent's prompts, and its subagents', as
// cancelled once its turn is stopped, as ACP asks of a client after
// session/cancel.
func (m *model) withdrawPrompts(agent string) {
	mine := func(r *acpdriver.PermissionRequest) bool {
		return r.Agent == agent || strings.HasPrefix(r.Agent, agent+"/")
	}
	for _, p := range m.permissionQueue {
		if mine(p.request) {
			p.request.Cancel()
		}
	}
	if m.permission != nil && mine(m.permission) {
		m.permission.Cancel()
	}
	m.dropWithdrawn()
	m.permissionQueue = slices.DeleteFunc(m.permissionQueue, func(p pendingPermission) bool { return p.request.Withdrawn() })
}

// answering reports whether the user types the answer to the question on
// screen, one without options, in the prompt's own field rather than the
// input, which keeps their message: from when they have had time to read it,
// as typing before then is their message going on.
func (m model) answering() bool {
	return m.permission != nil && m.permission.Kind == acpdriver.KindQuestion && len(m.permission.Options) == 0 &&
		!time.Now().Before(m.permissionHold)
}

// promptName is what the prompt on screen is.
func promptName(p *acpdriver.PermissionRequest) string {
	if p.Kind == acpdriver.KindQuestion {
		return "question"
	}
	return "permission"
}

// promptKeys says how to answer the prompt on screen: a question is skipped
// rather than denied, and one without options takes typed words.
func promptKeys(p *acpdriver.PermissionRequest) string {
	no := "esc deny"
	if p.Kind == acpdriver.KindQuestion {
		no = "esc skip"
		if len(p.Options) == 0 {
			return "type your answer · enter send · " + no
		}
	}
	if n := len(p.Options); n > 0 {
		return fmt.Sprintf("1-%d choose · %s", n, no)
	}
	return no
}

// helperMode is the policy for chat teammates and subagents. Full mode does
// not cover them: they still ask before non-read operations.
func (m model) helperMode() policy.Mode {
	if m.permissionMode == policy.Full {
		return policy.Build
	}
	return m.permissionMode
}
