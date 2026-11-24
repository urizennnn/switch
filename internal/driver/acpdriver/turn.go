package acpdriver

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TurnOptions controls RunTurn.
type TurnOptions struct {
	// Permission answers requests automatically when OnPermission is nil.
	Permission PermissionMode
	// OnText receives message, thought, and tool progress text.
	OnText func(string)
	// OnPermission takes ownership of each request and must answer it.
	OnPermission func(*PermissionRequest)
	// OnUsage receives the turn's token counts and the latest reported cost.
	OnUsage func(UsageInfo)
	// OnEvent receives every session event of the turn, for live views.
	OnEvent func(Event)
}

// RunTurn sends one prompt, streams it, and blocks until the turn ends. It
// returns the assistant's message text.
func (s *Session) RunTurn(ctx context.Context, prompt string, opts TurnOptions) (string, error) {
	onText := opts.OnText
	if onText == nil {
		onText = func(string) {}
	}

	s.drain() // left by an earlier turn or another reader, not this turn's
	done := make(chan error, 1)
	go func(done chan<- error) { // its own copy: the turn sets done to nil
		_, err := s.Prompt(ctx, prompt)
		done <- err
	}(done)
	// A turn that ends early waits for its Prompt, which returns promptly once
	// ctx ends, so that its error and late updates cannot reach the next turn.
	// Not for long: a Prompt stuck writing to an adapter that stopped reading
	// does not see ctx. The next turn's drain takes what it sends later.
	defer func() {
		late := time.After(2 * time.Second)
		for done != nil {
			select {
			case <-done:
				done = nil
			case <-s.events:
			case <-s.done:
				done = nil
			case <-late:
				done = nil
			}
		}
		s.drain()
	}()

	var output strings.Builder
	var cost float64
	var currency string
	for {
		select {
		case event := <-s.Events:
			if opts.OnEvent != nil {
				opts.OnEvent(event)
			}
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
				if event.Tool != nil && event.Tool.Title != "" &&
					(event.Tool.Status == "completed" || event.Tool.Status == "failed") {
					onText(fmt.Sprintf("[tool] %s: %s\n", event.Tool.Title, event.Tool.Status))
				}
			case EventUsage:
				if event.Usage != nil && event.Usage.CostAmount > 0 {
					cost, currency = event.Usage.CostAmount, event.Usage.CostCurrency
				}
			case EventTurn:
				if opts.OnUsage != nil {
					usage := UsageInfo{CostAmount: cost, CostCurrency: currency}
					if event.Usage != nil {
						usage.InputTokens = event.Usage.InputTokens
						usage.OutputTokens = event.Usage.OutputTokens
						usage.TotalTokens = event.Usage.TotalTokens
					}
					opts.OnUsage(usage)
				}
				// Prompt emits EventTurn after every update of the turn, so the
				// output is complete here; returning on done could drop the tail.
				onText("\n")
				return output.String(), nil
			case EventError:
				if event.Err != nil {
					return "", event.Err
				}
			case EventExit:
				return "", s.exited()
			}
		case request := <-s.Permissions():
			if opts.OnPermission != nil {
				opts.OnPermission(request)
			} else {
				request.Resolve(opts.Permission)
			}
		case err := <-done:
			done = nil // on success, keep draining until EventTurn
			if err != nil {
				return "", err
			}
		case <-s.Done():
			return "", s.exited()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// drain drops queued events and refuses queued permission requests: they
// belong to a turn that is over, and the next turn would take them as its own.
func (s *Session) drain() {
	for {
		select {
		case <-s.events:
		case request := <-s.permissions:
			request.Cancel()
		default:
			return
		}
	}
}

// Resolve answers the request without asking, with the option pick finds,
// and cancels it when there is none.
func (p *PermissionRequest) Resolve(mode PermissionMode) {
	if option, ok := pick(p.Options, mode); ok {
		p.Respond(option.ID)
		return
	}
	p.Cancel()
}

// pick is the option that answers for mode without asking: allow picks an
// allow option, deny a reject one, and anything else none.
func pick(options []PermissionOption, mode PermissionMode) (PermissionOption, bool) {
	prefix := ""
	switch mode {
	case PermissionAllow:
		prefix = "allow"
	case PermissionDeny:
		prefix = "reject"
	}
	if prefix != "" {
		// Prefer one-shot choices. An adapter's persistent allow option must
		// never silently outlive Switch's selected policy.
		for _, option := range options {
			if option.Kind == prefix+"_once" {
				return option, true
			}
		}
		for _, option := range options {
			if strings.HasPrefix(option.Kind, prefix) && !strings.HasSuffix(option.Kind, "_always") {
				return option, true
			}
		}
	}
	return PermissionOption{}, false
}
