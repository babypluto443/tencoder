// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package skills

import (
	"context"
	"fmt"

	"tencoder/internal/tools"
)

// LoadSkillTool is the on-demand activation entry point. It's registered
// into the main tool registry at startup with progressive-disclosure
// semantics: the model sees a `## Available Skills` listing of every
// skill's name + description in the system prompt, and calls LoadSkill
// with the chosen name. The full SOP body is returned as the tool result
// so it enters the conversation as a regular message.
type LoadSkillTool struct {
	Catalog *Catalog
	Host    SkillHost
}

func (t *LoadSkillTool) Name() string                 { return "LoadSkill" }

func (t *LoadSkillTool) Category() tools.ToolCategory { return tools.CategoryRead }

func (t *LoadSkillTool) IsSystemTool() bool           { return true }

func (t *LoadSkillTool) Description() string {
	return "Activate a Skill by name. Returns the full SOP body so you can follow its " +
		"instructions. Call this when the user's request matches one of the available " +
		"Skills listed in the system prompt. Pass the Skill name without a leading slash."
}

func (t *LoadSkillTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "The Skill name to activate (e.g. \"commit\", \"backend-interview\").",
				},
			},
			"required": []string{"name"},
		},
	}
}

func (t *LoadSkillTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	name, _ := args["name"].(string)
	if name == "" {
		return tools.ToolResult{Output: "name is required", IsError: true}
	}
	if t.Catalog == nil || t.Host == nil {
		return tools.ToolResult{Output: "LoadSkill not wired (Catalog or Host nil)", IsError: true}
	}
	skill, err := t.Catalog.GetFull(name)
	if err != nil && skill == nil {
		return tools.ToolResult{Output: fmt.Sprintf("unknown skill: %s", name), IsError: true}
	}
	if skill.PromptBody == "" {
		return tools.ToolResult{Output: fmt.Sprintf("skill %q has empty body — cannot activate", name), IsError: true}
	}

	t.Host.ActivateSkill(skill.Meta.Name, skill.PromptBody)

	header := fmt.Sprintf("# Skill: %s\n\n", skill.Meta.Name)
	return tools.ToolResult{
		Output: header + skill.PromptBody,
	}
}
