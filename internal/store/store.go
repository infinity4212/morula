// Package store 把 Agent 花名册、设置、项目和任务树存进 SQLite。
// 桌面窗口和 `morula mcp` 是两个进程，共用同一个库文件。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Runtimes 是 agent 可选的 harness。
// 能真正启动的只有 internal/harness 里有适配器的那几个。
var Runtimes = []string{"codex", "opencode", "pi", "hermes", "workbuddy"}

// RoleAssistant 是助理的角色名。助理和子 agent 是同一种 agent，只是角色不同；花名册里只能有一个助理。
const RoleAssistant = "助理"

type Agent struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Runtime      string   `json:"runtime"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	Instructions string   `json:"instructions"`
	Skills       []string `json:"skills"`   // 绑定的 skill 名，派活时让子 agent 先读对应的 SKILL.md
	Projects     []int64  `json:"projects"` // 常用于哪些项目，派活时优先选
	Profile      string   `json:"profile"`  // 写给用户看的介绍：助理按绑定的 skill 写，用户也能改
	Status       string   `json:"status"`
	Task         string   `json:"task"`
	UpdatedAt    string   `json:"updatedAt"`
	Off          bool     `json:"off"` // 关着：不接新活，手上的活做完为止
}

// Patch 是 update 时可改的字段；nil 表示不改。
type Patch struct {
	Role         *string   `json:"role,omitempty"`
	Runtime      *string   `json:"runtime,omitempty"`
	Model        *string   `json:"model,omitempty"`
	Effort       *string   `json:"effort,omitempty"`
	Instructions *string   `json:"instructions,omitempty"`
	Skills       *[]string `json:"skills,omitempty"`
	Projects     *[]int64  `json:"projects,omitempty"`
	Profile      *string   `json:"profile,omitempty"`
	Off          *bool     `json:"off,omitempty"`
}

type Store struct{ db *sql.DB }

// Dir 是 morula 的数据目录；MORULA_HOME 可以改掉它（测试用）。
func Dir() (string, error) {
	if d := os.Getenv("MORULA_HOME"); d != "" {
		return d, nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "morula"), nil
}

func Open() (*Store, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dir, "morula.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS agents (
	name         TEXT PRIMARY KEY,
	role         TEXT NOT NULL DEFAULT '执行',
	runtime      TEXT NOT NULL,
	model        TEXT NOT NULL,
	effort       TEXT NOT NULL DEFAULT '',
	instructions TEXT NOT NULL DEFAULT '',
	skills       TEXT NOT NULL DEFAULT '[]',
	projects     TEXT NOT NULL DEFAULT '[]',
	profile      TEXT NOT NULL DEFAULT '',
	status       TEXT NOT NULL DEFAULT 'idle',
	task         TEXT NOT NULL DEFAULT '',
	off          INTEGER NOT NULL DEFAULT 0,
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);` + projectTables + routineTables); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败：%w", err)
	}
	// agents 表补列：skills、projects、profile。
	for _, c := range []struct{ col, def string }{{"skills", "'[]'"}, {"projects", "'[]'"}, {"profile", "''"}} {
		col := c.col
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = ?`, col).Scan(&n)
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE agents ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ` + c.def); err != nil {
				db.Close()
				return nil, fmt.Errorf("给 agents 加 %s 列失败：%w", col, err)
			}
		}
	}
	// agents 表补列：off（Agent 页的开关）。
	var nOff int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = 'off'`).Scan(&nOff)
	if nOff == 0 {
		if _, err := db.Exec(`ALTER TABLE agents ADD COLUMN off INTEGER NOT NULL DEFAULT 0`); err != nil {
			db.Close()
			return nil, fmt.Errorf("给 agents 加 off 列失败：%w", err)
		}
	}
	// runs 表补列：verdict（验收结论：pass、fail，没验收时为空）。
	var nVerdict int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'verdict'`).Scan(&nVerdict)
	if nVerdict == 0 {
		if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN verdict TEXT NOT NULL DEFAULT ''`); err != nil {
			db.Close()
			return nil, fmt.Errorf("给 runs 加 verdict 列失败：%w", err)
		}
	}
	// runs 表补列：redo_of、rejects（验收打回后重新安排的那串安排）。
	for _, col := range []string{"redo_of", "rejects"} {
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = ?`, col).Scan(&n)
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE runs ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
				db.Close()
				return nil, fmt.Errorf("给 runs 加 %s 列失败：%w", col, err)
			}
		}
	}
	// projects 表补列：owner（项目负责人）。
	var nOwner int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name = 'owner'`).Scan(&nOwner)
	if nOwner == 0 {
		if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN owner TEXT NOT NULL DEFAULT ''`); err != nil {
			db.Close()
			return nil, fmt.Errorf("给 projects 加 owner 列失败：%w", err)
		}
	}
	// projects 表补列：merged_into（合并项目）。
	var nMerged int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name = 'merged_into'`).Scan(&nMerged)
	if nMerged == 0 {
		if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN merged_into INTEGER NOT NULL DEFAULT 0`); err != nil {
			db.Close()
			return nil, fmt.Errorf("给 projects 加 merged_into 列失败：%w", err)
		}
	}
	// tasks 表补列：effort（派活时给每个任务定的思考强度）。
	var nEffort int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tasks') WHERE name = 'effort'`).Scan(&nEffort)
	if nEffort == 0 {
		if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN effort TEXT NOT NULL DEFAULT ''`); err != nil {
			db.Close()
			return nil, fmt.Errorf("给 tasks 加 effort 列失败：%w", err)
		}
	}
	// 迁移已停用的运行时标识。
	if _, err := db.Exec(`UPDATE agents SET runtime = 'codex', model = 'gpt-6-sol', updated_at = ? WHERE runtime = 'claude'`, now()); err != nil {
		db.Close()
		return nil, fmt.Errorf("迁移 claude agent 失败：%w", err)
	}
	// 迁移已改名的运行时标识。
	for _, t := range []string{"agents", "tasks"} {
		if _, err := db.Exec(`UPDATE ` + t + ` SET runtime = 'workbuddy' WHERE runtime = 'codebuddy'`); err != nil {
			db.Close()
			return nil, fmt.Errorf("把 %s 里的 codebuddy 改成 workbuddy 失败：%w", t, err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Recover 在 app 启动时调用：上次退出时还在跑的任务已经没有进程了，标成失败；agent 都放回空闲，
// 免得界面一直显示工作中。不放在 Open 里，因为 `morula mcp` 也会 Open，那时 app 可能正在跑任务。
func (s *Store) Recover() error {
	for _, q := range []string{
		`UPDATE tasks SET status = 'failed', feedback = 'morula 退出时中断', updated_at = ? WHERE status IN ('waiting','working','checking','approval')`,
		`UPDATE runs SET status = 'failed', updated_at = ? WHERE status = 'working'`,
	} {
		if _, err := s.db.Exec(q, now()); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(`UPDATE agents SET status = 'idle', task = ''`); err != nil {
		return err
	}
	return s.EndStaleRoutineRuns()
}

// list 把切片存成 JSON 数组；nil 存成 []。
func list[T any](v []T) string {
	if v == nil {
		return "[]"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func now() string { return time.Now().Format(time.RFC3339) }

func (s *Store) Agents() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT name, role, runtime, model, effort, instructions, skills, projects, profile, status, task, updated_at, off
		FROM agents ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Agent{}
	for rows.Next() {
		var a Agent
		var skills, projects string
		if err := rows.Scan(&a.Name, &a.Role, &a.Runtime, &a.Model, &a.Effort, &a.Instructions, &skills, &projects, &a.Profile, &a.Status, &a.Task, &a.UpdatedAt, &a.Off); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(skills), &a.Skills)
		json.Unmarshal([]byte(projects), &a.Projects)
		if a.Skills == nil {
			a.Skills = []string{}
		}
		if a.Projects == nil {
			a.Projects = []int64{}
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

func checkRuntime(r string) error {
	if !slices.Contains(Runtimes, r) {
		return fmt.Errorf("不支持的运行时 %q，可选：%s", r, strings.Join(Runtimes, "、"))
	}
	return nil
}

func (s *Store) AddAgent(a Agent) error {
	a.Name = strings.TrimSpace(a.Name)
	a.Model = strings.TrimSpace(a.Model)
	if a.Name == "" || a.Runtime == "" || a.Model == "" {
		return errors.New("名称、运行时、模型都要填")
	}
	if err := checkRuntime(a.Runtime); err != nil {
		return err
	}
	if a.Role == "" {
		a.Role = "执行"
	}
	if a.Role == RoleAssistant {
		if cur, _ := s.Assistant(); cur != nil {
			return errors.New("已经有助理了：" + cur.Name + "。要换助理，在设置里把助理角色指给另一个 agent")
		}
	}
	t := now()
	_, err := s.db.Exec(`INSERT INTO agents (name, role, runtime, model, effort, instructions, skills, projects, profile, status, task, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'idle', '', ?, ?)`,
		a.Name, a.Role, a.Runtime, a.Model, a.Effort, a.Instructions, list(a.Skills), list(a.Projects), a.Profile, t, t)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("已有同名 Agent：" + a.Name)
	}
	return err
}

func (s *Store) UpdateAgent(name string, p Patch) error {
	if p.Runtime != nil {
		if err := checkRuntime(*p.Runtime); err != nil {
			return err
		}
	}
	sets, args := []string{}, []any{}
	add := func(col string, v *string) {
		if v != nil {
			sets = append(sets, col+" = ?")
			args = append(args, *v)
		}
	}
	if p.Role != nil && *p.Role == RoleAssistant {
		if cur, _ := s.Assistant(); cur != nil && cur.Name != name {
			return errors.New("已经有助理了：" + cur.Name + "。用「设为助理」换人")
		}
	}
	add("role", p.Role)
	add("runtime", p.Runtime)
	add("model", p.Model)
	add("effort", p.Effort)
	add("instructions", p.Instructions)
	add("profile", p.Profile)
	if p.Skills != nil {
		v := list(*p.Skills)
		add("skills", &v)
	}
	if p.Projects != nil {
		v := list(*p.Projects)
		add("projects", &v)
	}
	if p.Off != nil {
		sets = append(sets, "off = ?")
		args = append(args, *p.Off)
	}
	if len(sets) == 0 {
		return errors.New("没有要改的字段")
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, now(), name)
	res, err := s.db.Exec("UPDATE agents SET "+strings.Join(sets, ", ")+" WHERE name = ?", args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("没有这个 Agent：" + name)
	}
	return nil
}

func (s *Store) RemoveAgent(name string) error {
	res, err := s.db.Exec(`DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("没有这个 Agent：" + name)
	}
	// 被删 agent 的项目负责人清空，改回由助理兼任
	_, err = s.db.Exec(`UPDATE projects SET owner = '' WHERE owner = ?`, name)
	return err
}

