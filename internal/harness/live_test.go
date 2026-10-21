package harness_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morula/internal/gateway"
	"morula/internal/harness"
	"morula/internal/plugin"
)

// TestLiveCodexPlugin 让 codex 用插件模型在临时目录里写一个文件。要已登录的插件模型，默认跳过：
//
//	MORULA_HOME=<数据目录> MORULA_LIVE_MODEL=workbuddy/glm-5.3 go test ./internal/harness -run Live -v
func TestLiveCodexPlugin(t *testing.T) {
	model := os.Getenv("MORULA_LIVE_MODEL")
	if model == "" {
		t.Skip("没设 MORULA_LIVE_MODEL")
	}
	g, err := gateway.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	harness.Plugins.Route = func(ctx context.Context, m string) (harness.Route, bool) {
		ref, ok := plugin.Find(ctx, m)
		return harness.Route{BaseURL: g.BaseURL(), Key: g.Key, Context: ref.Model.Context}, ok
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	a, _ := harness.Get("codex")
	res, err := a.Run(ctx, harness.Spec{
		Dir: dir, Model: model, Access: harness.Workspace, Ephemeral: true,
		Prompt: "在当前目录新建 hello.txt，内容只有一行：morula via plugin。写完后回复 done。",
	}, func(e harness.Event) { t.Logf("%s: %s", e.Kind, e.Text) })
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("没写出文件：%v；最终回复 %q", err, res.Final)
	}
	if !strings.Contains(string(b), "morula via plugin") {
		t.Fatalf("文件内容 %q", b)
	}
	t.Logf("最终回复 %q，tokens %d", res.Final, res.Tokens)
}

// TestLiveHarness 用 opencode 或 pi 自己的模型在临时目录里写一个文件，按 JSON Schema 回复，再读一遍轨迹。
// 会发出一次真实的模型调用，默认跳过：
//
//	MORULA_LIVE_HARNESS=pi MORULA_LIVE_HARNESS_MODEL=<provider/model> go test ./internal/harness -run LiveHarness -v
func TestLiveHarness(t *testing.T) {
	rt, model := os.Getenv("MORULA_LIVE_HARNESS"), os.Getenv("MORULA_LIVE_HARNESS_MODEL")
	if rt == "" {
		t.Skip("没设 MORULA_LIVE_HARNESS")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var out struct {
		File string `json:"file"`
	}
	res, err := harness.Ask(ctx, rt, harness.Spec{
		Dir: dir, Model: model, Effort: os.Getenv("MORULA_LIVE_HARNESS_EFFORT"), Access: harness.Workspace,
		Schema: `{"type":"object","properties":{"file":{"type":"string"}},"required":["file"],"additionalProperties":false}`,
		Prompt: "在当前目录新建 hello.txt，内容只有一行：morula via " + rt + "。写完后回复文件名。",
	}, &out, func(e harness.Event) { t.Logf("%s: %s %s", e.Kind, e.Tool, e.Text) })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("会话 %s，token %d，回复 %q", res.Session, res.Tokens, res.Final)
	b, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || !strings.Contains(string(b), "morula via "+rt) {
		t.Fatalf("hello.txt：%q %v", b, err)
	}
	if out.File != "hello.txt" {
		t.Errorf("回复里的文件名 %q", out.File)
	}
	a, _ := harness.Get(rt)
	steps, err := a.Trajectory(res.Session)
	if err != nil || len(steps) == 0 {
		t.Fatalf("轨迹：%d 步，%v", len(steps), err)
	}
	t.Logf("轨迹 %d 步，第一步 %s", len(steps), steps[0].Role)
}

// TestLiveGatewayHarness 让 opencode、pi 经网关调一个 API key 来源，验证网关模型能接到 codex 以外的 harness。
// 来源指向一个本机的假 OpenAI 兼容服务，不发真实模型调用，默认跳过：
//
//	MORULA_GW_FAKE=http://127.0.0.1:<端口>/v1 go test ./internal/harness -run LiveGateway -v
func TestLiveGatewayHarness(t *testing.T) {
	base := os.Getenv("MORULA_GW_FAKE")
	if base == "" {
		t.Skip("没设 MORULA_GW_FAKE")
	}
	t.Setenv("MORULA_HOME", t.TempDir())
	ctx := context.Background()
	if _, err := plugin.AddAPIKey(ctx, plugin.APIKey{ID: "fake", Name: "假来源", BaseURL: base, Key: "sk-fake"}); err != nil {
		t.Fatal(err)
	}
	g, err := gateway.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	old := harness.Plugins
	defer func() { harness.Plugins = old }()
	harness.Plugins.Models = func() []harness.Model {
		return []harness.Model{{ID: "fake/fake-1", Name: "假来源 · fake-1", Efforts: []string{}, Via: "fake"}}
	}
	harness.Plugins.Route = func(ctx context.Context, m string) (harness.Route, bool) {
		ref, ok := plugin.Find(ctx, m)
		return harness.Route{BaseURL: g.BaseURL(), Key: g.Key, Context: ref.Model.Context}, ok
	}
	for _, rt := range []string{"opencode", "pi"} {
		t.Run(rt, func(t *testing.T) {
			a, err := harness.Get(rt)
			if err != nil || a.Detect() != nil {
				t.Skip("本机没有 " + rt)
			}
			found := false
			for _, m := range a.Models() {
				found = found || m.ID == "morula/fake/fake-1"
			}
			if !found {
				t.Fatal("模型列表里没有 morula/fake/fake-1")
			}
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			res, err := a.Run(ctx, harness.Spec{Dir: t.TempDir(), Model: "morula/fake/fake-1", Access: harness.ReadOnly, Ephemeral: true, Prompt: "回复一句话"},
				func(e harness.Event) { t.Logf("%s: %s", e.Kind, e.Text) })
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Final, "网关通了") {
				t.Fatalf("回复 = %q", res.Final)
			}
		})
	}
}
