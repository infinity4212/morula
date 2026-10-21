package harness

// 每个 harness 命令行程序的版本、最新版本、怎么更新、没装时怎么装。设置页的 Harness 一页照着显示。
// 按程序文件真实所在的位置
// 判断它是怎么装的，就用同一种方式更新；判断不了时只显示版本，不给更新。安装命令只给用户复制，morula 不替用户跑。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// CLI 是一个 harness 命令行程序的情况。
type CLI struct {
	Path    string `json:"path,omitempty"`    // 在 PATH 里找到的命令
	Version string `json:"version,omitempty"` // 本机的版本；跑 --version 失败时为空
	Broken  string `json:"broken,omitempty"`  // 命令在，但 --version 跑不起来时的输出
	Latest  string `json:"latest,omitempty"`  // 能更新到的最新版本；查不到时为空
	Via     string `json:"via,omitempty"`     // 怎么装的：npm、bun、pnpm、brew、self；判断不了时为空
	Command string `json:"command,omitempty"` // 更新命令，写成用户在终端里敲的样子
	Update  bool   `json:"update,omitempty"`  // Latest 比 Version 新，并且知道怎么更新
	Install []Cmd  `json:"install,omitempty"` // 没装时的安装命令
}

// Cmd 是一条安装命令。
type Cmd struct {
	Via     string `json:"via"` // script、brew、npm
	Command string `json:"command"`
	Missing string `json:"missing,omitempty"` // 这条命令要的程序本机没有时，写要先装什么；空表示能直接跑
}

// cliSpec 是一个 harness 发布在哪里。
type cliSpec struct {
	bin  string
	npm  []string // npm 上的包，现在用的排第一
	brew string   // Homebrew 的 formula
	cask string   // Homebrew 的 cask
	// self 是程序自带的更新命令：真实路径落在官方安装脚本放的位置时用
	self    func(real string) []string
	install []Cmd // 官方文档给的安装命令，排在 npm 前面
}

var cliSpecs = map[string]cliSpec{
	"codex": {bin: "codex", npm: []string{"@openai/codex"}, cask: "codex",
		self: func(real string) []string {
			if strings.Contains(real, "/.codex/packages/standalone/") {
				return []string{"codex", "update"}
			}
			return nil
		},
		install: []Cmd{{Via: "script", Command: "curl -fsSL https://chatgpt.com/codex/install.sh | sh"}, {Via: "brew", Command: "brew install --cask codex"}}},
	"opencode": {bin: "opencode", npm: []string{"opencode-ai"}, brew: "opencode",
		self: func(real string) []string {
			if strings.Contains(real, "/.opencode/bin/") {
				return []string{"opencode", "upgrade"}
			}
			return nil
		},
		install: []Cmd{{Via: "script", Command: "curl -fsSL https://opencode.ai/install | bash"}}},
	"pi":        {bin: "pi", npm: []string{"@earendil-works/pi-coding-agent", "@mariozechner/pi-coding-agent"}},
	"workbuddy": {bin: "codebuddy", npm: []string{"@tencent-ai/codebuddy-code"}}, // WorkBuddy 的终端版
	// hermes 的安装脚本把程序装在 ~/.hermes/hermes-agent，~/.local/bin/hermes 是启动脚本；查不到最新版本，只显示版本。
	"hermes": {bin: "hermes",
		self: func(real string) []string {
			if strings.HasSuffix(real, "/.local/bin/hermes") {
				return []string{"hermes", "update"}
			}
			return nil
		},
		install: []Cmd{{Via: "script", Command: "curl -fsSL https://raw.githubusercontent.com/NousResearch/hermes-agent/main/scripts/install.sh | bash"}}},
}

// 查最新版本的地址；测试里换成本机服务。
var (
	npmRegistry = "https://registry.npmjs.org"
	brewAPI     = "https://formulae.brew.sh/api"
)

var verRe = regexp.MustCompile(`\d+\.\d+\.\d+[0-9A-Za-z.+-]*`)

// latestCache 记查到的最新版本，一小时内不再查。
var latestCache = struct {
	sync.Mutex
	m map[string]latestHit
}{m: map[string]latestHit{}}

type latestHit struct {
	v  string
	at time.Time
}

