// Command pi is the Go port of the pi coding agent CLI.
//
// Usage:
//
//	pi [flags] "your prompt"          run a single prompt (print mode)
//	pi [flags]                        interactive REPL
//	pi models                         list available models
//
// Flags:
//
//	-m, --model   model spec ("provider/id" or bare id); default anthropic/claude-sonnet-4-5
//	-p, --print   print mode (non-interactive); also implied when a prompt arg is given
//	    --system  override the system prompt
//	-C, --cwd     working directory (default: current directory)
//	    --think   reasoning level: off|minimal|low|medium|high|xhigh
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/ai/providers"
	"github.com/sky-valley/pi/coding"
)

func main() {
	providers.RegisterBuiltins()

	args := os.Args[1:]
	if len(args) > 0 && args[0] == "models" {
		listModels(args[1:])
		return
	}
	if len(args) > 0 && args[0] == "sessions" {
		listSessions()
		return
	}

	var modelSpec, system, cwd, think, resumePath string
	var printMode, continueLatest bool
	var promptParts []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-m" || a == "--model":
			modelSpec = next()
		case strings.HasPrefix(a, "--model="):
			modelSpec = strings.TrimPrefix(a, "--model=")
		case a == "-p" || a == "--print":
			printMode = true
		case a == "--system":
			system = next()
		case a == "-C" || a == "--cwd":
			cwd = next()
		case a == "--think":
			think = next()
		case a == "-c" || a == "--continue":
			continueLatest = true
		case a == "--resume":
			resumePath = next()
		case a == "-h" || a == "--help":
			usage()
			return
		default:
			promptParts = append(promptParts, a)
		}
	}

	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// Load the resumed session (if any) before resolving the model so the
	// session's saved model/thinking level can be restored (pi createAgentSession
	// restores both from the session context).
	if resumePath == "" && continueLatest {
		if latest, ok := coding.LatestSession(cwd); ok {
			resumePath = latest.Path
		}
	}
	var resumeCtx *coding.BranchContext
	var resumeHasThinkingEntry bool
	if resumePath != "" {
		tree, err := coding.LoadSessionTree(resumePath)
		if err != nil {
			fatal(err)
		}
		ctx := tree.BuildContext()
		resumeCtx = &ctx
		for _, e := range tree.Entries {
			if e.Type == "thinking_level_change" {
				resumeHasThinkingEntry = true
				break
			}
		}
	}

	var model *ai.Model
	parsedThink := ""
	if modelSpec == "" && resumeCtx != nil && resumeCtx.Provider != "" && resumeCtx.ModelID != "" {
		// Restore the session's model when no -m flag is given.
		model = ai.GetModel(resumeCtx.Provider, resumeCtx.ModelID)
		if model == nil {
			fmt.Fprintf(os.Stderr, "\033[33mWarning: Could not restore model %s/%s\033[0m\n", resumeCtx.Provider, resumeCtx.ModelID)
		}
	}
	if model == nil {
		resolved, err := coding.ResolveModelPattern(modelSpec)
		if err != nil {
			fatal(err)
		}
		if resolved.Warning != "" {
			fmt.Fprintf(os.Stderr, "\033[33mWarning: %s\033[0m\n", resolved.Warning)
		}
		model = resolved.Model
		parsedThink = resolved.ThinkingLevel
	}
	apiKey := ai.GetEnvApiKey(model.Provider)
	if apiKey == "" {
		fatal(fmt.Errorf("no API key found for provider %q (set the appropriate *_API_KEY env var)", model.Provider))
	}

	// Thinking level priority: --think flag, then a ":level" suffix on -m, then
	// the resumed session's recorded level (when one was recorded).
	if think == "" {
		think = parsedThink
	}
	if think == "" && resumeCtx != nil && resumeHasThinkingEntry {
		think = resumeCtx.ThinkingLevel
	}

	sess := coding.NewSession(coding.SessionOptions{
		Model:         model,
		Cwd:           cwd,
		SystemPrompt:  system,
		ThinkingLevel: agent.ThinkingLevel(think),
		APIKey:        apiKey,
		// pi enables compaction by default (settings compaction.enabled ?? true).
		Compaction: &coding.DefaultCompactionSettings,
	})

	if resumePath != "" {
		sess.LoadHistory(resumeCtx.Messages)
		fmt.Fprintf(os.Stderr, "\033[2mresumed %d messages from %s\033[0m\n", len(resumeCtx.Messages), resumePath)
		// Resume APPENDS to the existing session file (pi setSessionFile), never
		// forks a new one.
		if rec, err := coding.ResumeSession(resumePath); err == nil {
			if !resumeHasThinkingEntry {
				// pi appends a thinking_level_change when the resumed session
				// lacks one (sdk.ts:359-361).
				rec.RecordThinkingLevel(string(sess.Agent.State().ThinkingLevel))
			}
			sess.Record(rec)
			defer rec.Close()
		}
	} else {
		// Record this session to disk (model_change + thinking_level_change,
		// like pi's createAgentSession for new sessions).
		if rec, err := coding.StartSession(cwd, model, string(sess.Agent.State().ThinkingLevel)); err == nil {
			sess.Record(rec)
			defer rec.Close()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	prompt := strings.Join(promptParts, " ")
	if prompt != "" || printMode {
		if prompt == "" {
			fatal(fmt.Errorf("print mode requires a prompt"))
		}
		out, err := sess.RunPrint(ctx, os.Stdout, prompt)
		fmt.Println()
		if err != nil {
			fatal(err)
		}
		_ = out
		return
	}

	interactive(ctx, sess, model)
}

func interactive(ctx context.Context, sess *coding.Session, model *ai.Model) {
	fmt.Printf("\033[1mpi\033[0m (go) · %s/%s · cwd %s\n", model.Provider, model.ID, sess.Cwd)
	fmt.Println("Type your message. /help for commands, Ctrl-D or /quit to exit.")
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("\n\033[1m> \033[0m")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if quit := handleSlash(sess, line); quit {
				return
			}
			continue
		}
		if _, err := sess.RunPrint(ctx, os.Stdout, line); err != nil {
			fmt.Fprintf(os.Stderr, "\n\033[31merror: %v\033[0m\n", err)
		}
		fmt.Println()
	}
}

