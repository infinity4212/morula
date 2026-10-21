package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// 项目、派单和任务树。
//
//	projects  一个项目：项目摘要（助理常驻持有）、项目记忆（选定项目后才加载）、工作目录、项目负责人
//	runs      用户的一条消息被归入项目后，助理做的一次安排
//	tasks     一次安排里的任务，按 parent_id 组成任务树；每个节点由一个子 agent 执行
//	imported  首次整理时看过的本机 harness 会话；project_id 为 0 表示判为闲聊，没归项目
//	memory_versions  项目记忆每次被改之前的内容，每个项目留最近 memoryKeep 条
//	merges    一次合并挪了哪些安排和整理记录、往目标项目记忆里加了什么，撤销合并时照着挪回去
const projectTables = `
CREATE TABLE IF NOT EXISTS projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	summary    TEXT NOT NULL DEFAULT '',
	memory     TEXT NOT NULL DEFAULT '',
	dir        TEXT NOT NULL DEFAULT '',
	owner      TEXT NOT NULL DEFAULT '',
	merged_into INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	request    TEXT NOT NULL,
	plan       TEXT NOT NULL DEFAULT '',
	reply      TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'working',
	redo_of    INTEGER NOT NULL DEFAULT 0,
	rejects    INTEGER NOT NULL DEFAULT 0,
	verdict    TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	run_id      INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	parent_id   INTEGER REFERENCES tasks(id) ON DELETE CASCADE,
	depth       INTEGER NOT NULL DEFAULT 1,
	title       TEXT NOT NULL,
	brief       TEXT NOT NULL DEFAULT '',
	agent       TEXT NOT NULL DEFAULT '',
	check_cmd   TEXT NOT NULL DEFAULT '',
	effort      TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL DEFAULT 'waiting',
	runtime     TEXT NOT NULL DEFAULT '',
	model       TEXT NOT NULL DEFAULT '',
	session     TEXT NOT NULL DEFAULT '',
	report      TEXT NOT NULL DEFAULT '',
	check_out   TEXT NOT NULL DEFAULT '',
	feedback    TEXT NOT NULL DEFAULT '',
	approval    TEXT NOT NULL DEFAULT '',
	attempts    INTEGER NOT NULL DEFAULT 0,
	tokens      INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL,
	updated_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_run ON tasks(run_id);
CREATE INDEX IF NOT EXISTS runs_project ON runs(project_id);
CREATE TABLE IF NOT EXISTS imported (
	harness    TEXT NOT NULL,
	session    TEXT NOT NULL,
	project_id INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	PRIMARY KEY (harness, session)
);
CREATE TABLE IF NOT EXISTS memory_versions (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	memory     TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS memory_versions_project ON memory_versions(project_id);
CREATE TABLE IF NOT EXISTS merges (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	src        INTEGER NOT NULL,
	dst        INTEGER NOT NULL,
	runs       TEXT NOT NULL DEFAULT '[]',
	imported   TEXT NOT NULL DEFAULT '[]',
	added      TEXT NOT NULL DEFAULT '',
	undone_at  TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL
);`

type Project struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Summary    string `json:"summary"`
	Memory     string `json:"memory,omitempty"`
	Dir        string `json:"dir"`
	Owner      string `json:"owner"`                // 项目负责人的名字；空表示由助理兼任
	MergedInto int64  `json:"mergedInto,omitempty"` // 并进了哪个项目；0 表示没并
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

type Run struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"projectId"`
	Request   string `json:"request"`
	Plan      string `json:"plan"`
	Reply     string `json:"reply"`
	Status    string `json:"status"`
	// 验收没通过、打回给助理重新安排时：RedoOf 是这串安排的第一次，Rejects 是这次之前已被打回几次。
	RedoOf    int64  `json:"redoOf"`
	Rejects   int    `json:"rejects"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Tasks     []Task `json:"tasks"`
}

