// Package notify defines domain notification types for agent events.
// These types are decoupled from UI concerns so the agent can publish
// events without importing UI packages.
package notify

// Type identifies the kind of agent notification.
type Type string

const (
	// TypeAgentFinished indicates the agent has completed its turn.
	TypeAgentFinished Type = "agent_finished"
	// TypeReAuthenticate indicates the agent encountered an
	// authentication error and the user needs to re-authenticate.
	TypeReAuthenticate Type = "re_authenticate"
	// TypeAgentError indicates the agent's turn terminated with an
	// error. The error text is carried in Notification.Message.
	TypeAgentError Type = "error"
	// TypeAWSSSOAuth indicates AWS SSO credentials have expired and the
	// coordinator is running the configured refresh command. It opens the
	// AWS SSO dialog; a follow-up with the same type carries the SSO URL
	// once it appears in the command output. AWSSOCommand carries the
	// command being run; AWSSOURL carries the verification URL when known.
	TypeAWSSSOAuth Type = "aws_sso_auth"
	// TypeAWSSSOAuthResult indicates the AWS SSO refresh command has
	// finished. Message carries the error text when it failed, empty on
	// success.
	TypeAWSSSOAuthResult Type = "aws_sso_auth_result"
	// TypeTurnStarted indicates a turn has genuinely become a session's
	// active run. It is published from the one point that decides that
	// (see sessionAgent.runTurn), so it covers the turn a client asked
	// for and equally the one the session's own queue handed to itself
	// after the previous turn ended - which no client can see coming and
	// which therefore had nothing to start a turn clock with.
	//
	// Lossy and best-effort, like TypeQueueChanged: a dropped event costs
	// an elapsed-time display for one turn, and the next terminal event
	// still clears it. Nothing may treat this as the authority on whether
	// a session is busy - ask the dispatcher for that.
	TypeTurnStarted Type = "turn_started"
	// TypeQueueChanged indicates a session's queued-follow-up count may
	// have changed (a call was enqueued, drained, requeued, canceled, or
	// cleared). It carries no payload beyond SessionID; observers re-probe
	// the queue rather than trust an embedded count. This is a lossy,
	// best-effort signal for refreshing a UI immediately instead of
	// waiting on a TTL backstop - it is not the source of truth for the
	// queue's contents, and a dropped event must still self-heal from
	// that backstop.
	TypeQueueChanged Type = "queue_changed"
	// TypeAccountRotated indicates automatic account rotation (see
	// internal/providers/accounts) switched a provider to a different
	// stored account, either because the active one crossed its usage
	// threshold or because it was rate-limited. Message carries a
	// human-readable summary naming the accounts involved.
	TypeAccountRotated Type = "account_rotated"
	// TypeAccountRotationExhausted indicates a provider's rotation ran
	// out of usable accounts (every candidate disabled, cooling down, or
	// over threshold) and the request is proceeding un-rotated - the
	// original error the request failed with is unaffected. Message
	// carries a human-readable summary, including the reset time when
	// known.
	TypeAccountRotationExhausted Type = "account_rotation_exhausted"
	// TypeUsageLimitWaiting indicates a turn ended because the provider's
	// subscription window is spent, and the session has been parked until
	// that window resets: it picks its own work back up then, with no
	// prompt from the user. Message carries which limit was hit and when
	// the session will continue.
	TypeUsageLimitWaiting Type = "usage_limit_waiting"
	// TypeUsageLimitResumed indicates a parked session's limit has reset
	// and its continuation turn is starting. Message names the provider.
	TypeUsageLimitResumed Type = "usage_limit_resumed"
)

// Notification represents a domain event published by the agent.
type Notification struct {
	SessionID    string
	SessionTitle string
	Type         Type
	ProviderID   string
	// RunID, when non-empty, is the caller-supplied correlator for the run
	// that produced this notification. It lets observers attribute an agent
	// error to a specific request rather than to any in-flight run on the
	// session. Empty when no caller set one.
	RunID string
	// Message carries the error text for TypeAgentError. Other
	// notification types ignore it.
	Message string
	// AWSSOCommand carries the shell command for TypeAWSSSOAuth.
	AWSSOCommand string
	// AWSSOURL carries the SSO verification URL for TypeAWSSSOAuth once it
	// appears in the refresh command's output.
	AWSSOURL string
}

// RunComplete is the authoritative end-of-run signal for a session.
// It is published exactly once per top-level agent run (per
// [sessionAgent.Run] invocation that actually executed) after all
// message updates for the turn have been flushed via
// the message store FlushAll operation. Carries the final assistant text and
// message ID so non-interactive clients can reconcile stdout even if
// SSE events arrive out of order or are dropped by the broker. Error
// is non-empty when the run terminated with an error; Cancelled is
// true when the run terminated due to context cancellation. The two
// are mutually exclusive in the success case but may overlap when a
// cancel triggers a downstream error.
//
// RunID identifies the specific request that produced this event. It is the
// value propagated via agent.WithRunID on the context that reaches the
// coordinator; empty when no caller set one. Filtering
// by RunID lets a client correlate a SendMessage call with its
// terminal event even when the session is busy and other turns are
// finishing on the same session.
type RunComplete struct {
	SessionID string
	RunID     string
	MessageID string
	Text      string
	Error     string
	Cancelled bool
}
