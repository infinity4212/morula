// Package mcpserver 是 `morula mcp`：一个 stdio MCP 服务器，
// 让外部的 agent 工具（例如终端里的 codex）读和改 morula 的 Agent 花名册。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"morula/internal/journal"
	"morula/internal/store"
)

type dayInput struct {
	Day   string `json:"day" jsonschema:"日期，格式 2026-10-12（凌晨 4 点换日）"`
	Level string `json:"level,omitempty" jsonschema:"summary：只看总结（默认）；full：完整记录"`
}

type searchInput struct {
	Query string `json:"query" jsonschema:"要找的词，不分大小写"`
	Limit int    `json:"limit,omitempty" jsonschema:"最多返回几条，默认 20"`
}

type memoryInput struct {
	Content string `json:"content" jsonschema:"长期记忆的完整新内容（Markdown），会整体替换旧内容"`
}

type noInput struct{}

type hireInput struct {
	Name         string `json:"name" jsonschema:"Agent 名称，唯一，例如 审阅-登录"`
	Role         string `json:"role,omitempty" jsonschema:"角色：规划、审阅或执行，默认执行"`
	Runtime      string `json:"runtime" jsonschema:"运行时：codex、opencode、pi、hermes、workbuddy 之一"`
	Model        string `json:"model" jsonschema:"模型名，例如 opus、sonnet、gpt-6-luna、deepseek-v4-pro"`
	Effort       string `json:"effort,omitempty" jsonschema:"推理强度：low、medium、high，可留空"`
	Instructions string `json:"instructions,omitempty" jsonschema:"这个 Agent 的职责说明，可留空"`
}

type nameInput struct {
	Name string `json:"name" jsonschema:"Agent 名称"`
}

type updateInput struct {
	Name         string  `json:"name" jsonschema:"要修改的 Agent 名称"`
	Role         *string `json:"role,omitempty" jsonschema:"新角色"`
	Runtime      *string `json:"runtime,omitempty" jsonschema:"新运行时"`
	Model        *string `json:"model,omitempty" jsonschema:"新模型"`
	Effort       *string `json:"effort,omitempty" jsonschema:"新推理强度"`
	Instructions *string `json:"instructions,omitempty" jsonschema:"新的职责说明"`
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func failed(err error) *mcp.CallToolResult {
	r := text(err.Error())
	r.IsError = true
	return r
}

func listing(st *store.Store) *mcp.CallToolResult {
	list, err := st.Agents()
	if err != nil {
		return failed(err)
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	return text(string(b))
}

// Run 在 stdin/stdout 上提供 MCP 服务，直到对端断开。
func Run(ctx context.Context, version string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	s := mcp.NewServer(&mcp.Implementation{Name: "morula", Version: version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_agents",
		Description: "列出用户 morula 团队里的全部 Agent：名称、角色、运行时、模型、推理强度、职责、状态。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		return listing(st), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "hire_agent",
		Description: "在 morula 团队里新建一个 Agent。成功后返回最新的花名册。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in hireInput) (*mcp.CallToolResult, any, error) {
		err := st.AddAgent(store.Agent{
			Name: in.Name, Role: in.Role, Runtime: in.Runtime, Model: in.Model,
			Effort: in.Effort, Instructions: in.Instructions,
		})
		if err != nil {
			return failed(err), nil, nil
		}
		return listing(st), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "update_agent",
		Description: "修改一个已有 Agent 的角色、运行时、模型、推理强度或职责说明。只传要改的字段。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in updateInput) (*mcp.CallToolResult, any, error) {
		err := st.UpdateAgent(in.Name, store.Patch{
			Role: in.Role, Runtime: in.Runtime, Model: in.Model, Effort: in.Effort, Instructions: in.Instructions,
		})
		if err != nil {
			return failed(err), nil, nil
		}
		return listing(st), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "fire_agent",
		Description: "从 morula 团队里删除一个 Agent。删除前先和用户确认。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, any, error) {
		if err := st.RemoveAgent(in.Name); err != nil {
			return failed(err), nil, nil
		}
		return listing(st), nil, nil
	})

	// 记录和记忆：渐进式查看，先 list_days，再看某天总结，需要细节才看完整记录。
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_days",
		Description: "列出和用户有过对话的日期（凌晨 4 点换日），每天附总结的第一行和消息数。查以前的事先用它。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		var b strings.Builder
		for _, d := range journal.Days() {
			evs, _ := journal.Load(d)
			n := 0
			for _, e := range evs {
				if e["type"] == "user" {
					n++
				}
			}
			first := "（还没有总结）"
			if sum, ok := journal.Summary(d); ok {
				first = strings.SplitN(strings.TrimSpace(sum), "\n", 2)[0]
			}
			fmt.Fprintf(&b, "%s  用户消息 %d 条  %s\n", d, n, first)
		}
		if b.Len() == 0 {
			return text("还没有任何记录。"), nil, nil
		}
		return text(b.String()), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_day",
		Description: "读某一天：level=summary 看助理写的当天总结；level=full 看当天完整记录（用户原话、助理回复、工具调用）。先看总结，不够再看完整记录。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dayInput) (*mcp.CallToolResult, any, error) {
		if in.Level == "full" {
			t, err := journal.Transcript(in.Day, 60000)
			if err != nil {
				return failed(err), nil, nil
			}
			if t == "" {
				return text(in.Day + " 没有记录。"), nil, nil
			}
			return text(t), nil, nil
		}
		if sum, ok := journal.Summary(in.Day); ok {
			return text(sum), nil, nil
		}
		return text(in.Day + " 还没有总结（可能就是今天）。要细节用 level=full。"), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "search_history",
		Description: "在全部历史记录里按关键词找，从新到旧返回命中的行和日期。找到后可用 read_day 看那一天。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
		if in.Limit <= 0 {
			in.Limit = 20
		}
		hits := journal.Search(in.Query, in.Limit)
		if len(hits) == 0 {
			return text("没找到「" + in.Query + "」。"), nil, nil
		}
		b, _ := json.MarshalIndent(hits, "", "  ")
		return text(string(b)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "read_memory",
		Description: "读你自己维护的长期记忆（用户偏好、团队约定、长期项目等）。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		m := journal.Memory()
		if strings.TrimSpace(m) == "" {
			return text("长期记忆还是空的。"), nil, nil
		}
		return text(m), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "write_memory",
		Description: "整体改写长期记忆。先 read_memory，在旧内容基础上增删后把完整内容传进来。只记以后还用得上的事实和偏好，过时或错的要删掉。",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in memoryInput) (*mcp.CallToolResult, any, error) {
		if err := journal.SaveMemory(in.Content); err != nil {
			return failed(err), nil, nil
		}
		return text("已保存长期记忆，共 " + fmt.Sprint(len([]rune(in.Content))) + " 字。"), nil, nil
	})

	return s.Run(ctx, &mcp.StdioTransport{})
}
