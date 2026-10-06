package agent

import (
	"fmt"

	"charm.land/fantasy"
)

func usageIsZero(usage fantasy.Usage) bool {
	return usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.TotalTokens == 0 &&
		usage.ReasoningTokens == 0 &&
		usage.CacheCreationTokens == 0 &&
		usage.CacheReadTokens == 0
}

func fallbackStepUsage(messages []fantasy.Message, step fantasy.StepResult) (fantasy.Usage, bool) {
	if !usageIsZero(step.Usage) {
		return step.Usage, false
	}

	inputTokens := estimateMessageTokens(messages)
	outputTokens := estimateStepCompletionTokens(step)
	if inputTokens == 0 && outputTokens == 0 {
		return fantasy.Usage{}, false
	}

	return fantasy.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}, true
}

func cloneFantasyMessages(messages []fantasy.Message) []fantasy.Message {
	cloned := make([]fantasy.Message, len(messages))
	for i, msg := range messages {
		cloned[i] = msg
		cloned[i].Content = append([]fantasy.MessagePart(nil), msg.Content...)
	}
	return cloned
}

func estimateMessageTokens(messages []fantasy.Message) int64 {
	var tokens int64
	for _, msg := range messages {
		tokens += approxTokenCount(string(msg.Role))
		for _, part := range msg.Content {
			tokens += estimateMessagePartTokens(part)
		}
	}
	return tokens
}

func estimateStepCompletionTokens(step fantasy.StepResult) int64 {
	var tokens int64
	for _, content := range step.Content {
		switch c := content.(type) {
		case fantasy.TextContent:
			tokens += approxTokenCount(c.Text)
		case *fantasy.TextContent:
			tokens += approxTokenCount(c.Text)
		case fantasy.ReasoningContent:
			tokens += approxTokenCount(c.Text)
		case *fantasy.ReasoningContent:
			tokens += approxTokenCount(c.Text)
		case fantasy.FileContent:
			tokens += estimateGeneratedFileTokens(c)
		case *fantasy.FileContent:
			tokens += estimateGeneratedFileTokens(*c)
		case fantasy.SourceContent:
			tokens += estimateSourceTokens(c)
		case *fantasy.SourceContent:
			tokens += estimateSourceTokens(*c)
		case fantasy.ToolCallContent:
			tokens += estimateToolCallTokens(c.ToolName, c.Input)
		case *fantasy.ToolCallContent:
			tokens += estimateToolCallTokens(c.ToolName, c.Input)
		case fantasy.ToolResultContent:
			if c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.Result)
			}
		case *fantasy.ToolResultContent:
			if c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.Result)
			}
		}
	}
	return tokens
}

// estimateStepHistoryTokens is how much a finished step adds to the history
// a summary would replace: everything the model produced plus the results of
// the tools it called, which come back into the next request whether the
// provider ran them or this process did.
func estimateStepHistoryTokens(step fantasy.StepResult) int64 {
	return estimateStepCompletionTokens(step) + estimateStepToolResultTokens(step)
}

// estimateStepToolResultTokens is the part of a finished step that no usage
// figure has counted yet: the results of the tools this process ran. The
// provider reported the step's prompt and its reply; the results only show
// up in the next request's prompt.
func estimateStepToolResultTokens(step fantasy.StepResult) int64 {
	var tokens int64
	for _, content := range step.Content {
		switch c := content.(type) {
		case fantasy.ToolResultContent:
			if !c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.Result)
			}
		case *fantasy.ToolResultContent:
			if !c.ProviderExecuted {
				tokens += estimateToolResultContentTokens(c.ToolCallID, c.ToolName, c.Result)
			}
		}
	}
	return tokens
}

func estimateMessagePartTokens(part fantasy.MessagePart) int64 {
	switch p := part.(type) {
	case fantasy.TextPart:
		return approxTokenCount(p.Text)
	case *fantasy.TextPart:
		return approxTokenCount(p.Text)
	case fantasy.ReasoningPart:
		return approxTokenCount(p.Text)
	case *fantasy.ReasoningPart:
		return approxTokenCount(p.Text)
	case fantasy.FilePart:
		return estimateFilePartTokens(p)
	case *fantasy.FilePart:
		return estimateFilePartTokens(*p)
	case fantasy.ToolCallPart:
		return estimateToolCallTokens(p.ToolName, p.Input)
	case *fantasy.ToolCallPart:
		return estimateToolCallTokens(p.ToolName, p.Input)
	case fantasy.ToolResultPart:
		return estimateToolResultContentTokens(p.ToolCallID, "", p.Output)
	case *fantasy.ToolResultPart:
		return estimateToolResultContentTokens(p.ToolCallID, "", p.Output)
	default:
		return 0
	}
}

func estimateToolCallTokens(toolName, input string) int64 {
	return approxTokenCount(toolName) + approxTokenCount(input)
}

// estimateToolResultContentTokens leaves a result's client metadata out: it
// is kept for the UI and is not part of any request (see
// toAIMessage, whose tool results carry the output only).
// Counting it doubled the estimate for a file read, whose metadata holds a
// second copy of the content.
func estimateToolResultContentTokens(toolCallID, toolName string, output fantasy.ToolResultOutputContent) int64 {
	tokens := approxTokenCount(toolCallID) + approxTokenCount(toolName)
	switch result := output.(type) {
	case fantasy.ToolResultOutputContentText:
		tokens += approxTokenCount(result.Text)
	case *fantasy.ToolResultOutputContentText:
		tokens += approxTokenCount(result.Text)
	case fantasy.ToolResultOutputContentError:
		if result.Error != nil {
			tokens += approxTokenCount(result.Error.Error())
		}
	case *fantasy.ToolResultOutputContentError:
		if result.Error != nil {
			tokens += approxTokenCount(result.Error.Error())
		}
	case fantasy.ToolResultOutputContentMedia:
		tokens += estimateMediaTokens(result.MediaType, result.Text, len(result.Data))
	case *fantasy.ToolResultOutputContentMedia:
		tokens += estimateMediaTokens(result.MediaType, result.Text, len(result.Data))
	}
	return tokens
}

func estimateFilePartTokens(file fantasy.FilePart) int64 {
	return estimateMediaTokens(file.MediaType, file.Filename, len(file.Data))
}

func estimateGeneratedFileTokens(file fantasy.FileContent) int64 {
	return estimateMediaTokens(file.MediaType, "", len(file.Data))
}

func estimateMediaTokens(mediaType, text string, dataBytes int) int64 {
	if dataBytes == 0 {
		return approxTokenCount(mediaType) + approxTokenCount(text)
	}
	return approxTokenCount(fmt.Sprintf("%s %s %d bytes", mediaType, text, dataBytes))
}

func estimateSourceTokens(source fantasy.SourceContent) int64 {
	return approxTokenCount(string(source.SourceType)) +
		approxTokenCount(source.ID) +
		approxTokenCount(source.URL) +
		approxTokenCount(source.Title) +
		approxTokenCount(source.MediaType) +
		approxTokenCount(source.Filename)
}

func approxTokenCount(s string) int64 {
	if s == "" {
		return 0
	}
	return int64((len(s) + 3) / 4)
}
