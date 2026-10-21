package harness

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestGatewayRoute(t *testing.T) {
	old := Plugins
	defer func() { Plugins = old }()
	Plugins.Models = func() []Model {
		return []Model{{ID: "demo/demo-pro", Name: "演示 · demo-pro", Efforts: []string{"low", "high"}, Via: "demo"}}
	}
	Plugins.Route = func(_ context.Context, m string) (Route, bool) {
		return Route{BaseURL: "http://127.0.0.1:1/v1", Key: "k", Context: 200000}, m == "demo/demo-pro"
	}
	if ms := gatewayModels(); len(ms) != 1 || ms[0].ID != "morula/demo/demo-pro" || ms[0].Via != "demo" {
		t.Fatalf("gatewayModels = %+v", ms)
	}
	// opencode、pi 自己的模型不走网关，哪怕 provider 和插件撞名。
	for _, m := range []string{"demo/demo-pro", "opencode/big-pickle", "morula/other/x"} {
		if _, _, ok := gatewayRoute(context.Background(), m); ok {
			t.Errorf("%s 不该走网关", m)
		}
	}
	inner, r, ok := gatewayRoute(context.Background(), "morula/demo/demo-pro")
	if !ok || inner != "demo/demo-pro" {
		t.Fatalf("gatewayRoute = %q %v", inner, ok)
	}

	model, env := opencodeEnv(context.Background(), Spec{Model: "morula/demo/demo-pro", Tools: &Tools{URL: "http://127.0.0.1:1/mcp", Timeout: 60}})
	cfg, _ := strings.CutPrefix(env[1], "OPENCODE_CONFIG_CONTENT=")
	var c struct {
		MCP map[string]struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Timeout int    `json:"timeout"`
		} `json:"mcp"`
		Provider map[string]struct {
			NPM     string            `json:"npm"`
			Options map[string]string `json:"options"`
			Models  map[string]struct {
				Limit    map[string]int             `json:"limit"`
				Variants map[string]json.RawMessage `json:"variants"`
			} `json:"models"`
		} `json:"provider"`
	}
	if err := json.Unmarshal([]byte(cfg), &c); err != nil {
		t.Fatal(err)
	}
	p := c.Provider["morula"]
	md := p.Models["demo/demo-pro"]
	if p.NPM != "@ai-sdk/openai-compatible" || p.Options["baseURL"] != r.BaseURL || p.Options["apiKey"] != "{env:MORULA_GATEWAY_KEY}" ||
		md.Limit["context"] != 200000 || len(md.Variants) != 2 || env[0] != "MORULA_GATEWAY_KEY=k" || model != "morula/demo/demo-pro" {
		t.Fatalf("opencode 配置不对：%s %v", cfg, env)
	}
	if m := c.MCP["morula_assistant"]; m.Type != "remote" || m.URL != "http://127.0.0.1:1/mcp" || m.Timeout != 60000 {
		t.Fatalf("opencode 的 MCP 配置不对：%s", cfg)
	}

	t.Setenv("HOME", t.TempDir())
	args, penv, err := piGateway(inner, r)
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-e" || !slices.Equal(args[2:], []string{"--provider", "morula", "--model", "demo/demo-pro"}) {
		t.Fatalf("pi 参数 = %v", args)
	}
	if b, _ := os.ReadFile(args[1]); string(b) != piExtension {
		t.Fatal("pi 扩展文件没写对")
	}
	if !slices.Contains(penv, "MORULA_GATEWAY_URL="+r.BaseURL) || !slices.ContainsFunc(penv, func(s string) bool { return strings.HasPrefix(s, `MORULA_GATEWAY_MODEL={"context":200000`) }) {
		t.Fatalf("pi 环境变量 = %v", penv)
	}
}
