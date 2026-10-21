package gui

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"

	"morula/internal/harness"
	"morula/internal/skills"
	"morula/internal/work"
)

// onboardRoutes：首次使用的进度，以及整理本机会话。助理页的快速上手卡片按这里的进度打勾，之后从设置进整理页。
func onboardRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/onboard", func(w http.ResponseWriter, r *http.Request) {
		res := map[string]any{"asked": db.Setting("onboard.asked", "") != "", "intro": db.Setting("onboard.intro", "") != "", "hidden": db.Setting("onboard.hidden", "") != "", "organize": eng.Organizing()}
		if r.URL.Query().Has("asked") {
			reply(w, res) // 只问要不要弹首次提示时不扫会话
			return
		}
		if r.URL.Query().Has("rescan") {
			eng.Scan()
		}
		pending, skipped, err := eng.Pending()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		on := db.Tools() // 没勾的工具的会话不列
		pending = slices.DeleteFunc(pending, func(s harness.Local) bool { return !slices.Contains(on, s.Harness) })
		by := map[string]int{}
		for _, s := range pending {
			by[s.Harness]++
		}
		if r.URL.Query().Has("list") { // 启动流程第四步：最近的排前面，页面列前 5 个，「查看全部」列全部
			slices.SortFunc(pending, func(a, b harness.Local) int { return b.Updated.Compare(a.Updated) })
			res["sessions"] = pending
		}
		res["pending"], res["skipped"], res["byHarness"] = len(pending), skipped, by
		// 每个来源一张卡片：读的位置、新会话数、已整理或跳过的数、上次整理的时间
		all := map[string]int{}
		for _, s := range eng.Scanned() {
			all[s.Harness]++
		}
		last, _ := db.LastImported()
		// 一个工具读几个目录时（WorkBuddy 读 ~/.codebuddy 和 ~/.workbuddy）合成一张卡片，数字按工具算
		var srcs []map[string]any
		at := map[string]int{}
		for _, s := range harness.SessionSources() {
			if !slices.Contains(on, s.Name) {
				continue
			}
			if i, ok := at[s.Name]; ok {
				srcs[i]["dir"] = srcs[i]["dir"].(string) + "、" + s.Dir
				srcs[i]["found"] = srcs[i]["found"].(bool) || s.Found
				continue
			}
			at[s.Name] = len(srcs)
			srcs = append(srcs, map[string]any{"name": s.Name, "dir": s.Dir, "found": s.Found,
				"pending": by[s.Name], "skipped": all[s.Name] - by[s.Name], "last": last[s.Name]})
		}
		res["sources"] = srcs
		home, _ := os.UserHomeDir()
		var dirs []string
		for _, d := range skills.Dirs()[:3] { // 全局的三个；项目目录里的不列
			if home != "" && strings.HasPrefix(d, home+"/") {
				d = "~" + strings.TrimPrefix(d, home)
			}
			dirs = append(dirs, d)
		}
		res["skillDirs"] = dirs
		if a, _ := db.Assistant(); a != nil {
			res["assistant"] = a.Name + "（" + a.Runtime + " · " + a.Model + "）"
		}
		if r.URL.Query().Has("cost") && len(pending) > 0 {
			if c, err := eng.OrganizeCost(); err == nil {
				res["cost"] = c
			}
		}
		reply(w, res)
	})
	mux.HandleFunc("POST /api/onboard/start", func(w http.ResponseWriter, r *http.Request) {
		db.SetSetting("onboard.asked", "1")
		var req struct {
			Only []string `json:"only"`
			IDs  []string `json:"ids"` // 「harness/会话 id」
		}
		json.NewDecoder(r.Body).Decode(&req) // 没有请求体时整理全部来源
		if len(req.Only) == 0 {
			req.Only = db.Tools() // 没勾的工具的会话不整理
		}
		if err := eng.StartOrganize(req.Only, req.IDs); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, eng.Organizing())
	})
	// 首次引导里选「全新开始」：记下回答过，以后只从设置进整理页。
	mux.HandleFunc("POST /api/onboard/skip", func(w http.ResponseWriter, r *http.Request) {
		db.SetSetting("onboard.asked", "1")
		reply(w, map[string]any{"asked": true})
	})
	// 助理页的快速上手卡片点了关闭：记下，以后助理在线时不再显示。
	mux.HandleFunc("POST /api/onboard/hide", func(w http.ResponseWriter, r *http.Request) {
		db.SetSetting("onboard.hidden", "1")
		reply(w, map[string]any{"hidden": true})
	})
	mux.HandleFunc("POST /api/onboard/apply", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Items []work.Proposal `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		errs, err := eng.ApplyOrganize(req.Items)
		if err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, map[string]any{"errors": errs})
	})
	// 首次引导第 3 步点了「整理」但没有要整理的会话：直接读最近的对话记录。只做一次。
	mux.HandleFunc("POST /api/onboard/brief", func(w http.ResponseWriter, r *http.Request) {
		db.SetSetting("onboard.asked", "1")
		eng.StartBrief()
		reply(w, map[string]any{"asked": true})
	})
	mux.HandleFunc("POST /api/onboard/discard", func(w http.ResponseWriter, r *http.Request) {
		eng.DiscardOrganize()
		reply(w, eng.Organizing())
	})
}
