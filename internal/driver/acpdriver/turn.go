package acpdriver

import (
	"context"
	"fmt"
	"strings"
)

// TurnOptions controls RunTurn.
type TurnOptions struct {
	// Permission answers requests automatically when OnPermission is nil.
	Permission PermissionMode
	// OnText receives message, thought, and tool progress text.
	OnText func(string)
	// OnPermission takes ownership of each request and must answer it.
	OnPermission func(*PermissionRequest)
}

// RunTurn sends one prompt, streams it, and blocks until the turn ends. It
// returns the assistant's message text.
func (s *Session) RunTurn(ctx context.Context, prompt string, opts TurnOptions) (string, error) {
	onText := opts.OnText
	if onText == nil {
		onText = func(string) {}
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.Prompt(ctx, prompt)
		done <- err
	}()

	var output strings.Builder
	for {
		select {
		case event := <-s.Events:
			switch event.Type {
			case EventMessage:
				output.WriteString(event.Text)
				onText(event.Text)
			case EventThought:
				onText(event.Text)
			case EventToolCall:
				if event.Tool != nil {
					onText(fmt.Sprintf("\n[tool] %s (%s)\n", event.Tool.Title, event.Tool.Status))
				}
			case EventToolEdit:
				if event.Tool != nil && (event.Tool.Status == "completed" || event.Tool.Status == "failed") {
					onText(fmt.Sprintf("[tool] %s: %s\n", event.Tool.Title, event.Tool.Status))
				}
			case EventTurn:
				// Prompt emits EventTurn after every update of the turn, so the
				// output is complete here; returning on done could drop the tail.
				onText("\n")
				return output.String(), nil
			case EventError:
				if event.Err != nil {
					return "", event.Err
				}
			case EventExit:
				return "", fmt.Errorf("%s adapter exited", s.Agent)
			}
		case request := <-s.Permissions():
			if opts.OnPermission != nil {
				opts.OnPermission(request)
			} else {
				request.Resolve(opts.Permission)
			}
		case err := <-done:
			if err != nil {
				return "", err
			}
			done = nil // success: keep draining until EventTurn
		case <-s.Done():
			return "", fmt.Errorf("%s adapter exited", s.Agent)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// Resolve answers the request without asking: allow picks the first allow
// option, deny the first reject option, and anything else cancels.
func (p *PermissionRequest) Resolve(mode PermissionMode) {
	prefix := ""
	switch mode {
	case PermissionAllow:
		prefix = "allow"
	case PermissionDeny:
		prefix = "reject"
	}
	if prefix != "" {
		for _, option := range p.Options {
			if strings.HasPrefix(option.Kind, prefix) {
				p.Respond(option.ID)
				return
			}
		}
	}
	p.Cancel()
}
