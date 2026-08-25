package agent

import (
	"context"

	"github.com/rave-soft/sennit/internal/message"
)

type MessageService interface {
	Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error)
	Update(ctx context.Context, message message.Message) error
	Get(ctx context.Context, id string) (message.Message, error)
	List(ctx context.Context, sessionID string) ([]message.Message, error)
	Delete(ctx context.Context, id string) error
	FlushAll(ctx context.Context) error
}
