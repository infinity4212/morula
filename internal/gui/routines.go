package gui

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"morula/internal/journal"
	"morula/internal/store"
	"morula/internal/work"
)

// routineRoutes：例行事项页和侧栏的搜索。
func routineRoutes(mux *http.ServeMux) {
	type item struct {
		store.Routine
		Next string `json:"next"` // 下一次到点的时间（RFC3339）；关着或没选日子时为空
	}
	list := func() ([]item, error) {
		rs, err := db.Routines()
		if err != nil {
			return nil, err
		}
		out := []item{}
		now := time.Now()
		for _, r := range rs {
			it := item{Routine: r}
			if t := work.NextSlot(r, now); r.Enabled && !t.IsZero() {
				it.Next = t.Format(time.RFC3339)
			}
			out = append(out, it)
		}
		return out, nil
	}
	sendList := func(w http.ResponseWriter) {
		l, err := list()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, l)
	}
	mux.HandleFunc("GET /api/routines", func(w http.ResponseWriter, r *http.Request) { sendList(w) })
	mux.HandleFunc("POST /api/routines", func(w http.ResponseWriter, r *http.Request) {
		var in store.Routine
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "格式不对")
			return
		}
		if _, err := db.AddRoutine(in); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		sendList(w)
	})
	mux.HandleFunc("PUT /api/routines/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		var in store.Routine
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			fail(w, http.StatusBadRequest, "格式不对")
			return
		}
		in.ID = id
		if _, err := db.UpdateRoutine(in); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		sendList(w)
	})
	mux.HandleFunc("DELETE /api/routines/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		if err := db.RemoveRoutine(id); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		sendList(w)
	})
	// 现在跑一次：不等到点，马上发给助理。助理正忙时拒绝。
	mux.HandleFunc("POST /api/routines/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		id, _ := pathID(r)
		rt, _ := db.Routine(id)
		if rt == nil {
			fail(w, http.StatusNotFound, "没有这件例行事项")
			return
		}
		if _, ok := eng.RunRoutine(*rt, "manual-"+time.Now().Format(time.RFC3339Nano)); !ok {
			fail(w, http.StatusConflict, "助理正在处理别的消息，等它空下来再试")
			return
		}
		sendList(w)
	})
	mux.HandleFunc("GET /api/routines/runs", func(w http.ResponseWriter, r *http.Request) {
		runs, err := db.RoutineRuns(0, 100)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		reply(w, runs)
	})

	// 侧栏的搜索：项目（名字、摘要、项目记忆）、安排的原话、和助理的对话。
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		type projHit struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			Hit  string `json:"hit"` // 名字以外命中的那段话；名字命中时为空
		}
		out := struct {
			Projects []projHit      `json:"projects"`
			Runs     []store.RunHit `json:"runs"`
			Chats    []journal.Hit  `json:"chats"`
		}{[]projHit{}, []store.RunHit{}, []journal.Hit{}}
		if q == "" {
			reply(w, out)
			return
		}
		lq := strings.ToLower(q)
		ps, _ := db.Projects()
		for _, p := range ps {
			switch {
			case strings.Contains(strings.ToLower(p.Name), lq):
				out.Projects = append(out.Projects, projHit{ID: p.ID, Name: p.Name})
			case strings.Contains(strings.ToLower(p.Summary), lq):
				out.Projects = append(out.Projects, projHit{ID: p.ID, Name: p.Name, Hit: around(p.Summary, lq)})
			default:
				// 列表不带项目记忆，单独取一次
				if full, _ := db.Project(p.ID); full != nil && strings.Contains(strings.ToLower(full.Memory), lq) {
					out.Projects = append(out.Projects, projHit{ID: p.ID, Name: p.Name, Hit: around(full.Memory, lq)})
				}
			}
		}
		out.Runs, _ = db.SearchRuns(q, 8)
		out.Chats = journal.Search(q, 12)
		reply(w, out)
	})
}

// around 是 s 里第一处命中 lq 附近的一段话，前后各留一些字。
func around(s, lq string) string {
	rs := []rune(s)
	i := strings.Index(strings.ToLower(s), lq)
	if i < 0 {
		return ""
	}
	at := len([]rune(s[:i]))
	from, to := max(0, at-20), min(len(rs), at+len([]rune(lq))+40)
	t := strings.Join(strings.Fields(string(rs[from:to])), " ")
	if from > 0 {
		t = "…" + t
	}
	if to < len(rs) {
		t += "…"
	}
	return t
}
