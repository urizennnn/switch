package chatcli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/urizennnn/switch/internal/chat"
	"github.com/urizennnn/switch/internal/config"
	"github.com/urizennnn/switch/internal/driver/acpdriver"
	"github.com/urizennnn/switch/internal/rules"
	"github.com/urizennnn/switch/internal/store"
)

// Run executes `switch chat` and returns a process exit code.
func Run(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	agent := fs.String("agent", "", "agent adapter (defaults to project config)")
	model := fs.String("model", "", "model config value (defaults to project config)")
	cwd := fs.String("cwd", ".", "project directory")
	newSession := fs.Bool("new", false, "start a new session instead of resuming")
	permission := fs.String("permission", "ask", "permission mode: ask, allow, or deny")
	agentLogs := fs.Bool("agent-logs", false, "stream adapter stderr to the terminal")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	agentExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "agent" {
			agentExplicit = true
		}
	})

	mode, err := acpdriver.ParsePermissionMode(*permission)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 2
	}

	project, err := filepath.Abs(*cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}

	cfg, created, err := config.LoadOrInit(project)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}
	if len(created) > 0 {
		fmt.Printf("initialized switch in %s\n", project)
		for _, path := range created {
			fmt.Printf("  created %s\n", path)
		}
		fmt.Println()
	}
	if *agent == "" {
		*agent = cfg.Agent
	}
	if *model == "" && (!agentExplicit || *agent == cfg.Agent) {
		*model = cfg.Model
	}

	ruleSet, err := rules.Load(cfg.Rules)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}

	state, err := store.Open(config.StatePath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: open state: %v\n", err)
		return 1
	}
	defer state.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var logs io.Writer
	if *agentLogs {
		logs = os.Stderr
	}
	conversation, status, err := chat.Open(ctx, chat.Options{
		Project:   project,
		Agent:     *agent,
		Model:     *model,
		Rules:     ruleSet,
		Store:     state,
		New:       *newSession,
		AgentLogs: logs,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}
	defer conversation.Close(context.Background())

	printStatus(conversation, status, ruleSet)

	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := readLine(reader, "you › ")
		if err != nil {
			fmt.Fprintln(os.Stderr)
			return 0
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			switch handleCommand(ctx, conversation, ruleSet, reader, line) {
			case commandQuit:
				return 0
			case commandContinue:
				continue
			}
			continue
		}
		if err := runTurn(ctx, conversation, reader, line, mode); err != nil {
			if ctx.Err() != nil {
				return 0
			}
			fmt.Fprintf(os.Stderr, "\nchat: %v\n", err)
		}
	}
}

type commandResult int

const (
	commandContinue commandResult = iota
	commandQuit
)

func handleCommand(ctx context.Context, conversation *chat.Chat, ruleSet rules.Set, reader *bufio.Reader, line string) commandResult {
	fields := strings.Fields(line)
	command := fields[0]
	switch command {
	case "/quit", "/exit":
		return commandQuit
	case "/help":
		fmt.Println(`commands:
  /rules        show effective global/project rules
  /session      show the current session
  /new          start a new ACP session
  /quit         exit
Anything else is sent to the agent.`)
	case "/rules":
		if ruleSet.Empty() {
			fmt.Println("no rules loaded")
			return commandContinue
		}
		fmt.Println(ruleSet.Text())
	case "/session":
		fmt.Printf("agent:   %s\nmodel:   %s\nsession: %s\n", conversation.Agent, conversation.Model, conversation.Session.ID)
	case "/new":
		next, status, err := conversation.NewSession(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "chat: new session: %v\n", err)
			return commandContinue
		}
		*conversation = *next
		fmt.Printf("session: %s (new)\n", status.SessionID)
	default:
		fmt.Printf("unknown command %s (try /help)\n", command)
	}
	return commandContinue
}

