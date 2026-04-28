package claude

type Event interface{ isClaudeEvent() }

type InitEvent struct {
	SessionID string
}

type AssistantTextEvent struct {
	Text string
}

type ToolUseEvent struct {
	Name string
}

// ResultEvent always fires last in the events channel. On crash, cancel,
// or unexpected exit, the runner synthesises one with Error set.
type ResultEvent struct {
	CostUSD    float64
	DurationMS int64
	Error      error
}

func (InitEvent) isClaudeEvent()          {}
func (AssistantTextEvent) isClaudeEvent() {}
func (ToolUseEvent) isClaudeEvent()       {}
func (ResultEvent) isClaudeEvent()        {}
