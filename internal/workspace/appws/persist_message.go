package appws

import (
	"context"
	"log/slog"

	"github.com/rave-soft/sennit/internal/db"
	"github.com/rave-soft/sennit/internal/message"
	messagestore "github.com/rave-soft/sennit/internal/message/store"
)

// persistShellOutput stores a bang-mode shell command result as a user
// message. If the target session no longer exists (deleted before or
// during the command), persistence is skipped without surfacing an error.
//
// This is domain logic for bang-mode command persistence, not a shell
// utility, so it lives here with its only caller (AgentRunShellCommand)
// rather than in internal/shell.
func persistShellOutput(
	ctx context.Context,
	messages messagestore.Service,
	sessionID, command, output string,
	exitCode int,
) error {
	if sessionID == "" {
		return nil
	}

	_, err := messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role: message.User,
		Parts: []message.ContentPart{message.ShellCommand{
			Command:  command,
			Output:   output,
			ExitCode: exitCode,
		}},
	})
	// The messages table has a single foreign key (session_id), so an FK
	// failure here can only mean the session is gone.
	if db.IsForeignKeyConstraintError(err) {
		slog.Debug(
			"Skipping shell command persistence: session no longer exists",
			"session_id", sessionID,
		)
		return nil
	}
	return err
}
