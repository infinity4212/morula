package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Route 是插件模型走的本地网关。
type Route struct {
	BaseURL string // 例如 http://127.0.0.1:53211/v1
	Key     string // 网关 key，用环境变量交给 harness
	Context int    // 模型的上下文窗口，未知时为 0
}

// Plugins 由 gui 启动时设置，接上插件提供的模型（写作 <provider>/<model>）。
// 两个函数都为 nil 时 harness 只用自己的模型。harness 包不直接依赖插件和网关。
var Plugins struct {
	Models func() []Model
	Route  func(ctx context.Context, model string) (Route, bool)
	// Off 判断用户在「模型来源」里有没有取消勾选这个模型。来源 codex 指 codex 订阅自己的模型。
	Off func(source, id string) bool
}

// chatgptVia 是 codex 登录的 ChatGPT 订阅在网关里的 provider id（plugin.ChatGPTID），opencode、pi 经网关用它。
const chatgptVia = "codex"

func pluginModels() []Model {
	if Plugins.Models == nil {
		return nil
	}
	return Plugins.Models()
}

// codexRoute 给插件模型拼 codex 参数：自定义一个 Responses 接口的 provider 指向网关。
// 不是插件模型时 ok 为假。
func codexRoute(ctx context.Context, model string) (args, env []string, ok bool) {
	if Plugins.Route == nil || model == "" {
		return nil, nil, false
	}
	r, ok := Plugins.Route(ctx, model)
	if !ok {
		return nil, nil, false
	}
	args = []string{
		"-c", `model_providers.morula={name="morula",base_url="` + r.BaseURL + `",wire_api="responses",env_key="MORULA_GATEWAY_KEY"}`,
		"-c", `model_provider="morula"`,
	}
	if r.Context > 0 {
		args = append(args, "-c", "model_context_window="+strconv.Itoa(r.Context))
	}
	return args, []string{"MORULA_GATEWAY_KEY=" + r.Key}, true
}

// gwPrefix 是 opencode、pi 里网关模型的 id 前缀。opencode、pi 自己的模型 id 也写作 provider/model，
// 加上前缀才不会和插件的 provider 撞名。codex 自己的模型 id 不带「/」，不加前缀。
const gwPrefix = "morula/"

// gatewayModels 是给 opencode、pi 列的网关模型：id 前面加 morula/。
func gatewayModels() []Model {
	out := []Model{}
	for _, m := range pluginModels() {
		m.ID = gwPrefix + m.ID
		out = append(out, m)
	}
	return out
}

// gatewayRoute 认出带 morula/ 前缀的模型，返回去掉前缀的模型 id 和网关地址。不是网关模型时 ok 为假。
func gatewayRoute(ctx context.Context, model string) (inner string, r Route, ok bool) {
	inner, ok = strings.CutPrefix(model, gwPrefix)
	if !ok || Plugins.Route == nil {
		return "", Route{}, false
	}
	r, ok = Plugins.Route(ctx, inner)
	return inner, r, ok
}

// gatewayModel 在插件模型里找 id，找不到时只填 id。
func gatewayModel(id string) Model {
	for _, m := range pluginModels() {
		if m.ID == id {
			return m
		}
	}
	return Model{ID: id, Name: id}
}

// opencodeProvider 是给这一次调用加的 morula provider，指向网关，只列这一个模型。写进 OPENCODE_CONFIG_CONTENT 的 provider 段，
// 不写 ~/.config/opencode。
func opencodeProvider(inner string, r Route) map[string]any {
	m := gatewayModel(inner)
	md := map[string]any{"name": m.Name}
	if r.Context > 0 {
		md["limit"] = map[string]int{"context": r.Context, "output": min(32000, r.Context/4)}
	}
	if len(m.Efforts) > 0 {
		v := map[string]any{}
		for _, e := range m.Efforts {
			v[e] = map[string]string{"reasoningEffort": e}
		}
		md["variants"] = v
	}
	return map[string]any{"morula": map[string]any{
		"npm":     "@ai-sdk/openai-compatible",
		"name":    "morula",
		"options": map[string]string{"baseURL": r.BaseURL, "apiKey": "{env:MORULA_GATEWAY_KEY}"},
		"models":  map[string]any{inner: md},
	}}
}

// piExtension 是给 pi 加载的扩展：从环境变量读网关地址和模型，注册一个 morula provider。
// 不写 ~/.pi/agent/models.json。
const piExtension = `export default function (pi) {
  const m = JSON.parse(process.env.MORULA_GATEWAY_MODEL || "{}");
  pi.registerProvider("morula", {
    baseUrl: process.env.MORULA_GATEWAY_URL,
    api: "openai-completions",
    apiKey: "$MORULA_GATEWAY_KEY",
    models: [{
      id: m.id, name: m.name || m.id, reasoning: !!m.reasoning, input: ["text"],
      contextWindow: m.context || 128000, maxTokens: m.output || 32000,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    }],
  });
}
`

// piTools 是给 pi 加载的扩展：从环境变量读 morula 的 MCP 服务器，只对这次调用注册。
// exposure 用 direct，工具直接声明给模型；默认的 codemode 要模型写脚本才能调。
const piTools = `export default function (pi) {
  const t = JSON.parse(process.env.MORULA_TOOLS || "{}");
  if (!t.url && !t.command) return;
  const c = t.url ? { url: t.url } : { command: t.command, args: t.args || [], env: t.env || {} };
  pi.registerMcpServer("morula_assistant", { ...c, exposure: "direct", description: "morula 给的工具", ...(t.timeout ? { timeout: t.timeout } : {}) });
}
`

// piToolArgs 写出工具扩展，返回 pi 的参数和环境变量。
func piToolArgs(t *Tools) (args, env []string, err error) {
	p, err := piFile("pi-tools.js", piTools)
	if err != nil {
		return nil, nil, err
	}
	b, _ := json.Marshal(map[string]any{"url": t.URL, "command": t.Command, "args": t.Args, "env": t.Env, "timeout": t.Timeout})
	return []string{"-e", p}, []string{"MORULA_TOOLS=" + string(b)}, nil
}

// piFile 把扩展写到 <用户缓存目录>/morula/<name>，内容没变时不重写。
func piFile(name, content string) (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	dir = filepath.Join(dir, "morula")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if b, _ := os.ReadFile(p); string(b) != content {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return p, nil
}

// piGateway 写出扩展文件，返回 pi 的参数和环境变量。
func piGateway(inner string, r Route) (args, env []string, err error) {
	p, err := piFile("pi-gateway.js", piExtension)
	if err != nil {
		return nil, nil, err
	}
	m := gatewayModel(inner)
	ctxw := r.Context
	b, _ := json.Marshal(map[string]any{"id": inner, "name": m.Name, "reasoning": len(m.Efforts) > 0,
		"context": ctxw, "output": min(32000, max(ctxw/4, 4096))})
	args = []string{"-e", p, "--provider", "morula", "--model", inner}
	env = []string{"MORULA_GATEWAY_URL=" + r.BaseURL, "MORULA_GATEWAY_KEY=" + r.Key, "MORULA_GATEWAY_MODEL=" + string(b)}
	return args, env, nil
}