// CLIs 查 names 里每个 harness 的命令行程序，并发查，整体最多等 10 秒。
func CLIs(ctx context.Context, names []string) map[string]CLI {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out := map[string]CLI{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := cliOf(ctx, n)
			mu.Lock()
			out[n] = c
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func cliOf(ctx context.Context, name string) CLI {
	spec, ok := cliSpecs[name]
	if !ok {
		return CLI{}
	}
	path, err := exec.LookPath(spec.bin)
	if err != nil {
		return CLI{Install: installCmds(spec)}
	}
	c := CLI{Path: path}
	if c.Version, c.Broken = runVersion(ctx, path); c.Broken != "" {
		c.Install = installCmds(spec)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	real = filepath.ToSlash(real)
	argv, via, src := updateCmd(spec, real)
	if argv != nil {
		c.Via, c.Command = via, strings.Join(argv, " ")
		c.Latest = latest(ctx, src)
		c.Update = c.Version != "" && c.Latest != "" && newer(c.Latest, c.Version)
	}
	return c
}

// updateCmd 按真实路径判断怎么装的，返回更新命令、方式，以及查最新版本的地方（npm:包名、brew:名字、cask:名字）。
func updateCmd(spec cliSpec, real string) (argv []string, via, src string) {
	if spec.self != nil {
		if a := spec.self(real); a != nil {
			if len(spec.npm) > 0 {
				src = "npm:" + spec.npm[0] // 官方安装脚本和 npm 发同一个版本号
			}
			return a, "self", src
		}
	}
	if strings.Contains(real, "/node_modules/") {
		for _, p := range spec.npm {
			if !strings.Contains(real, "/node_modules/"+p+"/") {
				continue
			}
			switch {
			case strings.Contains(real, "/.bun/"):
				return []string{"bun", "add", "-g", p + "@latest"}, "bun", "npm:" + p
			case strings.Contains(real, "/pnpm/"):
				return []string{"pnpm", "add", "-g", p + "@latest"}, "pnpm", "npm:" + p
			}
			return []string{"npm", "install", "-g", p + "@latest"}, "npm", "npm:" + p
		}
	}
	if spec.brew != "" && strings.Contains(real, "/Cellar/"+spec.brew+"/") {
		return []string{"brew", "upgrade", spec.brew}, "brew", "brew:" + spec.brew
	}
	if spec.cask != "" && strings.Contains(real, "/Caskroom/"+spec.cask+"/") {
		return []string{"brew", "upgrade", "--cask", spec.cask}, "brew", "cask:" + spec.cask
	}
	return nil, "", ""
}

func installCmds(spec cliSpec) []Cmd {
	out := append([]Cmd{}, spec.install...)
	if len(spec.npm) > 0 {
		out = append(out, Cmd{Via: "npm", Command: "npm install -g " + spec.npm[0]})
	}
	for i := range out {
		out[i].Missing = needs(out[i].Via)
	}
	return out
}

// needs 报告这种安装方式缺什么程序：brew 要 Homebrew，npm 要 Node.js，script 要 curl。
func needs(via string) string {
	need := map[string][2]string{"brew": {"brew", "Homebrew"}, "npm": {"npm", "Node.js"}, "script": {"curl", "curl"}}[via]
	if need[0] == "" {
		return ""
	}
	if _, err := exec.LookPath(need[0]); err != nil {
		return "要先装 " + need[1]
	}
	return ""
}

var installing sync.Map // harness 名字 -> true

// runVersion 跑一次 path --version，最多等 5 秒。跑不起来时 broken 是输出的第一行。
func runVersion(ctx context.Context, path string) (version, broken string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if v := verRe.FindString(string(b)); err == nil && v != "" {
		return v, ""
	}
	broken = strings.TrimSpace(firstLine(string(b)))
	if broken == "" && err != nil {
		broken = err.Error()
	}
	return "", broken
}

// setupSeen 记下每个命令最近一次 --version 的结果，1 分钟内不再跑：界面装软件时每秒查一次。
var setupSeen = struct {
	sync.Mutex
	m map[string]setupRun
}{m: map[string]setupRun{}}

type setupRun struct {
	at     time.Time
	broken string
}

// Setup 报告 harness 装好没有，以及没装好时能用的安装命令。命令在 PATH 里但 --version 跑不起来
// （例如 hermes 的启动脚本指向已删掉的目录）也算没装好，broken 是它的输出。
func Setup(name string) (installed bool, broken string, cmds []Cmd) {
	spec, ok := cliSpecs[name]
	if !ok {
		return false, "", nil
	}
	path, err := exec.LookPath(spec.bin)
	if err != nil {
		return false, "", installCmds(spec)
	}
	setupSeen.Lock()
	r, ok := setupSeen.m[path]
	setupSeen.Unlock()
	if !ok || time.Since(r.at) > time.Minute {
		_, r.broken = runVersion(context.Background(), path)
		r.at = time.Now()
		setupSeen.Lock()
		setupSeen.m[path] = r
		setupSeen.Unlock()
	}
	return r.broken == "", r.broken, installCmds(spec)
}

// ForgetSetup 让下一次 Setup 重新跑 --version，装完之后调用。
func ForgetSetup() {
	setupSeen.Lock()
	clear(setupSeen.m)
	setupSeen.Unlock()
}

// InstallCLI 用 via 那条官方安装命令装上 harness，返回输出。用户在界面上点了安装才调用。
func InstallCLI(ctx context.Context, name, via string) (string, error) {
	spec, ok := cliSpecs[name]
	if !ok {
		return "", fmt.Errorf("不知道 %s 怎么装", name)
	}
	var c *Cmd
	for _, x := range installCmds(spec) {
		if x.Via == via {
			c = &x
			break
		}
	}
	if c == nil {
		return "", fmt.Errorf("%s 没有 %s 这种装法", name, via)
	}
	if c.Missing != "" {
		return "", fmt.Errorf("用 %s 装 %s %s", via, name, c.Missing)
	}
	if _, busy := installing.LoadOrStore(name, true); busy {
		return "", fmt.Errorf("%s 正在安装", name)
	}
	defer installing.Delete(name)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/zsh", "-lc", c.Command)
	cmd.Env = append(os.Environ(), "CI=1", "NO_COLOR=1") // 不进交互界面
	cmd.Stdin = nil
	b, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(b))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, fmt.Errorf("%s 等了 10 分钟没跑完", c.Command)
		}
		return out, fmt.Errorf("%s 失败：%v", c.Command, err)
	}
	return out, nil
}

