package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"morula/internal/gateway"
	"morula/internal/harness"
	"morula/internal/plugin"
	"morula/internal/subs"
)

var gw *gateway.Gateway

// setupPlugins 起本地网关，把插件模型接到 harness 上。网关起不来时插件模型不可用，其他功能照常。
func setupPlugins() {
	g, err := gateway.Start()
	if err != nil {
		log.Println("morula:", err)
		return
	}
	gw = g
	go checkinLoop()
	harness.Plugins.Models = pluginModels
	harness.Plugins.Off = plugin.ModelOff
	harness.Plugins.Route = func(ctx context.Context, model string) (harness.Route, bool) {
		if !strings.Contains(model, "/") {
			return harness.Route{}, false
		}
		ref, ok := plugin.Find(ctx, model)
		if !ok {
			return harness.Route{}, false
		}
		return harness.Route{BaseURL: gw.BaseURL(), Key: gw.Key, Context: ref.Model.Context}, true
	}
}

// checkinLoop 每半小时看一次有没有该签到的来源（plugin.CheckinDue），启动后先等一分钟。
func checkinLoop() {
	time.Sleep(time.Minute)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		plugin.CheckinDue(ctx)
		cancel()
		time.Sleep(30 * time.Minute)
	}
}

// pluginModels 列已登录 provider 和 API key 来源的模型。没有启用的插件时不启动插件运行器。
func pluginModels() []harness.Model {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ps, err := plugin.Catalog(ctx, false)
	if err != nil {
		log.Println("morula: 读插件模型失败：", err)
	}
	var out []harness.Model
	for _, p := range ps {
		if !p.SignedIn {
			continue
		}
		off := plugin.OffModels()[p.ID]
		for _, m := range p.Models {
			if slices.Contains(off, m.ID) {
				continue
			}
			name := m.Name
			if name == "" {
				name = m.ID
			}
			md := harness.Model{ID: p.ID + "/" + m.ID, Name: p.Name + " · " + name, Efforts: m.Efforts, Via: p.ID}
			if md.Efforts == nil {
				md.Efforts = []string{}
			}
			out = append(out, md)
		}
	}
	return out
}

// pluginState 是插件页要的全部内容。
func pluginState(ctx context.Context, refresh bool) map[string]any {
	list, err := plugin.List()
	res := map[string]any{"plugins": []any{}, "providers": []plugin.Provider{}}
	if err != nil {
		res["error"] = err.Error()
		return res
	}
	if bin, err := plugin.Bun(); err != nil {
		res["bun"] = err.Error()
	} else {
		res["bunVersion"] = bunVersion(bin)
	}
	errs := map[string]string{}
	on := 0
	for _, e := range list {
		if !e.Off {
			on++
		}
	}
	if on > 0 {
		if h, err := plugin.Get(ctx); err != nil {
			res["error"] = err.Error()
		} else {
			for _, l := range h.Loaded {
				errs[l.Spec] = l.Error
			}
		}
	}
	// 插件运行器出错时 Catalog 仍返回 API key 来源
	ps, err := plugin.Catalog(ctx, refresh)
	if err != nil && res["error"] == nil {
		res["error"] = err.Error()
	}
	if ps != nil {
		// ChatGPT 订阅在「模型来源」里就是 codex 那一行，不另列
		res["providers"] = slices.DeleteFunc(slices.Clone(ps), plugin.IsChatGPT)
	}
	rows := []map[string]any{}
	for _, e := range list {
		rows = append(rows, map[string]any{"spec": e.Spec, "name": e.Name, "off": e.Off, "info": plugin.Meta(e.Name), "error": errs[e.Spec]})
	}
	res["plugins"] = rows
	off := plugin.OffModels()
	if o := codexOff(); o != nil {
		off["codex"] = o
	} else {
		delete(off, "codex")
	}
	res["off"] = off
	res["checkin"] = plugin.Checkins()
	res["codex"] = codexSource()
	return res
}

// codexSource 是「模型来源」里 codex 订阅那一行：登录状态、套餐、codex 自己的全部模型。
// 套餐来自最近一次 codex 会话记下的额度，不读 auth.json。
func codexSource() map[string]any {
	c := codexState()
	c["models"] = harness.CodexModels()
	accs, _, _ := subs.Accounts(false)
	for _, a := range accs {
		if a.Source == "codex" {
			c["plan"] = a.Plan
			break
		}
	}
	return c
}

// changed 在插件增删、启停后重启插件运行器。
func changed() {
	plugin.Restart()
	plugin.Forget()
}