var slashHelp = []struct{ name, desc string }{
	{"/help", "show this help"},
	{"/model <spec>", "switch model (keeps transcript)"},
	{"/models [filter]", "list available models"},
	{"/think <level>", "set reasoning: off|minimal|low|medium|high|xhigh"},
	{"/new", "clear the transcript and start fresh"},
	{"/session", "show current session info"},
	{"/sessions", "list saved sessions for this directory"},
	{"/copy", "print the last assistant message"},
	{"/quit", "exit (alias /exit)"},
}

// handleSlash dispatches a slash command, returning true to quit.
func handleSlash(sess *coding.Session, line string) bool {
	parts := strings.Fields(line)
	cmd := parts[0]
	arg := strings.TrimSpace(strings.TrimPrefix(line, cmd))
	switch cmd {
	case "/quit", "/exit":
		return true
	case "/help":
		for _, c := range slashHelp {
			fmt.Printf("  \033[1m%-18s\033[0m %s\n", c.name, c.desc)
		}
	case "/models":
		listModels(parts[1:])
	case "/model":
		if arg == "" {
			fmt.Printf("current: %s/%s\n", sess.Model.Provider, sess.Model.ID)
			break
		}
		m, err := coding.ResolveModel(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "\033[31m%v\033[0m\n", err)
			break
		}
		key := ai.GetEnvApiKey(m.Provider)
		if key == "" {
			fmt.Fprintf(os.Stderr, "\033[31mno API key for provider %q\033[0m\n", m.Provider)
			break
		}
		sess.SetModel(m, key)
		fmt.Printf("switched to %s/%s\n", m.Provider, m.ID)
	case "/think":
		sess.SetThinkingLevel(agent.ThinkingLevel(arg))
		fmt.Printf("thinking level: %s\n", arg)
	case "/new":
		sess.Reset()
		fmt.Println("started a new transcript")
	case "/session":
		if sess.Recorder != nil {
			fmt.Printf("id %s · %d messages · %s\n", sess.Recorder.ID(), len(sess.History()), sess.Recorder.Path())
		} else {
			fmt.Printf("%d messages (not recording)\n", len(sess.History()))
		}
	case "/sessions":
		listSessions()
	case "/copy":
		fmt.Println(sess.LastAssistantText())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (try /help)\n", cmd)
	}
	return false
}

func listModels(args []string) {
	filter := ""
	if len(args) > 0 {
		filter = args[0]
	}
	for _, provider := range sortedProviders() {
		var ids []string
		for _, m := range ai.GetModels(provider) {
			if filter == "" || strings.Contains(m.ID, filter) || strings.Contains(provider, filter) {
				ids = append(ids, m.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		fmt.Printf("\033[1m%s\033[0m\n", provider)
		sortStrings(ids)
		for _, id := range ids {
			fmt.Printf("  %s\n", id)
		}
	}
}

func listSessions() {
	cwd, _ := os.Getwd()
	infos := coding.ListSessions(cwd)
	if len(infos) == 0 {
		fmt.Println("No sessions for this directory.")
		return
	}
	for _, s := range infos {
		fmt.Printf("%s  %s  %d msgs  %s\n", s.Timestamp, s.ID, s.Messages, s.Path)
	}
}

func usage() {
	fmt.Print(`pi - Go port of the pi coding agent

Usage:
  pi [flags] "your prompt"   run a single prompt (print mode)
  pi [flags]                 interactive REPL
  pi models [filter]         list available models
  pi sessions                list saved sessions for this directory

Flags:
  -m, --model SPEC   model ("provider/id" or bare id); default anthropic/claude-sonnet-4-5
  -p, --print        print mode (non-interactive)
  -c, --continue     resume the most recent session for this directory
      --resume PATH  resume a specific session file
      --system TEXT  override the system prompt
  -C, --cwd DIR      working directory (default: current)
      --think LEVEL  reasoning: off|minimal|low|medium|high|xhigh
  -h, --help         show this help
`)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "pi: %v\n", err)
	os.Exit(1)
}

func sortedProviders() []string {
	p := ai.GetProviders()
	sortStrings(p)
	return p
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
