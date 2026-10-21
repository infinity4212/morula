package store

import (
	"database/sql"
	"errors"
	"strings"
)

// 例行事项：到点由助理照原话处理一件事，做完的结果进对话。
//
//	routines      一件例行事项：原话、哪几天几点、交给谁、放进哪个项目、开着没有
//	routine_runs  每到一次点的记录：什么时候发出、做了多久、结果
const routineTables = `
CREATE TABLE IF NOT EXISTS routines (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	prompt     TEXT NOT NULL,
	days       INTEGER NOT NULL DEFAULT 127,
	at         TEXT NOT NULL,
	agent      TEXT NOT NULL DEFAULT '',
	project_id INTEGER NOT NULL DEFAULT 0,
	enabled    INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS routine_runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	routine_id INTEGER NOT NULL REFERENCES routines(id) ON DELETE CASCADE,
	slot       TEXT NOT NULL,
	turn       TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'working',
	note       TEXT NOT NULL DEFAULT '',
	started_at TEXT NOT NULL,
	ended_at   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS routine_runs_routine ON routine_runs(routine_id);`

// Routine 是一件例行事项。Days 是星期几的位：第 0 位是星期日，127 表示每天。At 是 "HH:MM"。
type Routine struct {
	ID        int64       `json:"id"`
	Name      string      `json:"name"`
	Prompt    string      `json:"prompt"`
	Days      int         `json:"days"`
	At        string      `json:"at"`
	Agent     string      `json:"agent"`
	ProjectID int64       `json:"projectId"`
	Enabled   bool        `json:"enabled"`
	CreatedAt string      `json:"createdAt"`
	UpdatedAt string      `json:"updatedAt"`
	Last      *RoutineRun `json:"last,omitempty"`
}

// RoutineRun 是到一次点的记录。Status 是 working、done、stopped、failed 之一；Slot 是这次对应的时间点（RFC3339）。
type RoutineRun struct {
	ID        int64  `json:"id"`
	RoutineID int64  `json:"routineId"`
	Name      string `json:"name,omitempty"`
	Slot      string `json:"slot"`
	Turn      string `json:"turn"`
	Status    string `json:"status"`
	Note      string `json:"note"`
	StartedAt string `json:"startedAt"`
	EndedAt   string `json:"endedAt"`
}

const routineCols = `id, name, prompt, days, at, agent, project_id, enabled, created_at, updated_at`

func scanRoutine(sc interface{ Scan(...any) error }) (*Routine, error) {
	r := &Routine{}
	var en int
	if err := sc.Scan(&r.ID, &r.Name, &r.Prompt, &r.Days, &r.At, &r.Agent, &r.ProjectID, &en, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Enabled = en != 0
	return r, nil
}

func checkRoutine(r *Routine) error {
	r.Name, r.Prompt, r.At = strings.TrimSpace(r.Name), strings.TrimSpace(r.Prompt), strings.TrimSpace(r.At)
	switch {
	case r.Name == "":
		return errors.New("名字不能为空")
	case r.Prompt == "":
		return errors.New("要助理做什么不能为空")
	case r.Days&127 == 0:
		return errors.New("至少选一天")
	}
	if len(r.At) != 5 || r.At[2] != ':' || r.At[:2] > "23" || r.At[3:] > "59" || strings.Trim(r.At[:2]+r.At[3:], "0123456789") != "" {
		return errors.New("时间要写成 08:30 这样")
	}
	r.Days &= 127
	return nil
}

// Routines 列出全部例行事项，每件带上最后一次的记录。
func (s *Store) Routines() ([]Routine, error) {
	rows, err := s.db.Query(`SELECT ` + routineCols + ` FROM routines ORDER BY id`)
	if err != nil {
		return nil, err
	}
	out := []Routine{}
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, *r)
	}
	rows.Close()
	for i := range out {
		if runs, _ := s.RoutineRuns(out[i].ID, 1); len(runs) > 0 {
			out[i].Last = &runs[0]
		}
	}
	return out, rows.Err()
}