// gatewayState 是设置页「模型网关」一节的内容。
func gatewayState() map[string]any {
	res := map[string]any{"running": gw != nil}
	if gw != nil {
		res["addr"] = gw.BaseURL()
	} else {
		res["error"] = "模型转接没有启动，插件里的模型暂时用不了"
	}
	if bun, err := plugin.Bun(); err != nil {
		res["bunError"] = err.Error()
	} else {
		res["bun"] = bun
		if out, err := exec.Command(bun, "--version").Output(); err == nil {
			res["bunVersion"] = strings.TrimSpace(string(out))
		}
	}
	list, _ := plugin.List()
	on := 0
	for _, e := range list {
		if !e.Off {
			on++
		}
	}
	res["plugins"], res["enabled"] = len(list), on
	return res
}

func pluginRoutes(mux *http.ServeMux) {
	// 一个来源取消勾选哪些模型。来源 codex 指 codex 订阅自己的模型，存在 codex 这个工具勾的模型里。
	mux.HandleFunc("PUT /api/sources/{id}/off", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Off []string `json:"off"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		set := plugin.SetModelsOff
		if r.PathValue("id") == "codex" { // codex 订阅自己的模型和「AI 工具 / codex」勾的是同一份
			set = setCodexOff
		}
		if err := set(r.PathValue("id"), req.Off); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, pluginState(r.Context(), false))
	})
	mux.HandleFunc("POST /api/sources/test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Source string `json:"source"`
			Model  string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
			fail(w, http.StatusBadRequest, "要选一个模型")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		start := time.Now()
		text, err := testModel(ctx, req.Source, req.Model)
		res := map[string]any{"ok": err == nil, "ms": time.Since(start).Milliseconds(), "text": text}
		if err != nil {
			res["error"] = err.Error()
		}
		reply(w, res)
	})
	// 插件页「发现」：q 为空时搜默认关键字。已装的包标上 installed。
	mux.HandleFunc("GET /api/plugins/search", func(w http.ResponseWriter, r *http.Request) {
		ps, err := plugin.Search(r.Context(), r.URL.Query().Get("q"))
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		list, _ := plugin.List()
		rows := []map[string]any{}
		for _, p := range ps {
			installed := slices.ContainsFunc(list, func(e plugin.Entry) bool { return e.Name == p.Name })
			rows = append(rows, map[string]any{"name": p.Name, "version": p.Version, "description": p.Description, "homepage": p.Homepage,
				"publisher": p.Publisher, "updated": p.Updated, "weekly": p.Weekly, "installed": installed})
		}
		reply(w, map[string]any{"query": r.URL.Query().Get("q"), "default": plugin.DefaultQuery, "packages": rows})
	})
	mux.HandleFunc("GET /api/plugins/updates", func(w http.ResponseWriter, r *http.Request) {
		up, errs, err := plugin.Updates(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]any{"updates": up, "errors": errs})
	})
	mux.HandleFunc("GET /api/gateway", func(w http.ResponseWriter, r *http.Request) {
		reply(w, gatewayState())
	})
	mux.HandleFunc("GET /api/plugins", func(w http.ResponseWriter, r *http.Request) {
		reply(w, pluginState(r.Context(), r.URL.Query().Has("refresh")))
	})
	mux.HandleFunc("POST /api/plugins", func(w http.ResponseWriter, r *http.Request) {
		var p struct{ Spec string }
		json.NewDecoder(r.Body).Decode(&p)
		if _, err := plugin.Add(r.Context(), p.Spec); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		changed()
		reply(w, pluginState(r.Context(), true))
	})
	mux.HandleFunc("DELETE /api/plugins/{name...}", func(w http.ResponseWriter, r *http.Request) {
		if err := plugin.Remove(r.Context(), r.PathValue("name")); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		changed()
		reply(w, pluginState(r.Context(), true))
	})
	mux.HandleFunc("POST /api/plugins/off", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Name string
			Off  bool
		}
		json.NewDecoder(r.Body).Decode(&p)
		if err := plugin.SetOff(p.Name, p.Off); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		changed()
		reply(w, pluginState(r.Context(), true))
	})

	// 登录：先 authorize 拿到登录地址（后端用系统浏览器打开），再 callback 等结果。
	mux.HandleFunc("POST /api/plugins/signin", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Provider string            `json:"provider"`
			Method   int               `json:"method"`
			Inputs   map[string]string `json:"inputs"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		h, err := plugin.Get(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		var out struct {
			Session      string `json:"session,omitempty"`
			URL          string `json:"url,omitempty"`
			Instructions string `json:"instructions,omitempty"`
			Mode         string `json:"mode"`
		}
		if err := h.Call(r.Context(), "authorize", p, &out); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if out.Mode == "done" {
			plugin.Forget()
		}
		if strings.HasPrefix(out.URL, "https://") || strings.HasPrefix(out.URL, "http://") {
			exec.Command("open", out.URL).Start()
		}
		reply(w, out)
	})
	mux.HandleFunc("POST /api/plugins/signin/{session}", func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Code *string `json:"code"`
		}
		json.NewDecoder(r.Body).Decode(&p)
		h, err := plugin.Get(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		params := map[string]any{"session": r.PathValue("session")}
		if p.Code != nil {
			params["code"] = strings.TrimSpace(*p.Code)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		if err := h.Call(ctx, "callback", params, nil); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		plugin.Forget()
		reply(w, pluginState(r.Context(), true))
	})
	mux.HandleFunc("POST /api/plugins/apikey", func(w http.ResponseWriter, r *http.Request) {
		pluginCall(w, r, "apiKey")
	})
	mux.HandleFunc("POST /api/plugins/signout", func(w http.ResponseWriter, r *http.Request) {
		pluginCall(w, r, "signOut")
	})
	mux.HandleFunc("PUT /api/plugins/checkin/{provider}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := plugin.SetCheckin(r.PathValue("provider"), req.On); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, pluginState(r.Context(), false))
	})
	mux.HandleFunc("POST /api/plugins/checkin/{provider}/run", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		// 签到失败也记进状态，页面照常刷新，从状态里读原因
		if _, err := plugin.Checkin(ctx, r.PathValue("provider")); err != nil {
			log.Println("morula:", err)
		}
		reply(w, pluginState(r.Context(), false))
	})
	mux.HandleFunc("GET /api/plugins/usage/{provider}", func(w http.ResponseWriter, r *http.Request) {
		h, err := plugin.Get(r.Context())
		if err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		var out json.RawMessage
		if err := h.Call(r.Context(), "usage", map[string]string{"provider": r.PathValue("provider")}, &out); err != nil {
			fail(w, http.StatusBadGateway, err.Error())
			return
		}
		if out == nil {
			out = json.RawMessage("null")
		}
		reply(w, out)
	})
}

