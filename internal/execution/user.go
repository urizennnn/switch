package execution

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/urizennnn/switch/internal/driver/acpdriver"
)

// Question puts a worker's question to the user, the way its permission
// requests go, and waits until they answer or ctx ends; its task's idle
// watchdog waits too. A run nobody watches has no one to ask: it says so,
// and [run] notify tells the user what was asked.
func (e *Engine) Question(ctx context.Context, agent, taskID, question string, options []string) string {
	if e.opts.OnPermission == nil {
		e.notify("question", taskID, agent+" asks: "+question)
	} else {
		asking := agent + " question" // its own key, which the agent's next event does not clear, as names have no spaces
		e.beat(taskID, asking, true)
		defer e.beat(taskID, asking, false)
	}
	return AskUser(ctx, e.opts.OnPermission, agent, question, options)
}

// AskUser shows an agent's question to the user through ask, among their
// permission prompts, and returns the answer once they give it, or what to
// do without one. A nil ask means no user is watching. Once ctx ends the
// question goes through ask again, withdrawn, so its prompt leaves the
// screen rather than take an answer nobody reads.
func AskUser(ctx context.Context, ask func(*acpdriver.PermissionRequest), agent, question string, options []string) string {
	if ask == nil {
		return "no user is watching; decide yourself or switch_block"
	}
	request := acpdriver.NewQuestion(agent, question, options)
	ask(request)
	if answer, ok := request.Answer(ctx); ok {
		return answer
	}
	if request.Withdrawn() {
		ask(request)
	}
	return "the user did not answer; decide yourself or switch_block"
}

// notify runs [run] notify when a run nobody watches needs the user: event is
// review, blocked, completed or question. The command gets 10 seconds, and
// the engine does not wait for it. A TUI shows these itself.
func (e *Engine) notify(event, taskID, message string) {
	if e.opts.Config.Notify == "" || !e.opts.Headless {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	cmd := exec.CommandContext(ctx, "sh", "-c", e.opts.Config.Notify)
	cmd.Env = append(os.Environ(), "SWITCH_EVENT="+event, "SWITCH_RUN="+e.run.ID, "SWITCH_TASK="+planID(taskID), "SWITCH_MESSAGE="+firstLine(message))
	if cmd.Start() != nil {
		cancel()
		return
	}
	go func() {
		_ = cmd.Wait()
		cancel()
	}()
}