func (s *Store) Routine(id int64) (*Routine, error) {
	r, err := scanRoutine(s.db.QueryRow(`SELECT `+routineCols+` FROM routines WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *Store) AddRoutine(r Routine) (*Routine, error) {
	if err := checkRoutine(&r); err != nil {
		return nil, err
	}
	t := now()
	res, err := s.db.Exec(`INSERT INTO routines (name, prompt, days, at, agent, project_id, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Prompt, r.Days, r.At, strings.TrimSpace(r.Agent), r.ProjectID, b2i(r.Enabled), t, t)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Routine(id)
}

// UpdateRoutine 整体替换一件例行事项的设置。
func (s *Store) UpdateRoutine(r Routine) (*Routine, error) {
	if err := checkRoutine(&r); err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`UPDATE routines SET name = ?, prompt = ?, days = ?, at = ?, agent = ?, project_id = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		r.Name, r.Prompt, r.Days, r.At, strings.TrimSpace(r.Agent), r.ProjectID, b2i(r.Enabled), now(), r.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errors.New("没有这件例行事项")
	}
	return s.Routine(r.ID)
}

func (s *Store) RemoveRoutine(id int64) error {
	_, err := s.db.Exec(`DELETE FROM routines WHERE id = ?`, id)
	return err
}

// AddRoutineRun 记下到了一次点。同一件事项的同一个时间点只记一次，记过时返回 0。
func (s *Store) AddRoutineRun(rid int64, slot string) (int64, error) {
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM routine_runs WHERE routine_id = ? AND slot = ?`, rid, slot).Scan(&n)
	if n > 0 {
		return 0, nil
	}
	res, err := s.db.Exec(`INSERT INTO routine_runs (routine_id, slot, started_at) VALUES (?, ?, ?)`, rid, slot, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RemoveRoutineRun 删掉一条没发出去的记录。
func (s *Store) RemoveRoutineRun(id int64) error {
	_, err := s.db.Exec(`DELETE FROM routine_runs WHERE id = ?`, id)
	return err
}

// SetRoutineTurn 记下这次发给助理的那条消息，前端按它跳到对话里。
func (s *Store) SetRoutineTurn(id int64, turn string) error {
	_, err := s.db.Exec(`UPDATE routine_runs SET turn = ? WHERE id = ?`, turn, id)
	return err
}

// EndRoutineRun 记下这次的结果。
func (s *Store) EndRoutineRun(id int64, status, note string) error {
	_, err := s.db.Exec(`UPDATE routine_runs SET status = ?, note = ?, ended_at = ? WHERE id = ?`, status, note, now(), id)
	return err
}

// RoutineRuns 是一件事项（rid 为 0 时是全部事项）最近的 limit 次记录，新的在前。
func (s *Store) RoutineRuns(rid int64, limit int) ([]RoutineRun, error) {
	q := `SELECT r.id, r.routine_id, COALESCE(t.name, ''), r.slot, r.turn, r.status, r.note, r.started_at, r.ended_at
		FROM routine_runs r LEFT JOIN routines t ON t.id = r.routine_id`
	args := []any{}
	if rid != 0 {
		q += ` WHERE r.routine_id = ?`
		args = append(args, rid)
	}
	q += ` ORDER BY r.id DESC LIMIT ?`
	rows, err := s.db.Query(q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RoutineRun{}
	for rows.Next() {
		var x RoutineRun
		if err := rows.Scan(&x.ID, &x.RoutineID, &x.Name, &x.Slot, &x.Turn, &x.Status, &x.Note, &x.StartedAt, &x.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// EndStaleRoutineRuns 把上次退出时还没结束的记录标成停止：morula 重启后没人在等它们了。
func (s *Store) EndStaleRoutineRuns() error {
	_, err := s.db.Exec(`UPDATE routine_runs SET status = 'stopped', note = 'morula 退出时还没做完', ended_at = ? WHERE status = 'working'`, now())
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
