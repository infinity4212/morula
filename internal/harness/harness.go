// Package harness 是 agent 公式里的 harness 一层：agent = 模型 + harness + skill。
//
// 每个 harness 一个适配器，说明怎么检测、怎么非交互运行一次、怎么只对这一次调用指定模型、
// 怎么拿会话 id、怎么续跑、怎么读出 harness 自己保存的轨迹。
// 不改 harness 的全局配置：并发的子 agent 可能在同一个 harness 上用不同模型。
//
// 轨迹留在 harness 自己那里，morula 只记会话 id。
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Access 是这次运行能做什么。
type Access int

const (
	// ReadOnly 只读：助理路由、规划、检查用。
	ReadOnly Access = iota
	// Workspace 可写工作目录，越过沙箱的请求交给 harness 自带的自动审查（审批默认 auto）。
	Workspace
	// Full 不限文件和联网，用户选了全部放行时派活用。只有 codex 有沙箱，其他 harness 和 Workspace 一样。
	Full
)

// Spec 是一次运行的输入。
type Spec struct {
	Dir       string // 工作目录
	Model     string // 只对这次调用生效
	Effort    string
	Prompt    string
	Schema    string // 最终回复的 JSON Schema；空表示自由文本
	Resume    string // 非空时续跑这个会话
	Access    Access
	Ephemeral bool     // 不留会话文件（路由、检查这类一次性调用）
	Images    []string // 随提示词附上的图片路径。只有 codex 会把图片交给模型，其他 harness 忽略
	Tools     *Tools   // 这次运行能调用的 MCP 工具；只有 UsesTools 返回 true 的 harness 会接上
	CompactAt int      // 会话的上下文到多少 token 时由 harness 自己压缩；0 用 harness 的设置。只有 Compacts 返回 true 的 harness 照办
}

// Tools 是挂给这次运行的一个 MCP 服务器，只开放 Names 里的工具。
// URL 非空时连 morula 进程里的 streamable HTTP 服务，否则由 harness 按 Command 起一个 stdio 服务器。
type Tools struct {
	Command string
	Args    []string
	Env     map[string]string // harness 启动 MCP 服务器时只传这些环境变量里的值，数据目录要写在这里
	URL     string
	Names   []string
	Timeout int // 单次工具调用最多等几秒；0 用 harness 的默认值。等用户回答的工具要设大
}

// toolUser 由能接 MCP 工具的适配器实现。
type toolUser interface{ usesTools() bool }

// UsesTools 报告这个 harness 能不能在一次运行里调用 Spec.Tools。
func UsesTools(runtime string) bool {
	a, err := Get(runtime)
	if err != nil {
		return false
	}
	t, ok := a.(toolUser)
	return ok && t.usesTools()
}

// compacter 由能按调用设压缩上限（Spec.CompactAt）的适配器实现。
type compacter interface{ compacts() bool }

// Compacts 报告这个 harness 会不会按 Spec.CompactAt 自己压缩会话。不会时由 morula 写交接记录、换会话。
func Compacts(runtime string) bool {
	a, err := Get(runtime)
	if err != nil {
		return false
	}
	c, ok := a.(compacter)
	return ok && c.compacts()
}

// resumer 由能续跑会话、续跑时还能接 MCP 工具的适配器实现。
type resumer interface{ resumes() bool }

// Resumes 报告这个 harness 能不能续跑会话。用户和 agent 对话时要用。
func Resumes(runtime string) bool {
	a, err := Get(runtime)
	if err != nil {
		return false
	}
	r, ok := a.(resumer)
	return ok && r.resumes()
}

// Agentic 报告这个 harness 能不能当 agent 方式的助理：能接 MCP 工具，也能续跑会话。
func Agentic(runtime string) bool {
	a, err := Get(runtime)
	if err != nil {
		return false
	}
	r, ok := a.(resumer)
	return ok && r.resumes() && UsesTools(runtime)
}

// Event 是运行中的一步，给界面显示进度。
type Event struct {
	// message：agent 的话；command：执行命令；done：命令结束；thinking：思考摘要；
	// tool：调用 MCP 工具；tool_done：工具返回（Failed 表示失败）；file：改了文件
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Code   *int   `json:"code,omitempty"`
	Tool   string `json:"tool,omitempty"` // tool、tool_done：工具名，不带服务器名
	Args   string `json:"args,omitempty"` // tool：参数的 JSON
	Failed bool   `json:"failed,omitempty"`
}

// Result 是一次运行的结果。
type Result struct {
	Session string // harness 的会话 id
	Final   string // 最后一条 agent 消息；有 Schema 时是 JSON
	Tokens  int    // 这次运行所有请求的输入加输出 token，按次计费时看它
	Context int    // 最后一次请求的上下文大小（输入加输出 token），判断会话该不该换时看它；harness 给不出时是 0
}

// ContextTokens 是判断会话长短用的 token 数：harness 给出上下文大小时用它，否则用这次运行的总量。
func (r Result) ContextTokens() int {
	if r.Context > 0 {
		return r.Context
	}
	return r.Tokens
}

