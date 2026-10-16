// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com


package teams

// CoordinatorMode restricts the Lead agent's tools to coordination-only.
// When active, Lead can only use: Agent, TaskStop, SendMessage, and
// task management tools (TaskCreate, TaskGet, TaskList, TaskUpdate).
//
// The four-phase workflow:
// 1. Research: Lead explores the problem space
// 2. Synthesis: Lead creates a plan and task decomposition
// 3. Implementation: Lead spawns teammates to execute tasks
// 4. Verification: Lead verifies results and resolves conflicts

var CoordinatorAllowedTools = map[string]bool{
	"Agent":       true,
	"SendMessage": true,
	"TaskCreate":  true,
	"TaskGet":     true,
	"TaskList":    true,
	"TaskUpdate":  true,

	"TeamCreate":  true,
	"TeamDelete":  true,
	"ReadFile":    true,
	"Glob":        true,
	"Grep":        true,
	"Bash":        true,
}

// IsCoordinatorTool checks if a tool is allowed in Coordinator Mode.
func IsCoordinatorTool(name string) bool {
	return CoordinatorAllowedTools[name]
}

// CoordinatorToolFilter 返回 Lead Agent 的工具过滤函数。
// enabled 为 false 时返回 nil（不限制），为 true 时在有 Team 存在时限制工具集。
func CoordinatorToolFilter(teamMgr *TeamManager, enabled bool) func(name string) bool {
	if teamMgr == nil || !enabled {
		return nil
	}
	return func(name string) bool {
		if len(teamMgr.ListTeams()) == 0 {
			return true
		}
		return IsCoordinatorTool(name)
	}
}

