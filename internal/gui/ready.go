package gui

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"morula/internal/harness"
	"morula/internal/plugin"
	"morula/internal/store"
	"morula/internal/work"
)

// 首次使用：助理要有一个能用的模型才算上线。没上线时前端弹出首次引导，让用户接入 codex 订阅、插件订阅或 API key；
// 选好模型后助理做一次自我介绍，验证模型真的能调用；之后引导问要不要整理本机会话。

// ready 判断助理现在能不能用，不能用时给出原因。
func ready(ctx context.Context) (bool, string) {
	a, err := db.Assistant()
	if err != nil {
		return false, err.Error()
	}
	if a == nil {
		return false, "团队里没有助理"
	}
	h, err := harness.Get(a.Runtime)
	if err != nil {
		return false, a.Runtime + " 未接入"
	}
	if err := h.Detect(); err != nil {
		return false, err.Error()
	}
	// 网关模型：codex 里写作 来源/模型；opencode、pi 自己的模型 id 也带「/」，网关模型写作 morula/来源/模型。
	inner, viaGW := a.Model, a.Runtime == "codex" && strings.Contains(a.Model, "/")
	if a.Runtime != "codex" {
		inner, viaGW = strings.CutPrefix(a.Model, "morula/")
	}
	if viaGW {
		if gw == nil {
			return false, "模型转接没有启动，" + a.Model + " 暂时用不了"
		}
		if _, ok := plugin.Find(ctx, inner); !ok {
			return false, "模型来源里没有 " + a.Model + "（没登录或已删除）"
		}
		return true, ""
	}
	if a.Runtime == "codex" && !harness.CodexLoggedIn() {
		return false, "codex 还没登录，用不了它自己的模型 " + a.Model
	}
	return true, ""
}

// codexLogin 是后台在跑的 codex login。
var codexLogin struct {
	mu      sync.Mutex
	running bool
	url     string
	err     string
}

var urlRe = regexp.MustCompile(`https://\S+`)

func startCodexLogin() error {
	codexLogin.mu.Lock()
	defer codexLogin.mu.Unlock()
	if codexLogin.running {
		return nil
	}
	cmd := exec.Command("codex", "login")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	codexLogin.running, codexLogin.url, codexLogin.err = true, "", ""
	// codex login 自己会打开浏览器；登录地址也留给页面，浏览器没打开时用户可以点
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if u := urlRe.FindString(sc.Text()); u != "" {
				codexLogin.mu.Lock()
				if codexLogin.url == "" {
					codexLogin.url = u
				}
				codexLogin.mu.Unlock()
			}
		}
		close(done)
	}()
	go func() {
		t := time.AfterFunc(10*time.Minute, func() { cmd.Process.Kill() })
		<-done
		err := cmd.Wait()
		t.Stop()
		codexLogin.mu.Lock()
		codexLogin.running = false
		if err != nil && !harness.CodexLoggedIn() {
			codexLogin.err = "codex login 没有完成：" + err.Error()
		}
		codexLogin.mu.Unlock()
	}()
	return nil
}

// codexState 是 codex 订阅的登录状态：装没装、登没登录、正在登录时的登录页。
func codexState() map[string]any {
	_, lookErr := exec.LookPath("codex")
	codexLogin.mu.Lock()
	defer codexLogin.mu.Unlock()
	return map[string]any{
		"installed": lookErr == nil, "loggedIn": harness.CodexLoggedIn(),
		"logging": codexLogin.running, "url": codexLogin.url, "error": codexLogin.err,
	}
}

// readyState 是首次接入弹窗要的内容。
func readyState(ctx context.Context) map[string]any {
	online, reason := ready(ctx)
	res := map[string]any{"online": online, "reason": reason}
	if a, _ := db.Assistant(); a != nil {
		res["assistant"] = map[string]string{"name": a.Name, "runtime": a.Runtime, "model": a.Model, "effort": a.Effort}
	}
	res["codex"] = codexState()
	ps, _ := plugin.Catalog(ctx, false)
	sources := []map[string]any{}
	for _, p := range ps {
		if p.SignedIn && !plugin.IsChatGPT(p) { // ChatGPT 订阅算在上面的 codex 里
			sources = append(sources, map[string]any{"id": p.ID, "name": p.Name, "kind": map[bool]string{true: "api", false: "plugin"}[p.Builtin], "models": len(p.Models)})
		}
	}
	res["sources"] = sources
	// 本机能用的 AI 工具和各自的模型，弹窗里先选工具再选模型。codex 排第一，其余按 store.Runtimes 的顺序。
	tools, n := []map[string]any{}, 0
	for _, in := range chosen() {
		if in.Ready {
			tools = append(tools, map[string]any{"name": in.Name, "models": in.Models})
			n += len(in.Models)
		}
	}
	res["tools"], res["modelCount"] = tools, n
	return res
}

func readyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/ready", func(w http.ResponseWriter, r *http.Request) {
		reply(w, readyState(r.Context()))
	})
	mux.HandleFunc("POST /api/codex/login", func(w http.ResponseWriter, r *http.Request) {
		if err := startCodexLogin(); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, readyState(r.Context()))
	})
	// 选定助理的模型：助理和验收 agent 用同一个。验收的思考强度用 low，模型不支持 low 时留空。
	mux.HandleFunc("POST /api/ready/assistant", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Runtime string `json:"runtime"`
			Model   string `json:"model"`
			Effort  string `json:"effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Model) == "" {
			fail(w, http.StatusBadRequest, "要选一个模型")
			return
		}
		rt := req.Runtime
		if rt == "" {
			rt = "codex"
		}
		var md *harness.Model
		for _, in := range harness.List([]string{rt}) {
			for i := range in.Models {
				if in.Models[i].ID == req.Model {
					md = &in.Models[i]
				}
			}
		}
		if md == nil {
			fail(w, http.StatusBadRequest, rt+" 里没有这个模型："+req.Model)
			return
		}
		if !slices.Contains(md.Efforts, req.Effort) {
			req.Effort = md.Default
		}
		low := ""
		if slices.Contains(md.Efforts, "low") {
			low = "low"
		}
		a, err := db.Assistant()
		if err != nil || a == nil {
			fail(w, http.StatusConflict, "团队里没有助理")
			return
		}
		if err := db.UpdateAgent(a.Name, store.Patch{Runtime: &rt, Model: &req.Model, Effort: &req.Effort}); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if acc, _ := db.Agent(work.AcceptName); acc != nil {
			db.UpdateAgent(acc.Name, store.Patch{Runtime: &rt, Model: &req.Model, Effort: &low})
		}
		reply(w, readyState(r.Context()))
	})
	// 首次引导第 2 步：助理真实调用一次模型做自我介绍，介绍发到对话里。失败时 ok 为 false，引导让用户换模型。
	mux.HandleFunc("POST /api/ready/intro", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		if online, reason := ready(ctx); !online {
			reply(w, map[string]any{"ok": false, "error": reason})
			return
		}
		text, err := eng.Introduce(ctx)
		if err != nil {
			reply(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		reply(w, map[string]any{"ok": true, "text": text})
	})
	mux.HandleFunc("POST /api/apikeys", func(w http.ResponseWriter, r *http.Request) {
		var a plugin.APIKey
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		saved, err := plugin.AddAPIKey(r.Context(), a)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, map[string]any{"id": saved.ID, "name": saved.Name, "models": saved.Models})
	})
	mux.HandleFunc("DELETE /api/apikeys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := plugin.RemoveAPIKey(r.PathValue("id")); err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		reply(w, pluginState(r.Context(), true))
	})
}