// Task 的 Status：waiting 等待、working 工作中、checking 检查中、approval 待批准、
// done 完成、rejected 打回（等重做）、failed 失败。
type Task struct {
	ID        int64  `json:"id"`
	RunID     int64  `json:"runId"`
	ParentID  int64  `json:"parentId"` // 0 表示顶层节点，父节点是助理
	Depth     int    `json:"depth"`
	Title     string `json:"title"`
	Brief     string `json:"brief"`
	Agent     string `json:"agent"`
	Check     string `json:"check"`  // 机器检查命令，在项目目录里用 sh -c 跑；空表示没有
	Effort    string `json:"effort"` // 思考强度：派活时助理定的，开工后是实际用的；空表示用执行者自己的设置
	Status    string `json:"status"`
	Runtime   string `json:"runtime"`
	Model     string `json:"model"`
	Session   string `json:"session"` // harness 的会话 id，用来读轨迹和续跑
	Report    string `json:"report"`
	CheckOut  string `json:"checkOut"`
	Feedback  string `json:"feedback"`
	Approval  string `json:"approval"`
	Attempts  int    `json:"attempts"`
	Tokens    int    `json:"tokens"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

const projectCols = `id, name, summary, memory, dir, owner, merged_into, created_at, updated_at`

func scanProject(r interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	if err := r.Scan(&p.ID, &p.Name, &p.Summary, &p.Memory, &p.Dir, &p.Owner, &p.MergedInto, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// Projects 按最近更新排序，不带项目记忆（渐进式披露：列表只给摘要）。并进别的项目的不列。
func (s *Store) Projects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM projects WHERE merged_into = 0 ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		p.Memory = ""
		list = append(list, *p)
	}
	return list, rows.Err()
}

// Project 取一个项目，带项目记忆；没有时返回 nil。
func (s *Store) Project(id int64) (*Project, error) {
	p, err := scanProject(s.db.QueryRow(`SELECT `+projectCols+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *Store) AddProject(name, summary, dir string) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("项目名不能为空")
	}
	t := now()
	res, err := s.db.Exec(`INSERT INTO projects (name, summary, dir, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		name, summary, dir, t, t)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errors.New("已有同名项目：" + name)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.Project(id)
}