// pluginCall 把请求体原样交给插件运行器的一个方法，成功后返回新的插件页内容。
func pluginCall(w http.ResponseWriter, r *http.Request, method string) {
	var p map[string]any
	json.NewDecoder(r.Body).Decode(&p)
	h, err := plugin.Get(r.Context())
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := h.Call(r.Context(), method, p, nil); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	plugin.Forget()
	reply(w, pluginState(r.Context(), true))
}

// testModel 让一个模型回一句话，看它能不能用。这是一次真实的模型调用，只在用户点「测试模型」时发。
// codex 订阅的模型用 codex 跑一次只读、不留会话的调用；插件和 API key 的模型经网关发一次 Chat Completions。
func testModel(ctx context.Context, source, model string) (string, error) {
	const prompt = "只回复两个字：收到"
	if source == "codex" {
		a, err := harness.Get("codex")
		if err != nil {
			return "", err
		}
		dir, err := os.MkdirTemp("", "morula-test-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		res, err := a.Run(ctx, harness.Spec{Dir: dir, Model: model, Access: harness.ReadOnly, Ephemeral: true, Prompt: prompt}, func(harness.Event) {})
		return strings.TrimSpace(res.Final), err
	}
	if gw == nil {
		return "", errors.New("模型转接没有启动")
	}
	body, _ := json.Marshal(map[string]any{"model": source + "/" + model, "stream": false, "max_tokens": 32,
		"messages": []map[string]string{{"role": "user", "content": prompt}}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.BaseURL()+"/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+gw.Key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d：%s", res.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &out) != nil || len(out.Choices) == 0 {
		return "", errors.New("回复格式不对：" + clipText(string(b), 300))
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

var bunVer struct {
	sync.Mutex
	bin, v string
}

// bunVersion 跑一次 bun --version，按路径记住结果。
func bunVersion(bin string) string {
	bunVer.Lock()
	defer bunVer.Unlock()
	if bunVer.bin != bin {
		out, _ := exec.Command(bin, "--version").Output()
		bunVer.bin, bunVer.v = bin, strings.TrimSpace(string(out))
	}
	return bunVer.v
}
