package gui

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// projectRoutes：项目页要的数据。轨迹不在 morula 里存，按任务的会话 id 去 harness 读。
func projectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		def := eng.DefaultProject()
		list, err := db.Projects()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		// 每个项目附上最近一次安排的状态，列表上显示「在跑」。
		type item struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			Summary   string `json:"summary"`
			Dir       string `json:"dir"`
			UpdatedAt string `json:"updatedAt"`
			Status    string `json:"status"`
			Default   bool   `json:"default"`
			Waiting   bool   `json:"waiting"` // 有要用户批准的任务
			CreatedAt string `json:"createdAt"`
			Runs      int    `json:"runs"` // 安排过几次，最多记到 1；侧栏按它判断「新建」
		}
		waiting, _ := db.WaitingProjects()
		out := []item{}
		for _, p := range list {
			it := item{ID: p.ID, Name: p.Name, Summary: p.Summary, Dir: p.Dir, UpdatedAt: p.UpdatedAt, Default: def != nil && def.ID == p.ID,
				Waiting: waiting[p.ID], CreatedAt: p.CreatedAt}
			if runs, _ := db.Runs(p.ID, 1); len(runs) > 0 {
				it.Status = runs[0].Status
				it.Runs = 1
			}
			out = append(out, it)
		}
		reply(w, out)
	})
	mux.HandleFunc("POST /api/projects", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name    string `json:"name"`
			Summary string `json:"summary"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
			fail(w, http.StatusBadRequest, "项目名不能为空")
			return
		}
		p, err := eng.CreateProject(req.Name, req.Summary)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, p)
	})
	// 设成默认项目：闲聊和零散小事归到它。
	mux.HandleFunc("POST /api/projects/{id}/default", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		if p, _ := db.Project(id); p == nil {
			fail(w, http.StatusNotFound, "没有这个项目")
			return
		}
		if err := eng.SetDefaultProject(id); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r)
		p, err := db.Project(id)
		if !ok || err != nil || p == nil {
			fail(w, http.StatusNotFound, "没有这个项目")
			return
		}
		runs, err := db.Runs(id, 20)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		running := map[int64]bool{}
		for _, rid := range eng.Running() {
			running[rid] = true
		}
		live := map[string]string{}
		for k, v := range eng.Live() {
			live[strconv.FormatInt(k, 10)] = v
		}
		vers, err := db.MemoryVersions(id)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		def := eng.DefaultProject()
		reply(w, map[string]any{"project": p, "runs": runs, "live": live, "running": running, "default": def != nil && def.ID == id,
			"memoryVersions": vers})
	})
	// 把项目记忆退回到一个旧版本。
	mux.HandleFunc("POST /api/projects/{id}/memory/restore", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		var req struct {
			Version int64 `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		if err := db.RestoreMemory(id, req.Version); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("PATCH /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		var req struct {
			Name    *string `json:"name"`
			Summary *string `json:"summary"`
			Memory  *string `json:"memory"`
			Dir     *string `json:"dir"`
			Owner   *string `json:"owner"` // 项目负责人的名字；空串改回由助理兼任
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		if req.Dir != nil {
			if fi, err := os.Stat(*req.Dir); err != nil || !fi.IsDir() {
				fail(w, http.StatusBadRequest, "找不到这个文件夹："+*req.Dir)
				return
			}
		}
		if req.Name != nil {
			if _, err := eng.RenameProject(r.Context(), id, *req.Name); err != nil {
				fail(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if req.Summary != nil || req.Memory != nil || req.Dir != nil {
			if err := db.UpdateProject(id, req.Summary, req.Memory, req.Dir); err != nil {
				fail(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if req.Owner != nil {
			if err := eng.SetOwner(id, *req.Owner); err != nil {
				fail(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		reply(w, map[string]bool{"ok": true})
	})
	// 撤销一次合并：对话里合并记录上的撤销按钮。
	mux.HandleFunc("POST /api/merges/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		if _, _, err := eng.UndoMerge(r.Context(), id); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	// 删除项目只删 morula 里的记录，工作目录里的文件不动。
	mux.HandleFunc("DELETE /api/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		if err := db.RemoveProject(id); err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/runs/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		if err := eng.StopRun(id); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/tasks/{id}/trajectory", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		steps, err := eng.Trajectory(id)
		if err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		reply(w, steps)
	})
}