// UpdateProject 改项目的摘要、记忆、目录；nil 表示不改。
// 记忆有变化时，先把旧内容存进 memory_versions，助理写错了能退回。
// SetOwner 指定项目负责人；name 为空表示由助理兼任。
func (s *Store) SetOwner(id int64, name string) error {
	res, err := s.db.Exec(`UPDATE projects SET owner = ?, updated_at = ? WHERE id = ?`, name, now(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("没有这个项目")
	}
	return nil
}

func (s *Store) UpdateProject(id int64, summary, memory, dir *string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if memory != nil {
		var cur string
		if err := tx.QueryRow(`SELECT memory FROM projects WHERE id = ?`, id).Scan(&cur); err == sql.ErrNoRows {
			return errors.New("没有这个项目")
		} else if err != nil {
			return err
		}
		if cur != *memory && strings.TrimSpace(cur) != "" {
			if _, err := tx.Exec(`INSERT INTO memory_versions (project_id, memory, created_at) VALUES (?, ?, ?)`, id, cur, now()); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM memory_versions WHERE project_id = ? AND id NOT IN
				(SELECT id FROM memory_versions WHERE project_id = ? ORDER BY id DESC LIMIT ?)`, id, id, memoryKeep); err != nil {
				return err
			}
		}
	}
	sets, args := []string{"updated_at = ?"}, []any{now()}
	for col, v := range map[string]*string{"summary": summary, "memory": memory, "dir": dir} {
		if v != nil {
			sets = append(sets, col+" = ?")
			args = append(args, *v)
		}
	}
	args = append(args, id)
	res, err := tx.Exec("UPDATE projects SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("没有这个项目")
	}
	return tx.Commit()
}

// memoryKeep 是每个项目留的旧版本条数。
const memoryKeep = 50

// MemoryVersion 是项目记忆被改之前的一版。
type MemoryVersion struct {
	ID        int64  `json:"id"`
	Memory    string `json:"memory"`
	CreatedAt string `json:"createdAt"`
}

// MemoryVersions 返回项目记忆的旧版本，新的在前。CreatedAt 是这一版被替换掉的时间。
func (s *Store) MemoryVersions(projectID int64) ([]MemoryVersion, error) {
	rows, err := s.db.Query(`SELECT id, memory, created_at FROM memory_versions WHERE project_id = ? ORDER BY id DESC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MemoryVersion{}
	for rows.Next() {
		var v MemoryVersion
		if err := rows.Scan(&v.ID, &v.Memory, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RestoreMemory 把项目记忆退回到某个旧版本。退回本身也是一次改动，退回前的内容照样存成旧版本。
func (s *Store) RestoreMemory(projectID, versionID int64) error {
	var mem string
	err := s.db.QueryRow(`SELECT memory FROM memory_versions WHERE id = ? AND project_id = ?`, versionID, projectID).Scan(&mem)
	if err == sql.ErrNoRows {
		return errors.New("没有这个旧版本")
	}
	if err != nil {
		return err
	}
	return s.UpdateProject(projectID, nil, &mem, nil)
}

func (s *Store) RemoveProject(id int64) error {
	res, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("没有这个项目")
	}
	return nil
}

func (s *Store) AddRun(projectID int64, request string) (int64, error) {
	t := now()
	res, err := s.db.Exec(`INSERT INTO runs (project_id, request, created_at, updated_at) VALUES (?, ?, ?, ?)`, projectID, request, t, t)
	if err != nil {
		return 0, err
	}
	s.db.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, t, projectID)
	return res.LastInsertId()
}

// SetRun 改一次安排的字段：plan、reply、status。
func (s *Store) SetRun(id int64, field, value string) error {
	switch field {
	case "plan", "reply", "status", "verdict":
	default:
		return errors.New("不能改的字段：" + field)
	}
	_, err := s.db.Exec("UPDATE runs SET "+field+" = ?, updated_at = ? WHERE id = ?", value, now(), id)
	return err
}

// SetRework 记下这次安排是验收打回后重新安排的：root 是这串安排的第一次，rejects 是已被打回的次数。
func (s *Store) SetRework(id, root int64, rejects int) error {
	_, err := s.db.Exec(`UPDATE runs SET redo_of = ?, rejects = ?, updated_at = ? WHERE id = ?`, root, rejects, now(), id)
	return err
}

// Run 返回一次安排，带上它的全部任务；没有时返回 nil。
func (s *Store) Run(id int64) (*Run, error) {
	var r Run
	err := s.db.QueryRow(`SELECT `+runCols+` FROM runs WHERE id = ?`, id).Scan(r.fields()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Tasks, err = s.Tasks(id); err != nil {
		return nil, err
	}
	return &r, nil
}

const runCols = `id, project_id, request, plan, reply, status, redo_of, rejects, created_at, updated_at`

func (r *Run) fields() []any {
	return []any{&r.ID, &r.ProjectID, &r.Request, &r.Plan, &r.Reply, &r.Status, &r.RedoOf, &r.Rejects, &r.CreatedAt, &r.UpdatedAt}
}

// Runs 返回一个项目的安排，新的在前，每个带上它的全部任务。
func (s *Store) Runs(projectID int64, limit int) ([]Run, error) {
	rows, err := s.db.Query(`SELECT `+runCols+` FROM runs WHERE project_id = ? ORDER BY id DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	list := []Run{}
	for rows.Next() {
		var r Run
		if err := rows.Scan(r.fields()...); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()
	for i := range list {
		if list[i].Tasks, err = s.Tasks(list[i].ID); err != nil {
			return nil, err
		}
	}
	return list, nil
}

const taskCols = `id, run_id, COALESCE(parent_id, 0), depth, title, brief, agent, check_cmd, effort, status, runtime, model,
	session, report, check_out, feedback, approval, attempts, tokens, created_at, updated_at`

func scanTask(r interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := r.Scan(&t.ID, &t.RunID, &t.ParentID, &t.Depth, &t.Title, &t.Brief, &t.Agent, &t.Check, &t.Effort, &t.Status,
		&t.Runtime, &t.Model, &t.Session, &t.Report, &t.CheckOut, &t.Feedback, &t.Approval, &t.Attempts, &t.Tokens,
		&t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) Tasks(runID int64) ([]Task, error) {
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks WHERE run_id = ? ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *t)
	}
	return list, rows.Err()
}

// RecentTasks 返回项目里 since 之后建的任务，按编号排。
func (s *Store) RecentTasks(projectID int64, since time.Time) ([]Task, error) {
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks WHERE run_id IN (SELECT id FROM runs WHERE project_id = ?) ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		if at, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil && !at.Before(since) {
			list = append(list, *t)
		}
	}
	return list, rows.Err()
}

func (s *Store) Task(id int64) (*Task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// AddTask 建一个节点。parentID 为 0 时是顶层节点。
func (s *Store) AddTask(t Task) (int64, error) {
	var parent any
	if t.ParentID != 0 {
		parent = t.ParentID
	}
	ts := now()
	res, err := s.db.Exec(`INSERT INTO tasks (run_id, parent_id, depth, title, brief, agent, check_cmd, effort, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.RunID, parent, t.Depth, t.Title, t.Brief, t.Agent, t.Check, t.Effort, ts, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TaskPatch 是 SetTask 能改的字段；nil 表示不改。
type TaskPatch struct {
	Status, Agent, Runtime, Model, Effort, Session, Report, CheckOut, Feedback, Approval *string
	Attempts, Tokens                                                                     *int
}

func (s *Store) SetTask(id int64, p TaskPatch) error {
	sets, args := []string{"updated_at = ?"}, []any{now()}
	for col, v := range map[string]*string{
		"status": p.Status, "agent": p.Agent, "runtime": p.Runtime, "model": p.Model, "effort": p.Effort, "session": p.Session, "report": p.Report,
		"check_out": p.CheckOut, "feedback": p.Feedback, "approval": p.Approval,
	} {
		if v != nil {
			sets = append(sets, col+" = ?")
			args = append(args, *v)
		}
	}
	for col, v := range map[string]*int{"attempts": p.Attempts, "tokens": p.Tokens} {
		if v != nil {
			sets = append(sets, col+" = ?")
			args = append(args, *v)
		}
	}
	args = append(args, id)
	_, err := s.db.Exec("UPDATE tasks SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

// Ptr 方便写 TaskPatch：store.Ptr("done")。
func Ptr[T any](v T) *T { return &v }

// Seen 返回已经整理过的会话（键是「harness/会话 id」）和 morula 自己的任务用过的会话 id。
func (s *Store) Seen() (map[string]bool, error) {
	seen := map[string]bool{}
	rows, err := s.db.Query(`SELECT harness || '/' || session FROM imported UNION SELECT runtime || '/' || session FROM tasks WHERE session != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		seen[k] = true
	}
	return seen, rows.Err()
}

// MarkImported 记下一个会话已整理；projectID 为 0 表示判为闲聊。
func (s *Store) MarkImported(harness, session string, projectID int64) error {
	_, err := s.db.Exec(`INSERT INTO imported (harness, session, project_id, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(harness, session) DO UPDATE SET project_id = excluded.project_id`, harness, session, projectID, now())
	return err
}

// LastImported 返回每个 harness 最近一次整理进 morula 的时间（created_at），没整理过的不在里面。
func (s *Store) LastImported() (map[string]string, error) {
	out := map[string]string{}
	rows, err := s.db.Query(`SELECT harness, MAX(created_at) FROM imported GROUP BY harness`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h, at string
		if err := rows.Scan(&h, &at); err != nil {
			return nil, err
		}
		out[h] = at
	}
	return out, rows.Err()
}

// AgentStat 是一个 agent 名下任务的累计：做完几个、被打回或失败几个、最后一次动的时间。
type AgentStat struct {
	Done   int    `json:"done"`
	Missed int    `json:"missed"`
	Total  int    `json:"total"`
	Last   string `json:"last"`
}

// AgentStats 按 agent 名汇总所有任务。没接过任务的 agent 不在结果里。
func (s *Store) AgentStats() (map[string]AgentStat, error) {
	rows, err := s.db.Query(`SELECT agent,
		SUM(status = 'done'), SUM(status IN ('rejected', 'failed')), COUNT(*), MAX(updated_at)
		FROM tasks WHERE agent != '' GROUP BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AgentStat{}
	for rows.Next() {
		var name string
		var st AgentStat
		if err := rows.Scan(&name, &st.Done, &st.Missed, &st.Total, &st.Last); err != nil {
			return nil, err
		}
		out[name] = st
	}
	return out, rows.Err()
}

// AcceptStats 数验收下过几次结论、其中几次不通过。按安排上记的 verdict 数。
func (s *Store) AcceptStats() (checked, failed int, err error) {
	err = s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(verdict = 'fail'), 0) FROM runs WHERE verdict != ''`).Scan(&checked, &failed)
	return
}

// RunHit 是搜索命中的一次安排。
type RunHit struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"projectId"`
	Request   string `json:"request"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

// SearchRuns 找原话里包含 q 的安排（不分大小写），只看没被并掉的项目，新的在前，最多 n 条。
func (s *Store) SearchRuns(q string, n int) ([]RunHit, error) {
	q = strings.TrimSpace(q)
	out := []RunHit{}
	if q == "" {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT r.id, r.project_id, r.request, r.status, r.created_at FROM runs r JOIN projects p ON p.id = r.project_id
		WHERE p.merged_into = 0 AND instr(lower(r.request), lower(?)) > 0 ORDER BY r.id DESC LIMIT ?`, q, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h RunHit
		if err := rows.Scan(&h.ID, &h.ProjectID, &h.Request, &h.Status, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// WaitingProjects 是有任务在等用户批准的项目。
func (s *Store) WaitingProjects() (map[int64]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT r.project_id FROM tasks t JOIN runs r ON r.id = t.run_id WHERE t.status = 'approval'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
