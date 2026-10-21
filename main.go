// morula：一个 agent 团队的桌面工作台。
//
// 子命令：
//
//	morula           打开桌面窗口（默认）
//	morula web       只起 HTTP 服务，不开窗口
//	morula mcp       stdio MCP 服务器，供 Claude Code 管理 Agent 花名册
//	morula mcp --assistant  只读的 MCP 服务器，morula 判断归属时挂给助理，查项目和执行者名单
//	morula version   打印版本
package main

import (
	"context"
	"fmt"
	"os"

	"morula/internal/env"
	"morula/internal/gui"
	"morula/internal/mcpserver"
)

// version 由 Makefile 通过 -ldflags 写入。
var version = "0.0.0"

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "mcp":
		// 由 claude 以子进程方式启动，stdout 只能输出 MCP 协议。
		if len(os.Args) > 2 && os.Args[2] == "--assistant" {
			err = mcpserver.RunAssistant(context.Background(), version)
		} else {
			err = mcpserver.Run(context.Background(), version)
		}
	case "version":
		fmt.Println("morula", version)
		return
	case "web":
		addr := "127.0.0.1:7788"
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		env.FixPath()
		err = gui.Serve(version, addr)
	default:
		env.FixPath()
		err = gui.Run(version)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
