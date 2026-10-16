// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package agent

import (
	"time"

	"tencoder/internal/permissions"
)

type AgentEvent interface{ agentEvent() }

type StreamText struct{ Text string }
type ThinkingText struct{ Text string }
type ToolUseEvent struct {
	ToolID   string
	ToolName string
	Args     map[string]any
}
type ToolResultEvent struct {
	ToolID   string
	ToolName string
	Output   string
	IsError  bool
	Elapsed  time.Duration
}
type TurnComplete struct{ Turn int }
type LoopComplete struct{ TotalTurns int }
type UsageEvent struct{ InputTokens, OutputTokens int }
type ErrorEvent struct{ Message string }
type CompactEvent struct{ Message string }
type RetryEvent struct {
	Reason string
	Wait   time.Duration
}

// ToolEvictionEvent reports that a batch of deferred tool schemas was returned to the deferred
// pool (阶段三 任务 3.1). Surfaced rather than swallowed because it changes the tools parameter of
// every subsequent request: without an event the model would just see fewer tools and could not
// tell "evicted, reloadable via ToolSearch" apart from "never existed".
type ToolEvictionEvent struct {
	Evicted       int
	Remaining     int
	SkippedActive int
	Names         []string
}

type PermissionResponse int

const (
	PermAllow PermissionResponse = iota
	PermDeny
	PermAllowAlways
)

type PermissionRequestEvent struct {
	ToolName   string
	Desc       string
	ResponseCh chan<- PermissionResponse
	// Decision carries the permission layer's verdict so the UI can adapt to it (阶段四 4.2:
	// a read-only-workspace write must not offer "always allow"). Nil when the caller has no
	// checker / no decision to report — the UI then shows the default options.
	Decision *permissions.Decision
	// Target is the file the prompt concerns, for the audit record of the user's answer.
	Target string
}

func (StreamText) agentEvent() {}

func (ThinkingText) agentEvent() {}

func (ToolUseEvent) agentEvent()    {}
func (ToolResultEvent) agentEvent() {}

func (TurnComplete) agentEvent() {}

func (LoopComplete) agentEvent() {}
func (UsageEvent) agentEvent()   {}
func (ErrorEvent) agentEvent()   {}
func (CompactEvent) agentEvent() {}

func (RetryEvent) agentEvent()             {}
func (ToolEvictionEvent) agentEvent()      {}
func (PermissionRequestEvent) agentEvent() {}

type AskUserQuestionEvent struct {
	Questions  []map[string]any
	ResponseCh chan map[string]string
}

func (AskUserQuestionEvent) agentEvent() {}
