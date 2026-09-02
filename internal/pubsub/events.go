package pubsub

import (
	"context"
)

const (
	CreatedEvent EventType = "created"
	UpdatedEvent EventType = "updated"
	DeletedEvent EventType = "deleted"
)

// Subscriber can subscribe to events of type T.
type Subscriber[T any] interface {
	Subscribe(context.Context) <-chan Event[T]
}

type (
	// EventType identifies the type of event.
	EventType string

	// Event represents an event in the lifecycle of a resource.
	Event[T any] struct {
		Type    EventType `json:"type"`
		Payload T         `json:"payload"`
	}

	// Publisher can publish events of type T.
	//
	// Publish is best-effort and lossy under back-pressure;
	// PublishMustDeliver applies the bounded-blocking semantics used
	// for terminal events that must reach subscribers (finish, tool
	// result, error, cancel, RunComplete). See [Broker.Publish] and
	// [Broker.PublishMustDeliver].
	Publisher[T any] interface {
		Publish(EventType, T)
		PublishMustDeliver(context.Context, EventType, T)
	}
)
