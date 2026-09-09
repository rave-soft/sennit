package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rave-soft/sennit/internal/agent/prompt"
	"github.com/rave-soft/sennit/internal/agent/tools"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/message"
)

func (d *delegationFinalizer) snapshotDelegation(args *tools.TaskCreateArgs, definition *config.Agent, contexts ...context.Context) error {
	ctx := context.Background()
	if len(contexts) != 0 {
		ctx = contexts[0]
	}
	selected := d.cfg.Config().Model
	if definition.Model != "" {
		match, err := config.ResolveModelString(d.cfg.Config().Providers.Copy(), definition.Model)
		if err != nil {
			return err
		}
		selected = config.SelectedModel{Provider: match.Provider, Model: match.ModelID}
	}
	if definition.Model == "" {
		definition.Model = selected.Provider + "/" + selected.Model
		if definition.ReasoningEffort == "" {
			definition.ReasoningEffort = selected.ReasoningEffort
		}
	}
	spec := DelegationExecution{AgentID: args.AgentID, ParentSessionID: args.ParentSessionID, SessionID: args.SessionID, Goal: args.Goal, Depth: args.Depth, SessionTitle: args.SessionTitle, Definition: *definition, Model: selected}
	if args.AgentID != "" && d.sessions != nil && d.messages != nil {
		prior, err := d.sessions.ListSubAgentSessions(ctx, args.ParentSessionID, args.AgentID, args.SessionID)
		if err != nil {
			return fmt.Errorf("snapshot delegation sessions: %w", err)
		}
		for _, priorSession := range prior {
			messages, err := d.messages.List(ctx, priorSession.ID)
			if err != nil {
				return fmt.Errorf("snapshot delegation messages: %w", err)
			}
			if messages = trimToSummary(priorSession, messages); len(messages) != 0 {
				captured := make([]delegationHistoryMessage, 0, len(messages))
				for _, item := range messages {
					parts, err := message.MarshalParts(item.Parts)
					if err != nil {
						return fmt.Errorf("snapshot delegation content: %w", err)
					}
					item.Parts = nil
					captured = append(captured, delegationHistoryMessage{Message: item, Parts: parts})
				}
				spec.History = append(spec.History, captured)
			}
		}
	}
	if options := d.cfg.Config().Options; options != nil {
		spec.Options = DelegationRuntimeOptions{
			DisableAutoSummarize: options.DisableAutoSummarize,
			AutoSummarizeAt:      options.AutoSummarizeAt,
		}
	}
	spec.HistoryFrozen = true
	data, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	args.Execution = string(data)
	args.Factory = func(ctx context.Context, sessionID string) (func(context.Context) (tools.TaskRunResult, error), func(), error) {
		var captured DelegationExecution
		if err := json.Unmarshal(data, &captured); err != nil {
			return nil, nil, err
		}
		captured.SessionID = sessionID
		run, err := d.buildDelegationRun(ctx, captured)
		return run, nil, err
	}
	return nil
}

type delegationHistoryMessage struct {
	Message message.Message
	Parts   json.RawMessage
}

type DelegationRuntimeOptions struct {
	DisableAutoSummarize bool
	AutoSummarizeAt      int64
}

type DelegationExecution struct {
	AgentID         string
	ParentSessionID string
	SessionID       string
	SessionTitle    string
	Goal            string
	Depth           int
	Definition      config.Agent
	Model           config.SelectedModel
	History         [][]delegationHistoryMessage
	HistoryFrozen   bool
	Options         DelegationRuntimeOptions
}

func BuildDelegationRun(ctx context.Context, owner Coordinator, spec DelegationExecution) (func(context.Context) (tools.TaskRunResult, error), error) {
	c, ok := owner.(*coordinator)
	if !ok {
		return nil, fmt.Errorf("delegation runner requires a workspace coordinator")
	}
	return c.delegation.buildDelegationRun(ctx, spec)
}

func (d *delegationFinalizer) buildDelegationRun(ctx context.Context, spec DelegationExecution) (func(context.Context) (tools.TaskRunResult, error), error) {
	id := spec.AgentID
	if id == "" {
		id = config.AgentTask
	}
	definition := spec.Definition
	if definition.ID == "" {
		return nil, fmt.Errorf("delegation execution snapshot is missing")
	}
	var p *prompt.Prompt
	var err error
	if spec.AgentID == "" {
		p, err = builtinDelegatePrompt(prompt.WithWorkingDir(d.cfg.WorkingDir()))
	} else {
		p, err = prompt.NewPrompt(id, delegatedAgentPrompt(definition.Prompt), prompt.WithWorkingDir(d.cfg.WorkingDir()), prompt.ForSubagent())
	}
	if err != nil {
		return nil, err
	}
	runner, err := d.buildAgentWithOptions(ctx, p, definition, true, &spec.Options, spec.Model)
	if err != nil {
		return nil, err
	}
	history := make([][]message.Message, 0, len(spec.History))
	for _, prior := range spec.History {
		messages := make([]message.Message, 0, len(prior))
		for _, captured := range prior {
			item := captured.Message
			item.Parts, err = message.UnmarshalParts(captured.Parts, item.ID)
			if err != nil {
				return nil, fmt.Errorf("restore delegation history: %w", err)
			}
			messages = append(messages, item)
		}
		history = append(history, messages)
	}
	return func(ctx context.Context) (tools.TaskRunResult, error) {
		response, err := d.runSubAgent(ctx, subAgentParams{
			Agent: runner, SessionID: spec.ParentSessionID, ChildSessionID: spec.SessionID,
			Prompt: spec.Goal, Depth: spec.Depth, AgentID: spec.AgentID,
			History: history, HistoryFrozen: spec.HistoryFrozen,
		})
		if err != nil {
			return tools.TaskRunResult{}, err
		}
		if response.IsError {
			return tools.TaskRunResult{}, fmt.Errorf("delegation failed: %s", response.Content)
		}
		return tools.TaskRunResult{Text: response.Content}, nil
	}, nil
}