func (s *Store) Setting(key, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Agent 按名字取一个 Agent；没有时返回 nil。
func (s *Store) Agent(name string) (*Agent, error) {
	list, err := s.Agents()
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.Name == name {
			return &a, nil
		}
	}
	return nil, nil
}

// SetStatus 改运行状态和当前任务（不算配置改动，不更新 updated_at）。
func (s *Store) SetStatus(name, status, task string) error {
	_, err := s.db.Exec(`UPDATE agents SET status = ?, task = ? WHERE name = ?`, status, task, name)
	return err
}

// Assistant 返回角色为助理的 agent；没有时返回 nil。
func (s *Store) Assistant() (*Agent, error) {
	list, err := s.Agents()
	if err != nil {
		return nil, err
	}
	for _, a := range list {
		if a.Role == RoleAssistant {
			return &a, nil
		}
	}
	return nil, nil
}

// MakeAssistant 把助理角色指给 name；原来的助理改成「执行」。
func (s *Store) MakeAssistant(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t := now()
	if _, err := tx.Exec(`UPDATE agents SET role = '执行', updated_at = ? WHERE role = ?`, t, RoleAssistant); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE agents SET role = ?, updated_at = ? WHERE name = ?`, RoleAssistant, t, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("没有这个 Agent：" + name)
	}
	return tx.Commit()
}
