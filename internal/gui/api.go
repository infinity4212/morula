package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"morula/internal/chat"
	"morula/internal/env"
	"morula/internal/harness"
	"morula/internal/journal"
	"morula/internal/skills"
	"morula/internal/store"
	"morula/internal/subs"
	"morula/internal/update"
	"morula/internal/work"
)

var (
	db   *store.Store
	sess *chat.Session
	eng  *work.Engine
)

// setup 打开数据库，准备主对话和执行引擎。
func setup() error {
	if db != nil {
		return nil
	}
	var err error
	if db, err = store.Open(); err != nil {
		return err
	}
	sess = chat.New()
	eng = work.New(db, sess)
	// 第一次运行时放进助理；之后用户删掉就不再自动加回来。
	seedModel := "gpt-6-sol" // 读不到 codex 的模型列表时用；列表读得到时用列表里的第一个
	if ad, err := harness.Get("codex"); err == nil {
		if ms := ad.Models(); len(ms) > 0 {
			seedModel = ms[0].ID
		}
	}
	if db.Setting("assistant.seeded", "") == "" {
		if a, _ := db.Assistant(); a == nil {
			db.AddAgent(store.Agent{
				Name: "助理", Role: store.RoleAssistant, Runtime: "codex", Model: seedModel, Effort: "low",
				Instructions: "唯一入口。判断消息属于哪个项目，安排子 agent，检查汇报后向用户汇报。",
			})
		}
		db.SetSetting("assistant.seeded", "1")
	}
	// 审稿 agent 已移除（每条回复额外等待 18–50 秒），此处清理旧库中残留的记录。
	if db.Setting("review.removed", "") == "" {
		if a, _ := db.Agent("审稿"); a != nil && a.Role == "审阅" {
			db.RemoveAgent(a.Name)
		}
		db.SetSetting("review.removed", "1")
	}
	// 验收 agent 补入旧库：已有的库在下次启动时自动加入，模型跟助理一样。
	if db.Setting("accept.seeded", "") == "" {
		if a, _ := db.Agent(work.AcceptName); a == nil {
			acc := store.Agent{Name: work.AcceptName, Role: "验收", Runtime: "codex", Model: seedModel, Effort: "low",
				Instructions: work.AcceptInstructions}
			if as, _ := db.Assistant(); as != nil {
				acc.Runtime, acc.Model = as.Runtime, as.Model
			}
			db.AddAgent(acc)
		}
		db.SetSetting("accept.seeded", "1")
	}
	if err := db.Recover(); err != nil {
		return err
	}
	skills.ExtraDirs = projectSkillDirs
	setupPlugins()
	go sess.Watch(context.Background())
	go eng.Scan() // 本机会话先在后台扫好，首次引导问要不要整理时直接用
	go eng.Routines(context.Background())
	return nil
}

// projectSkillDirs 是各项目目录里的 .agents/skills 和 .codex/skills。
func projectSkillDirs() []string {
	ps, _ := db.Projects()
	var out []string
	for _, p := range ps {
		if p.Dir != "" {
			out = append(out, filepath.Join(p.Dir, ".agents", "skills"), filepath.Join(p.Dir, ".codex", "skills"))
		}
	}
	return out
}

// uiFont 是设置里选的字体大小，没选过时是 14。
func uiFont() int {
	n, err := strconv.Atoi(db.Setting("ui.font", ""))
	if err != nil || n < 13 || n > 16 {
		return 14
	}
	return n
}

// agentView 是 Agent 页的一张卡片：agent 本身、它名下任务的累计、介绍是否正在写。
type agentView struct {
	store.Agent
	Stats       store.AgentStat `json:"stats"`
	Describing  bool            `json:"describing"`
	DescribeErr string          `json:"describeErr,omitempty"`
	Turns       int             `json:"turns,omitempty"` // 助理今天接了几条用户消息
}

// describeIfSkills 在 agent 绑了 skill 时让助理在后台写介绍。没绑 skill 的卡片显示职责说明，不花 token。
func describeIfSkills(name string) {
	if a, _ := db.Agent(name); a != nil && len(a.Skills) > 0 {
		eng.DescribeLater(name)
	}
}

// todayTurns 数今天的对话记录里有几条用户消息。
func todayTurns() int {
	evs, _ := journal.Load(journal.Today())
	n := 0
	for _, e := range evs {
		if e["type"] == "user" {
			n++
		}
	}
	return n
}

