package work

import (
	"strings"
	"testing"

	"morula/internal/store"
)

// TestDescribePrompt 检查写介绍的提示词带上职责、常用项目和 skill 的说明，本机没有的 skill 不写进去。
func TestDescribePrompt(t *testing.T) {
	e := staffEngine(t)
	p, _ := e.DB.AddProject("官网", "", "")
	a := &store.Agent{Name: "后端", Role: "执行", Instructions: "写服务端", Skills: []string{"go-dev", "没有的"}, Projects: []int64{p.ID}}
	got := e.describePrompt(a)
	for _, want := range []string{"Agent「后端」", "职责说明：写服务端", "常用于项目：官网", "- go-dev：写 Go", "不超过 60 个字"} {
		if !strings.Contains(got, want) {
			t.Fatalf("提示词缺「%s」：\n%s", want, got)
		}
	}
	if strings.Contains(got, "没有的") {
		t.Fatalf("本机没有的 skill 不应写进去：\n%s", got)
	}
	if busy, why := e.Describing("后端"); busy || why != "" {
		t.Fatalf("没开始写时应为空：%v %q", busy, why)
	}
}
