package gui

import (
	"encoding/json"
	"net/http"

	"morula/internal/skills"
	"morula/internal/work"
)

// staffRoutes：配置子 agent。助理按项目和本机 skill 提议，用户在配置页勾选后才改花名册。
func staffRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/skills", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"skills": skills.List(), "dirs": skills.Dirs()})
	})
	mux.HandleFunc("GET /api/staff", func(w http.ResponseWriter, r *http.Request) {
		res := map[string]any{"staff": eng.Staffing(), "skills": skills.List()}
		if a, _ := db.Assistant(); a != nil {
			res["assistant"] = map[string]string{"runtime": a.Runtime, "model": a.Model, "effort": a.Effort}
		}
		reply(w, res)
	})
	mux.HandleFunc("POST /api/staff/start", func(w http.ResponseWriter, r *http.Request) {
		if err := eng.StartStaff(nil); err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		reply(w, eng.Staffing())
	})
	mux.HandleFunc("POST /api/staff/apply", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Items []work.StaffItem `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "请求格式不对")
			return
		}
		errs, err := eng.ApplyStaff(req.Items)
		if err != nil {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		for i, it := range req.Items {
			if i < len(errs) && errs[i] == "" {
				describeIfSkills(it.Name)
			}
		}
		reply(w, map[string]any{"errors": errs})
	})
	mux.HandleFunc("POST /api/staff/discard", func(w http.ResponseWriter, r *http.Request) {
		eng.DiscardStaff()
		reply(w, eng.Staffing())
	})
}
