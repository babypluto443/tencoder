// 来源：公众号@小林coding
// 后端八股网站：xiaolincoding.com
// Agent网站：xiaolinnote.com
// 简历模版：jianli.xiaolinnote.com

package tui

import (
	"testing"

	"tencoder/internal/config"
)

func TestClearResetsSession(t *testing.T) {
	provider := config.ProviderConfig{
		Name:     "test",
		Protocol: "anthropic",
		Model:    "claude-sonnet-4-20250514",
		APIKey:   "test-key",
	}
	m := New([]config.ProviderConfig{provider}, nil, nil)

	// 模拟已有一些状态
	m.chatMessages = []chatMessage{
		{role: "user", content: "hello"},
		{role: "assistant", content: "hi"},
	}
	m.committedUpTo = 2
	m.totalInput = 1000
	m.totalOutput = 500
	oldSessionID := m.sessionID

	// 执行 /clear
	updated, _ := m.executeCommand("clear", "")
	m = updated.(Model)

	// 验证消息已清空
	if len(m.chatMessages) != 0 {
		t.Errorf("chatMessages should be empty after /clear, got %d", len(m.chatMessages))
	}

	// 验证 committedUpTo 归零
	if m.committedUpTo != 0 {
		t.Errorf("committedUpTo should be 0 after /clear, got %d", m.committedUpTo)
	}

	// 验证 session ID 已更新（不等于旧值）
	if m.sessionID == oldSessionID {
		t.Errorf("sessionID should change after /clear, still %s", m.sessionID)
	}
	if m.sessionID == "" {
		t.Error("sessionID should not be empty after /clear")
	}

	// 验证 token 计数归零
	if m.totalInput != 0 {
		t.Errorf("totalInput should be 0 after /clear, got %d", m.totalInput)
	}
	if m.totalOutput != 0 {
		t.Errorf("totalOutput should be 0 after /clear, got %d", m.totalOutput)
	}

	// 验证 conversation 已重建
	if m.conversation == nil {
		t.Error("conversation should not be nil after /clear")
	}
}
