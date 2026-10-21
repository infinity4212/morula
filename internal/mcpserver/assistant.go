package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"morula/internal/store"
	"morula/internal/work"
)

// RunAssistant 是 `morula mcp --assistant`：morula 在判断归属时挂给助理的只读工具。
// 只给名字和介绍；项目记忆、执行者的详细说明等选定项目、要派活时由 morula 直接写进提示词。
func RunAssistant(ctx context.Context, version string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()

	s := mcp.NewServer(&mcp.Implementation{Name: "morula-assistant", Version: version}, nil)
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_projects",
		Description: "列出用户的全部项目：编号、名字和项目摘要。判断一条消息属于哪个项目前先调用它。",
		Annotations: ro,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		return text(work.ProjectList(st)), nil, nil
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_agents",
		Description: "列出团队里的全部 agent：名字、角色和一句话介绍。固定成员（你自己和验收）各有固定的活，单列在前面；派活时从后面的子 agent 里选。用户问团队里有谁、谁能做某件事时调用它。",
		Annotations: ro,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
		return text(work.AgentList(st)), nil, nil
	})
	return s.Run(ctx, &mcp.StdioTransport{})
}
