package tui

import (
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/policy"
)

// resolvePolicy handles only decisions explicitly covered by the session
// policy. Everything else remains in the ordinary permission dialog.
func (m model) resolvePolicy(request *acpdriver.PermissionRequest) bool {
	if request == nil {
		return false
	}
	switch policy.Decide(m.permissionMode, request.Kind) {
	case policy.Allow:
		request.Resolve(acpdriver.PermissionAllow)
		return true
	case policy.Deny:
		request.Resolve(acpdriver.PermissionDeny)
		return true
	default:
		return false
	}
}
