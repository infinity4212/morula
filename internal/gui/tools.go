package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"morula/internal/env"
	"morula/internal/harness"
	"morula/internal/plugin"
	"morula/internal/store"
	"morula/internal/subs"
)

// 启动流程的第一、二步：选 harness（没装的在这里装），每个工具配订阅、勾模型。
// 没勾的工具和模型在 morula 里不出现（chosen）。hermes 没有订阅，第二步不列它。

// toolLabels 是界面上的名字；不在表里的用 harness 名。
var toolLabels = map[string]string{"workbuddy": "WorkBuddy"}

// loginCmds 是在终端里登录要敲的命令。codex 由 morula 自己跑 codex login；codebuddy 和 pi 只能在交互界面里输 /login。
var loginCmds = map[string]struct{ cmd, hint string }{
	"opencode":  {"opencode auth login", "按提示选服务并登录"},
	"pi":        {"pi", "进去后输入 /login"},
	"workbuddy": {"codebuddy", "进去后输入 /login，用 WorkBuddy 的账号登录"},
}

// chosen 是用户勾上的工具和模型。
func chosen() []harness.Info {
	on, pick := db.Choice()
	return harness.Chosen(harness.List(store.Runtimes), on, pick)
}

// toolInstall 记后台在跑的安装：页面轮询 /api/tools 看进度。
var toolInstall = struct {
	sync.Mutex
	running map[string]bool
	err     map[string]string
	out     map[string]string
}{running: map[string]bool{}, err: map[string]string{}, out: map[string]string{}}

func toolList() []map[string]any {
	on := db.Tools()
	toolInstall.Lock()
	defer toolInstall.Unlock()
	rows := []map[string]any{}
	for _, n := range store.Runtimes {
		installed, broken, cmds := harness.Setup(n)
		if home, err := os.UserHomeDir(); err == nil {
			broken = strings.ReplaceAll(broken, home, "~")
		}
		label := toolLabels[n]
		if label == "" {
			label = n
		}
		// 没装好的不算勾上：没选过时全算勾上，关掉别的工具时不能把它一起写进去
		rows = append(rows, map[string]any{"name": n, "label": label, "installed": installed, "broken": broken, "enabled": installed && slices.Contains(on, n),
			"installing": toolInstall.running[n], "error": toolInstall.err[n], "output": toolInstall.out[n], "install": cmds,
			"sub": n != "hermes"})
	}
	return rows
}

// startInstall 在后台用第一条能直接跑的安装命令装上这个工具（via 非空时用那一条）。
func startInstall(name, via string) error {
	_, _, cmds := harness.Setup(name)
	if via == "" {
		for _, c := range cmds {
			if c.Missing == "" {
				via = c.Via
				break
			}
		}
	}
	if via == "" {
		if len(cmds) == 0 {
			return fmt.Errorf("不知道 %s 怎么装", name)
		}
		return fmt.Errorf("装 %s %s", name, cmds[0].Missing)
	}
	toolInstall.Lock()
	defer toolInstall.Unlock()
	if toolInstall.running[name] {
		return nil
	}
	toolInstall.running[name], toolInstall.err[name], toolInstall.out[name] = true, "", ""
	go func() {
		out, err := harness.InstallCLI(context.Background(), name, via)
		env.FixPath() // 安装脚本可能把命令放进新的目录，重新读一遍登录 shell 的 PATH
		harness.ForgetSetup()
		toolInstall.Lock()
		defer toolInstall.Unlock()
		toolInstall.running[name], toolInstall.out[name] = false, lastLines(out, 6)
		if err != nil {
			toolInstall.err[name] = err.Error()
		} else if ok, _, _ := harness.Setup(name); ok {
			// 装好的工具默认勾上
			if on := db.Tools(); !slices.Contains(on, name) {
				db.SetTools(append(on, name))
			}
		}
	}()
	return nil
}

// toolSub 是一个工具的订阅窗口：账号（只有 codex 能读到套餐和剩余额度）、来源（opencode、pi）和全部模型，勾上的标 on。
func toolSub(ctx context.Context, name string) (map[string]any, error) {
	if !slices.Contains(store.Runtimes, name) || name == "hermes" {
		return nil, fmt.Errorf("%s 没有订阅", name)
	}
	var info harness.Info
	for _, in := range harness.List([]string{name}) {
		info = in
	}
	picked, set := db.Picked(name)
	models := []map[string]any{}
	for _, m := range info.Models {
		models = append(models, map[string]any{"id": m.ID, "name": m.Name, "via": m.Via, "context": m.Context, "credits": m.Credits,
			"on": !set || slices.Contains(picked, m.ID)})
	}
	res := map[string]any{"name": name, "ready": info.Ready, "reason": info.Reason, "models": models}
	if l, ok := loginCmds[name]; ok {
		res["login"] = map[string]string{"command": l.cmd, "hint": l.hint}
	}
	switch name {
	case "codex":
		acct := codexState()
		as, _, _ := subs.Accounts(false)
		for _, a := range as {
			if a.Source == "codex" {
				acct["plan"], acct["windows"], acct["readAt"] = a.Plan, a.Windows, a.ReadAt
				break
			}
		}
		res["account"] = acct
	case "opencode", "pi":
		res["sources"] = toolSources(ctx, info.Models)
	}
	return res, nil
}

