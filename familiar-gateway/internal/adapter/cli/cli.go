package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/chzyer/readline"
	"github.com/familiar/gateway/internal/config"
	"github.com/familiar/gateway/internal/engine"
	"github.com/familiar/gateway/internal/pipeline"
	"github.com/familiar/gateway/internal/session"
	pb "github.com/familiar/gateway/proto/engine"
)

// CLIAdapter is the interactive terminal adapter.
type CLIAdapter struct {
	pipeline *pipeline.Pipeline
	sessions *session.Manager
	engine   engine.Service
	cfg      config.CLIConfig
	verbose  bool
}

// New constructs a CLIAdapter.
func New(p *pipeline.Pipeline, sm *session.Manager, eng engine.Service, cfg config.CLIConfig, verbose bool) *CLIAdapter {
	return &CLIAdapter{
		pipeline: p,
		sessions: sm,
		engine:   eng,
		cfg:      cfg,
		verbose:  verbose,
	}
}

// Run starts the interactive CLI read loop.
func (a *CLIAdapter) Run(ctx context.Context) error {
	fmt.Println("Familiar — type /help for commands, /quit to exit")
	fmt.Println()

	sess := a.sessions.GetOrCreate("cli", "local")
	sess.SetPlatform("cli")

	prompt := a.cfg.Prompt
	if prompt == "" {
		prompt = "> "
	}

	historyFile := a.cfg.HistoryFile
	if historyFile == "" {
		home, _ := os.UserHomeDir()
		historyFile = home + "/.familiar/cli_history"
	}

	// Ensure history directory exists.
	if dir := historyFile[:strings.LastIndex(historyFile, "/")]; dir != "" {
		_ = os.MkdirAll(dir, 0700)
	}

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          prompt,
		HistoryFile:     historyFile,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return fmt.Errorf("readline init: %w", err)
	}
	defer rl.Close()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line, err := rl.Readline()
		if err != nil {
			// EOF or Ctrl-C.
			return nil
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			if done := a.handleCommand(ctx, sess, line); done {
				return nil
			}
			continue
		}

		// Regular message: stream to LLM.
		fmt.Print("\nFamiliar: ")

		response, info, err := a.pipeline.HandleStream(ctx, sess, line, nil, func(chunk string) {
			fmt.Print(chunk)
			// Flush stdout for streaming display.
			os.Stdout.Sync()
		}, func(reasoning string) {
			// Show thinking in dim text for CLI users.
			fmt.Printf("[2m%s[0m", reasoning)
			os.Stdout.Sync()
		}, nil)
		if err != nil {
			fmt.Printf("\n[error: %v]\n\n", err)
			continue
		}

		// Ensure newline after streamed response.
		if !strings.HasSuffix(response, "\n") {
			fmt.Println()
		}

		if a.verbose && info != nil {
			parts := []string{"via " + info.ModelID}
			if info.MemHits > 0 {
				parts = append(parts, fmt.Sprintf("%d memory hits", info.MemHits))
			}
			fmt.Printf("\033[2m[%s]\033[0m\n", strings.Join(parts, ", "))
		}
		fmt.Println()
	}
}

// handleCommand processes a slash command. Returns true if the adapter should exit.
func (a *CLIAdapter) handleCommand(ctx context.Context, sess *session.Session, line string) bool {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return false
	}

	cmd := parts[0]

	switch cmd {
	case "/quit", "/exit":
		fmt.Println("Goodbye.")
		return true

	case "/session":
		fmt.Printf("Session ID: %s (channel: %s, sender: %s)\n", sess.ID, sess.ChannelID, sess.SenderID)
		fmt.Printf("Turns: %d\n", len(sess.RecentTurns(0)))

	case "/memory":
		if len(parts) < 2 {
			fmt.Println("Usage: /memory <query>")
			return false
		}
		query := strings.Join(parts[1:], " ")
		a.queryMemory(ctx, sess, query)

	case "/sleep":
		a.startSleep(ctx)

	case "/briefing":
		a.getBriefing(ctx)

	case "/help":
		fmt.Println("Available commands:")
		fmt.Println("  /quit, /exit         Exit the CLI")
		fmt.Println("  /session             Show current session info")
		fmt.Println("  /memory <query>      Search memory for a query")
		fmt.Println("  /sleep               Start a sleep cycle")
		fmt.Println("  /briefing            Show latest sleep briefing")
		fmt.Println("  /help                Show this help")

	default:
		fmt.Printf("Unknown command: %s (try /help)\n", cmd)
	}

	return false
}

func (a *CLIAdapter) queryMemory(ctx context.Context, sess *session.Session, query string) {
	resp, err := a.engine.QueryMemory(ctx, &pb.MemoryQueryRequest{
		Query: &pb.MemoryQueryRequest_Semantic{
			Semantic: &pb.SemanticQuery{
				QueryText: query,
				Limit:     10,
				Visibility: &pb.VisibilityContext{
					ChannelId: sess.ChannelID,
					UserId:    sess.CanonicalID(),
				},
			},
		},
	})
	if err != nil {
		fmt.Printf("Memory query error: %v\n", err)
		return
	}
	if resp.Error != "" {
		fmt.Printf("Memory query engine error: %s\n", resp.Error)
		return
	}

	if len(resp.Results) == 0 {
		fmt.Println("No memory results found.")
		return
	}

	fmt.Printf("Memory results (%d):\n", len(resp.Results))
	for i, r := range resp.Results {
		if r.Fact == nil {
			continue
		}
		staleness := r.Staleness
		if staleness == "" {
			staleness = "?"
		}
		fmt.Printf("  %d. [%.2f] %s (%s)\n", i+1, r.RelevanceScore, r.Fact.Content, staleness)
	}
}

func (a *CLIAdapter) startSleep(ctx context.Context) {
	fmt.Println("Starting sleep cycle...")
	handle, err := a.engine.StartSleep(ctx, nil)
	if err != nil {
		fmt.Printf("Sleep error: %v\n", err)
		return
	}
	fmt.Printf("Sleep started. Handle: %s\n", handle)
	fmt.Println("Use /briefing after sleep completes to see results.")
}

func (a *CLIAdapter) getBriefing(ctx context.Context) {
	briefing, err := a.engine.GetBriefing(ctx)
	if err != nil {
		fmt.Printf("Briefing error: %v\n", err)
		return
	}
	if briefing.Error != "" {
		fmt.Printf("Briefing engine error: %s\n", briefing.Error)
		return
	}

	fmt.Println("=== Briefing ===")
	if briefing.Summary != "" {
		fmt.Println(briefing.Summary)
	}
	fmt.Printf("Facts merged: %d | Facts expired: %d | Documents processed: %d | Facts verified: %d\n",
		briefing.FactsMerged, briefing.FactsExpired, briefing.DocumentsProcessed, briefing.FactsVerified)
	if len(briefing.NotableUpdates) > 0 {
		fmt.Println("Notable updates:")
		for _, u := range briefing.NotableUpdates {
			fmt.Printf("  - %s\n", u)
		}
	}
	fmt.Println("================")
}