// Step 是从轨迹文件读出的一步。
type Step struct {
	Role string `json:"role"` // user、agent、tool、output
	Text string `json:"text"`
	TS   string `json:"ts,omitempty"`
}

// Model 是 harness 能用的一个模型。
type Model struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Efforts []string `json:"efforts"`
	Default string   `json:"default,omitempty"`
	// Via 是插件或 API key 来源的 id，模型经 morula 的网关走；空表示 harness 自己的模型。
	// opencode、pi 自己的模型名也带「/」（provider/model），所以不能按「/」判断。
	Via string `json:"via,omitempty"`
	// Context 是模型的上下文窗口（token）；harness 没给时为 0。
	Context int `json:"context,omitempty"`
	// Credits 是每次调用按几倍扣点数，写成 harness 给的样子（例如 x0.29 credits）；只有 WorkBuddy 给。
	Credits string `json:"credits,omitempty"`
}

// Profile 是一个 harness 的说明，设置页照着显示。只写 morula 派活用得到的几项。
type Profile struct {
	Bin      string `json:"bin"`      // 命令名
	Sessions string `json:"sessions"` // 会话记录存在哪
	Sandbox  string `json:"sandbox"`  // 只读、可写两档各怎么限制
	Tools    bool   `json:"tools"`    // 能挂 morula 的 MCP 工具
	Compact  bool   `json:"compact"`  // 按 morula 的上限自己压缩会话
	Resume   bool   `json:"resume"`   // 能续跑会话
	Images   bool   `json:"images"`   // 能随提示词附图片
}

// profiler 由写了说明的适配器实现。
type profiler interface{ profile() Profile }

type Adapter interface {
	Name() string
	// Detect 返回 nil 表示本机装了这个 harness 并能用。
	Detect() error
	Models() []Model
	Run(ctx context.Context, s Spec, on func(Event)) (Result, error)
	Trajectory(session string) ([]Step, error)
}

var adapters = map[string]Adapter{}

func register(a Adapter) { adapters[a.Name()] = a }

// Get 按名字取适配器；没有适配器的 harness 返回错误。
func Get(name string) (Adapter, error) {
	if a, ok := adapters[name]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("harness %q 还没有适配器，目前能运行的是：%s", name, strings.Join(Names(), "、"))
}

// Names 返回有适配器的 harness，按名字排序。
func Names() []string {
	out := make([]string, 0, len(adapters))
	for n := range adapters {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Info 是设置页显示的一个 harness。
type Info struct {
	Name    string   `json:"name"`
	Ready   bool     `json:"ready"`
	Reason  string   `json:"reason,omitempty"`
	Models  []Model  `json:"models"`
	Profile *Profile `json:"profile,omitempty"`
	CLI     *CLI     `json:"cli,omitempty"` // 只在设置页的 Harness 一页要时查（/api/harnesses?cli）
}

// List 列出 runtimes 里每个 harness 的状态：有没有适配器、本机能不能用、有哪些模型。
func List(runtimes []string) []Info {
	out := []Info{}
	for _, n := range runtimes {
		a, err := Get(n)
		if err != nil {
			out = append(out, Info{Name: n, Reason: "未接入", Models: []Model{}})
			continue
		}
		var pf *Profile
		if p, ok := a.(profiler); ok {
			x := p.profile()
			pf = &x
		}
		if err := a.Detect(); err != nil {
			out = append(out, Info{Name: n, Reason: err.Error(), Models: []Model{}, Profile: pf})
			continue
		}
		out = append(out, Info{Name: n, Ready: true, Models: a.Models(), Profile: pf})
	}
	return out
}

// Ask 运行一次，并把带 Schema 的最终回复解析进 out。
func Ask(ctx context.Context, runtime string, s Spec, out any, on func(Event)) (Result, error) {
	a, err := Get(runtime)
	if err != nil {
		return Result{}, err
	}
	if on == nil {
		on = func(Event) {}
	}
	r, err := a.Run(ctx, s, on)
	if err != nil {
		return r, err
	}
	if out != nil {
		if err := json.Unmarshal([]byte(extractJSON(r.Final)), out); err != nil {
			return r, fmt.Errorf("看不懂 %s 的回复（应为 JSON）：%w\n%s", runtime, err, clip(r.Final, 600))
		}
	}
	return r, nil
}

// extractJSON 去掉模型偶尔包在外面的 ```json 围栏。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

var ErrNoSession = errors.New("找不到这个会话的轨迹文件")

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Chosen 只留用户勾上的工具（on），每个工具只留勾上的模型（pick 里没有这个工具时全留）。
func Chosen(list []Info, on []string, pick map[string][]string) []Info {
	out := []Info{}
	for _, in := range list {
		if !slices.Contains(on, in.Name) {
			continue
		}
		if ids, ok := pick[in.Name]; ok {
			in.Models = slices.DeleteFunc(slices.Clone(in.Models), func(m Model) bool { return !slices.Contains(ids, m.ID) })
		}
		out = append(out, in)
	}
	return out
}