func runTurn(ctx context.Context, conversation *chat.Chat, reader *bufio.Reader, text string, mode acpdriver.PermissionMode) error {
	fmt.Println()
	done := make(chan turnResult, 1)
	go func() {
		response, err := conversation.Send(ctx, text)
		done <- turnResult{response: response, err: err}
	}()

	var assistant strings.Builder
	turnSeen := false
	sendDone := false
	for {
		select {
		case event := <-conversation.Session.Events:
			switch event.Type {
			case acpdriver.EventMessage:
				fmt.Print(event.Text)
				assistant.WriteString(event.Text)
			case acpdriver.EventThought:
				// Keep the transcript readable; thoughts are internal.
			case acpdriver.EventToolCall:
				if event.Tool != nil {
					fmt.Printf("\n[tool] %s (%s)\n", event.Tool.Title, event.Tool.Status)
				}
			case acpdriver.EventToolEdit:
				if event.Tool != nil && (event.Tool.Status == "failed" || event.Tool.Status == "completed") && event.Tool.Title != "" {
					fmt.Printf("[tool] %s: %s\n", event.Tool.Title, event.Tool.Status)
				}
			case acpdriver.EventPlan:
				for _, entry := range event.Plan {
					fmt.Printf("[plan] %s: %s\n", entry.Status, entry.Content)
				}
			case acpdriver.EventTurn:
				fmt.Println()
				turnSeen = true
				if err := conversation.SaveAssistant(ctx, assistant.String()); err != nil {
					fmt.Fprintf(os.Stderr, "chat: save assistant message: %v\n", err)
				}
				if sendDone {
					return nil
				}
			case acpdriver.EventError:
				if event.Err != nil {
					fmt.Fprintf(os.Stderr, "\nchat: %v\n", event.Err)
				}
			case acpdriver.EventExit:
				return fmt.Errorf("adapter exited")
			}

		case request := <-conversation.Session.Permissions():
			respondPermission(request, reader, mode)

		case result := <-done:
			sendDone = true
			if result.err != nil {
				return result.err
			}
			if turnSeen {
				return nil
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type turnResult struct {
	response any
	err      error
}

func respondPermission(request *acpdriver.PermissionRequest, reader *bufio.Reader, mode acpdriver.PermissionMode) {
	fmt.Printf("\n[permission] %s\n", request.Title)
	for i, option := range request.Options {
		fmt.Printf("  %d. %s (%s)\n", i+1, option.Name, option.Kind)
	}

	switch mode {
	case acpdriver.PermissionAllow:
		if id := findOption(request.Options, "allow"); id != "" {
			request.Respond(id)
		} else {
			request.Cancel()
		}
		return
	case acpdriver.PermissionDeny:
		if id := findOption(request.Options, "reject"); id != "" {
			request.Respond(id)
		} else {
			request.Cancel()
		}
		return
	}

	fmt.Printf("choose an option [1-%d, empty = deny]: ", len(request.Options))
	line, err := reader.ReadString('\n')
	if err != nil {
		request.Cancel()
		return
	}
	line = strings.TrimSpace(line)
	if line == "" {
		request.Cancel()
		return
	}
	var index int
	if _, err := fmt.Sscanf(line, "%d", &index); err != nil {
		request.Cancel()
		return
	}
	index--
	if index < 0 || index >= len(request.Options) {
		request.Cancel()
		return
	}
	request.Respond(request.Options[index].ID)
}

func findOption(options []acpdriver.PermissionOption, prefix string) string {
	for _, option := range options {
		if strings.HasPrefix(option.Kind, prefix) {
			return option.ID
		}
	}
	return ""
}

func readLine(reader *bufio.Reader, prompt string) (string, error) {
	fmt.Print(prompt)
	return reader.ReadString('\n')
}

func printStatus(conversation *chat.Chat, status chat.Status, ruleSet rules.Set) {
	fmt.Printf("switch chat\n")
	fmt.Printf("agent:   %s\n", status.Agent)
	if status.Model != "" {
		fmt.Printf("model:   %s\n", status.Model)
	}
	state := "new"
	if status.Resumed {
		state = "resumed"
	}
	fmt.Printf("session: %s (%s)\n", status.SessionID, state)
	if status.ResumeError != nil {
		fmt.Printf("resume:  failed, started new session (%v)\n", status.ResumeError)
	}
	if ruleSet.Empty() {
		fmt.Println("rules:   none loaded")
	} else {
		scopes := make([]string, 0, len(ruleSet.Sources))
		for _, source := range ruleSet.Sources {
			scopes = append(scopes, source.Scope)
		}
		fmt.Printf("rules:   %s\n", strings.Join(scopes, ", "))
	}
	fmt.Println("type /help for commands, /quit to exit")
	fmt.Println()
}