func agents(w http.ResponseWriter) {
	list, err := db.Agents()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	stats, err := db.AgentStats()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 验收不在任务表里，按安排上记的结论数。
	if n, bad, err := db.AcceptStats(); err == nil && n > 0 {
		stats[work.AcceptName] = store.AgentStat{Done: n - bad, Missed: bad, Total: n}
	}
	out := make([]agentView, len(list))
	for i, a := range list {
		busy, why := eng.Describing(a.Name)
		out[i] = agentView{a, stats[a.Name], busy, why, 0}
		if a.Role == store.RoleAssistant {
			out[i].Turns = todayTurns()
		}
	}
	reply(w, out)
}

func routes(mux *http.ServeMux, version string) {
	pluginRoutes(mux)
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"version": version, "bundle": update.Bundle(), "feed": update.Feed()})
	})

	mux.HandleFunc("GET /api/agents", func(w http.ResponseWriter, r *http.Request) {
		agents(w)
	})
	mux.HandleFunc("POST /api/agents", func(w http.ResponseWriter, r *http.Request) {
		var a store.Agent
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		if err := db.AddAgent(a); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if strings.TrimSpace(a.Profile) == "" {
			describeIfSkills(strings.TrimSpace(a.Name))
		}
		agents(w)
	})
	mux.HandleFunc("PATCH /api/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		var p store.Patch
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		name := r.PathValue("name")
		old, _ := db.Agent(name)
		if err := db.UpdateAgent(name, p); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		// skill 换了、用户这次没自己改介绍时，让助理按新的 skill 重写。
		if old != nil && p.Skills != nil && p.Profile == nil && !slices.Equal(old.Skills, *p.Skills) {
			describeIfSkills(name)
		}
		agents(w)
	})
	mux.HandleFunc("POST /api/agents/{name}/describe", func(w http.ResponseWriter, r *http.Request) {
		if a, _ := db.Agent(r.PathValue("name")); a == nil {
			fail(w, http.StatusNotFound, "没有这个 Agent："+r.PathValue("name"))
			return
		}
		eng.DescribeLater(r.PathValue("name"))
		agents(w)
	})
	mux.HandleFunc("POST /api/agents/{name}/assistant", func(w http.ResponseWriter, r *http.Request) {
		if err := db.MakeAssistant(r.PathValue("name")); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		agents(w)
	})
	// 输入框旁的两个按钮：审批档位、助理会话的上下文用量。
	composer := func(w http.ResponseWriter) {
		reply(w, map[string]any{"tier": eng.Tier(), "context": eng.AssistantContext(), "limit": eng.CompactLimit(), "mix": eng.AssistantMix()})
	}
	mux.HandleFunc("GET /api/composer", func(w http.ResponseWriter, r *http.Request) { composer(w) })
	mux.HandleFunc("POST /api/composer/handoff", func(w http.ResponseWriter, r *http.Request) {
		if err := eng.HandoffNow(); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		composer(w)
	})
	mux.HandleFunc("PUT /api/composer/tier", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tier string `json:"tier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		if err := eng.SetTier(in.Tier); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		composer(w)
	})
	mux.HandleFunc("GET /api/harnesses", func(w http.ResponseWriter, r *http.Request) {
		list := chosen()
		if r.URL.Query().Has("cli") { // 要跑 --version、查 npm，只有 Harness 一页要。这一页列全部工具，没勾的也列
			list = harness.List(store.Runtimes)
			cs := harness.CLIs(r.Context(), store.Runtimes)
			for i := range list {
				if c, ok := cs[list[i].Name]; ok {
					list[i].CLI = &c
				}
			}
		}
		reply(w, list)
	})
	mux.HandleFunc("POST /api/harnesses/{name}/update", func(w http.ResponseWriter, r *http.Request) {
		out, err := harness.UpdateCLI(r.Context(), r.PathValue("name"))
		if err != nil {
			fail(w, http.StatusBadGateway, strings.TrimSpace(err.Error()+"\n"+lastLines(out, 6)))
			return
		}
		reply(w, map[string]any{"output": lastLines(out, 6)})
	})
	mux.HandleFunc("POST /api/harnesses/{name}/install", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Via string `json:"via"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		name := r.PathValue("name")
		out, err := harness.InstallCLI(r.Context(), name, in.Via)
		env.FixPath() // 安装脚本可能把命令放进新的目录，重新读一遍登录 shell 的 PATH
		harness.ForgetSetup()
		if err != nil {
			fail(w, http.StatusBadGateway, strings.TrimSpace(err.Error()+"\n"+lastLines(out, 6)))
			return
		}
		if on := db.Tools(); !slices.Contains(on, name) { // 装好的工具默认勾上
			db.SetTools(append(on, name))
		}
		reply(w, map[string]any{"output": lastLines(out, 6)})
	})
	mux.HandleFunc("GET /api/flows", func(w http.ResponseWriter, r *http.Request) {
		project, _ := strconv.ParseInt(r.URL.Query().Get("project"), 10, 64)
		reply(w, eng.Flows(project))
	})
	mux.HandleFunc("DELETE /api/agents/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := db.RemoveAgent(r.PathValue("name")); err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		agents(w)
	})

	mux.HandleFunc("GET /api/accounts", func(w http.ResponseWriter, r *http.Request) {
		list, at, errs := subs.Accounts(r.URL.Query().Has("refresh"))
		res := map[string]any{"accounts": list, "readAt": at.Format(time.RFC3339)}
		if len(errs) > 0 {
			res["error"] = strings.Join(errs, "\n")
		}
		reply(w, res)
	})

	// 主对话
	mux.HandleFunc("GET /api/chat/poll", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		epoch, _ := strconv.ParseInt(r.URL.Query().Get("epoch"), 10, 64)
		// epoch 对不上（首次打开或会话已重置）时立即返回全部事件，前端清空重画。
		wait := 25 * time.Second
		if r.URL.Query().Has("nowait") {
			wait = 0
		}
		e, list, busy := sess.Poll(r.Context(), epoch, after, wait)
		reply(w, map[string]any{"epoch": e, "events": list, "busy": busy, "queue": sess.Queue(), "runs": len(eng.Running()), "direct": eng.DirectState()})
	})
	// 和某个 agent 直接对话：agent 填名字开始，填空结束（结束时它写纪要进项目记忆）
	mux.HandleFunc("POST /api/chat/direct", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Agent   string `json:"agent"`
			Project int64  `json:"project"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var err error
		if strings.TrimSpace(req.Agent) == "" {
			err = eng.EndDirect()
		} else {
			_, err = eng.StartDirect(strings.TrimSpace(req.Agent), req.Project, "")
		}
		if err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, map[string]any{"ok": true, "direct": eng.DirectState()})
	})
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text    string   `json:"text"`
			Files   []string `json:"files"`   // POST /api/files 返回的 id
			Rewind  string   `json:"rewind"`  // 编辑一条已发的消息时填它的 id：先撤回它和之后的对话，再发新内容
			Project int64    `json:"project"` // 在项目页里发的：这条消息属于这个项目，不用助理判断归属
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" && len(req.Files) == 0 {
			fail(w, http.StatusBadRequest, "消息是空的")
			return
		}
		files, err := attachments(req.Files)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Rewind != "" {
			at, err := sess.Rewind(req.Rewind)
			if err != nil {
				fail(w, http.StatusConflict, err.Error())
				return
			}
			eng.DropFlowsSince(at)
			eng.ResetThread() // 助理的会话里还留着撤回的内容，下一条消息新开
		}
		if req.Project != 0 {
			if p, _ := db.Project(req.Project); p == nil {
				fail(w, http.StatusNotFound, "没有这个项目")
				return
			}
		}
		queued, err := sess.SendTo(strings.TrimSpace(req.Text), files, req.Project)
		if err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true, "queued": queued})
	})
	// 排队的消息：补充到当前这条（merge）、单独并行（parallel）、不发了（drop）
	mux.HandleFunc("POST /api/chat/queue", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID  string `json:"id"`
			How string `json:"how"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if err := sess.Dequeue(req.ID, req.How); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/chat/permission", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
			chat.Answer
		}
		json.NewDecoder(r.Body).Decode(&req)
		if err := sess.Answer(req.ID, req.Answer); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/chat/clear", func(w http.ResponseWriter, r *http.Request) {
		if err := sess.Clear(); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		eng.ClearFlows()
		eng.ResetThread()
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/chat/rewind", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		at, err := sess.Rewind(req.ID)
		if err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		eng.DropFlowsSince(at)
		eng.ResetThread() // 助理的会话里还留着撤回的内容，下一条消息新开
		reply(w, map[string]bool{"ok": true})
	})
	fileRoutes(mux)
	openRoutes(mux)
	// 停止：停掉助理这一轮。带 runs=1 时（对话页的停止按钮）一并停掉后台在跑的全部安排；
	// 撤回、编辑前的停止不带，只停助理。
	// 输入框的停止键只停最后开始的那一条消息；例行事项、并行的其他消息和派出的活照跑，在右栏「进行中」各自停。
	mux.HandleFunc("POST /api/chat/stop", func(w http.ResponseWriter, r *http.Request) {
		left := sess.StopTurn("")
		reply(w, map[string]any{"ok": true, "busy": left > 0})
	})
	// 停一条流程：停它的那条消息的处理，和它派出的那次安排。
	mux.HandleFunc("POST /api/flows/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
		for _, f := range eng.Flows(0) {
			if f.ID != id {
				continue
			}
			if f.Turn != "" {
				sess.StopTurn(f.Turn)
			}
			if f.Run != 0 {
				eng.StopRun(f.Run) // 安排已经结束时返回错误，不用管
			}
			reply(w, map[string]bool{"ok": true})
			return
		}
		fail(w, http.StatusNotFound, "找不到这条流程")
	})

	// 历史：前端往上翻时按天加载；记忆：用户可以查看和修改助理的长期记忆。
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		// 清空过对话页时，清空那天以前的日子不再给对话页看
		list := []map[string]any{}
		from := journal.LastClear()
		for _, d := range journal.Days() {
			if d < from {
				continue
			}
			sum, ok := journal.Summary(d)
			list = append(list, map[string]any{"day": d, "summary": sum, "hasSummary": ok})
		}
		reply(w, map[string]any{"today": journal.Today(), "days": list})
	})
	mux.HandleFunc("GET /api/history/{day}", func(w http.ResponseWriter, r *http.Request) {
		evs, err := journal.Load(r.PathValue("day"))
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, journal.Visible(evs))
	})
	mux.HandleFunc("GET /api/memory", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]string{"text": journal.Memory(), "path": journal.MemoryPath()})
	})
	mux.HandleFunc("PUT /api/memory", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		if err := journal.SaveMemory(req.Text); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})

	// 设置：任务树的限制和新项目的位置。
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		reply(w, eng.Limits())
	})
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var l work.Limits
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		if err := eng.SetLimits(l); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, eng.Limits())
	})

	// 界面偏好：字体大小（px），13 到 16，默认 14。
	mux.HandleFunc("GET /api/ui", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"font": uiFont()})
	})
	mux.HandleFunc("POST /api/ui", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Font int `json:"font"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Font < 13 || req.Font > 16 {
			fail(w, http.StatusBadRequest, "字体大小要在 13 到 16 之间")
			return
		}
		db.SetSetting("ui.font", strconv.Itoa(req.Font))
		reply(w, map[string]any{"font": uiFont()})
	})

	// 审批与通知：危险操作清单（数据目录里的 approval-rules.txt）和两类系统通知的开关。
	mux.HandleFunc("GET /api/approval", func(w http.ResponseWriter, r *http.Request) {
		reply(w, approvalState())
	})
	mux.HandleFunc("PUT /api/approval", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Rules  *[]string `json:"rules"`
			Ask    *bool     `json:"ask"`
			Report *bool     `json:"report"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		if req.Rules != nil {
			if err := work.SaveDanger(*req.Rules); err != nil {
				fail(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		for k, v := range map[string]*bool{"notify.ask": req.Ask, "notify.report": req.Report} {
			if v != nil {
				db.SetSetting(k, map[bool]string{true: "1", false: "0"}[*v])
			}
		}
		reply(w, approvalState())
	})

	projectRoutes(mux)
	routineRoutes(mux)
	onboardRoutes(mux)
	readyRoutes(mux)
	toolRoutes(mux)
	staffRoutes(mux)

	mux.HandleFunc("GET /api/update", func(w http.ResponseWriter, r *http.Request) {
		rel, err := update.Latest()
		if err != nil {
			reply(w, map[string]any{"available": false, "error": err.Error()})
			return
		}
		reply(w, map[string]any{
			"available": update.Newer(rel.Version, version),
			"version":   rel.Version,
			"notes":     rel.Notes,
		})
	})
	mux.HandleFunc("POST /api/update", func(w http.ResponseWriter, r *http.Request) {
		rel, err := update.Latest()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !update.Newer(rel.Version, version) {
			fail(w, http.StatusConflict, "已经是最新版本")
			return
		}
		if err := update.Apply(rel, update.Bundle()); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]string{"version": rel.Version})
		// 先让页面收到回复，再退出；relaunch 脚本会在退出后打开新版。
		go func() {
			time.Sleep(300 * time.Millisecond)
			if app != nil {
				app.Quit()
			}
		}()
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func approvalState() map[string]any {
	rules, err := work.Danger()
	path, _ := work.DangerPath()
	out := map[string]any{"path": path, "rules": rules, "ask": notifyOn("ask"), "report": notifyOn("report")}
	if err != nil {
		out["error"] = err.Error()
	}
	return out
}

// lastLines 取 s 的最后 n 行，命令输出太长时只留结尾。
func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}