// toolSources 按模型的来源分组：经网关借来的按插件或 API key 来源，工具自己登录的按 provider（模型 id 里「/」前面那段）。
func toolSources(ctx context.Context, models []harness.Model) []map[string]any {
	names := map[string]string{}
	if ps, err := plugin.Catalog(ctx, false); err == nil {
		for _, p := range ps {
			names[p.ID] = p.Name
		}
	}
	var order []string
	count := map[string]int{}
	kind := map[string]string{}
	for _, m := range models {
		key, k := m.Via, "gateway"
		if key == "" {
			key, k = strings.SplitN(m.ID, "/", 2)[0], "own"
		}
		if _, ok := count[key]; !ok {
			order = append(order, key)
			kind[key] = k
		}
		count[key]++
	}
	out := []map[string]any{}
	for _, key := range order {
		label := key
		if n := names[key]; n != "" && kind[key] == "gateway" {
			label = n
		}
		out = append(out, map[string]any{"id": key, "name": label, "kind": kind[key], "models": count[key]})
	}
	return out
}

// codexOff 是 codex 自己的模型里没勾的，给「设置 / 账号」的 codex 一行用。没设过时为 nil。
func codexOff() []string {
	picked, set := db.Picked("codex")
	if !set {
		return nil
	}
	off := []string{}
	for _, m := range harness.CodexModels() {
		if !slices.Contains(picked, m.ID) {
			off = append(off, m.ID)
		}
	}
	return off
}

// setCodexOff 按「设置 / 账号」里 codex 一行取消勾选的模型改 codex 勾的模型；经网关借来的模型勾没勾不变。
func setCodexOff(_ string, off []string) error {
	own := map[string]bool{}
	for _, m := range harness.CodexModels() {
		own[m.ID] = true
	}
	ids := []string{}
	for _, m := range harness.CodexModels() {
		if !slices.Contains(off, m.ID) {
			ids = append(ids, m.ID)
		}
	}
	others := []string{} // 经网关借来的：设过就照原样，没设过就是全勾
	if picked, set := db.Picked("codex"); set {
		others = slices.DeleteFunc(picked, func(id string) bool { return own[id] })
	} else {
		for _, in := range harness.List([]string{"codex"}) {
			for _, m := range in.Models {
				if !own[m.ID] {
					others = append(others, m.ID)
				}
			}
		}
	}
	return db.SetPicked("codex", append(ids, others...))
}

// openLogin 在终端里跑登录命令；codebuddy、pi 只有交互界面里的 /login，morula 没法替用户登录。
func openLogin(name string) error {
	if name == "codex" {
		return startCodexLogin()
	}
	l, ok := loginCmds[name]
	if !ok {
		return fmt.Errorf("%s 不用登录", name)
	}
	script := fmt.Sprintf(`tell application "Terminal"
activate
do script "echo %s; %s"
end tell`, strings.ReplaceAll(l.hint, `"`, ``), l.cmd)
	return exec.Command("osascript", "-e", script).Run()
}

// testTool 让这个工具用这个模型回一句话，确认能用。
func testTool(ctx context.Context, name, model string) (string, error) {
	a, err := harness.Get(name)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "morula-test-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	res, err := a.Run(ctx, harness.Spec{Dir: dir, Model: model, Access: harness.ReadOnly, Ephemeral: true, Prompt: "只回复两个字：收到"}, func(harness.Event) {})
	return strings.TrimSpace(res.Final), err
}

func toolRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tools", func(w http.ResponseWriter, r *http.Request) {
		reply(w, toolList())
	})
	mux.HandleFunc("PUT /api/tools/enabled", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On []string `json:"on"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := db.SetTools(req.On); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, toolList())
	})
	mux.HandleFunc("POST /api/tools/{name}/install", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Via string `json:"via"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if err := startInstall(r.PathValue("name"), req.Via); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, toolList())
	})
	mux.HandleFunc("GET /api/tools/{name}/sub", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("refresh") { // 重新获取模型
			harness.ForgetModels(r.PathValue("name"))
		}
		res, err := toolSub(r.Context(), r.PathValue("name"))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, res)
	})
	mux.HandleFunc("PUT /api/tools/{name}/models", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			IDs []string `json:"ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := db.SetPicked(r.PathValue("name"), req.IDs); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		res, err := toolSub(r.Context(), r.PathValue("name"))
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, res)
	})
	mux.HandleFunc("POST /api/tools/{name}/login", func(w http.ResponseWriter, r *http.Request) {
		if err := openLogin(r.PathValue("name")); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/tools/{name}/test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
			fail(w, http.StatusBadRequest, "要选一个模型")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		start := time.Now()
		text, err := testTool(ctx, r.PathValue("name"), req.Model)
		res := map[string]any{"ok": err == nil, "ms": time.Since(start).Milliseconds(), "text": text}
		if err != nil {
			res["error"] = err.Error()
		}
		reply(w, res)
	})
}