// latest 查 src 的最新版本，查不到返回空。
func latest(ctx context.Context, src string) string {
	if src == "" {
		return ""
	}
	latestCache.Lock()
	h, ok := latestCache.m[src]
	latestCache.Unlock()
	if ok && time.Since(h.at) < time.Hour {
		return h.v
	}
	kind, name, _ := strings.Cut(src, ":")
	var url string
	var v struct {
		Version  string `json:"version"`
		Versions struct {
			Stable string `json:"stable"`
		} `json:"versions"`
	}
	switch kind {
	case "npm":
		url = npmRegistry + "/" + name + "/latest"
	case "brew":
		url = brewAPI + "/formula/" + name + ".json"
	case "cask":
		url = brewAPI + "/cask/" + name + ".json"
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&v) != nil {
		return ""
	}
	got := v.Version
	if kind == "brew" {
		got = v.Versions.Stable
	}
	got = verRe.FindString(got) // cask 的版本可能带「,构建号」
	latestCache.Lock()
	latestCache.m[src] = latestHit{got, time.Now()}
	latestCache.Unlock()
	return got
}

// newer 比较 a 是否比 b 新：按点分的数字逐段比，预发布后缀不算。
func newer(a, b string) bool {
	num := func(s string) []int {
		s, _, _ = strings.Cut(s, "-")
		s, _, _ = strings.Cut(s, "+")
		var out []int
		for _, p := range strings.Split(s, ".") {
			n := 0
			fmt.Sscanf(p, "%d", &n)
			out = append(out, n)
		}
		return out
	}
	x, y := num(a), num(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var p, q int
		if i < len(x) {
			p = x[i]
		}
		if i < len(y) {
			q = y[i]
		}
		if p != q {
			return p > q
		}
	}
	return false
}

var updating sync.Map // 正在更新的 harness

// UpdateCLI 用 harness 装的那种方式把它更新到最新版本，返回命令的输出。最多等 5 分钟。
func UpdateCLI(ctx context.Context, name string) (string, error) {
	spec, ok := cliSpecs[name]
	if !ok {
		return "", fmt.Errorf("不知道 %s 怎么更新", name)
	}
	if _, busy := updating.LoadOrStore(name, true); busy {
		return "", fmt.Errorf("%s 正在更新", name)
	}
	defer updating.Delete(name)
	path, err := exec.LookPath(spec.bin)
	if err != nil {
		return "", fmt.Errorf("没找到 %s 命令", spec.bin)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	argv, _, src := updateCmd(spec, filepath.ToSlash(real))
	if argv == nil {
		return "", fmt.Errorf("看不出 %s 是怎么装的（%s），请按原来的方式自己更新", name, real)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "CI=1", "NO_COLOR=1") // 不进交互界面
	b, err := cmd.CombinedOutput()
	latestCache.Lock()
	delete(latestCache.m, src)
	latestCache.Unlock()
	out := strings.TrimSpace(string(b))
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return out, fmt.Errorf("%s 等了 5 分钟没跑完", strings.Join(argv, " "))
		}
		return out, fmt.Errorf("%s 失败：%v", strings.Join(argv, " "), err)
	}
	return out, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// ForgetModels 丢掉这个 harness 缓存的模型列表，下次 Models 重新问它。codex 每次都读 models_cache.json，没有缓存。
func ForgetModels(name string) {
	switch name {
	case "opencode":
		ocModels.Lock()
		ocModels.at = time.Time{}
		ocModels.Unlock()
	case "pi":
		piModels.Lock()
		piModels.at = time.Time{}
		piModels.Unlock()
	case "workbuddy":
		cbModels.Lock()
		cbModels.at = time.Time{}
		cbModels.Unlock()
	}
}
