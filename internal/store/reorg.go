package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// 整理项目：改名、把安排挪到别的项目、合并两个项目、撤销合并。
// 这些都只改 morula 里的记录，工作目录和里面的文件不动。

// RenameProject 改项目名，返回旧名字。
func (s *Store) RenameProject(id int64, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("项目名不能为空")
	}
	var old string
	if err := s.db.QueryRow(`SELECT name FROM projects WHERE id = ?`, id).Scan(&old); errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("没有这个项目")
	} else if err != nil {
		return "", err
	}
	if old == name {
		return old, nil
	}
	if _, err := s.db.Exec(`UPDATE projects SET name = ?, updated_at = ? WHERE id = ?`, name, now(), id); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", errors.New("已有同名项目：" + name)
		}
		return "", err
	}
	return old, nil
}

// MoveRuns 把几次安排挪到另一个项目。还在跑的安排不挪：子 agent 正在原项目的目录里干活。
func (s *Store) MoveRuns(ids []int64, dst int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var merged int64
	if err := tx.QueryRow(`SELECT merged_into FROM projects WHERE id = ?`, dst).Scan(&merged); errors.Is(err, sql.ErrNoRows) {
		return errors.New("没有这个项目")
	} else if err != nil {
		return err
	}
	if merged != 0 {
		return errors.New("这个项目已并进别的项目")
	}
	t := now()
	for _, id := range ids {
		var st string
		if err := tx.QueryRow(`SELECT status FROM runs WHERE id = ?`, id).Scan(&st); errors.Is(err, sql.ErrNoRows) {
			return errors.New("没有安排 " + strconv.FormatInt(id, 10))
		} else if err != nil {
			return err
		}
		if st == "working" {
			return errors.New("安排 " + strconv.FormatInt(id, 10) + " 还在跑，做完或停下后再挪")
		}
		if _, err := tx.Exec(`UPDATE runs SET project_id = ?, updated_at = ? WHERE id = ?`, dst, t, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, t, dst); err != nil {
		return err
	}
	return tx.Commit()
}

// Merge 是一次合并：src 并进了 dst。
type Merge struct {
	ID        int64  `json:"id"`
	Src       int64  `json:"src"`
	Dst       int64  `json:"dst"`
	Runs      int    `json:"runs"` // 挪过去的安排有几次
	UndoneAt  string `json:"undoneAt,omitempty"`
	CreatedAt string `json:"createdAt"`
}

type importedKey struct {
	Harness string `json:"harness"`
	Session string `json:"session"`
}

// MergeProjects 把 src 并进 dst：src 的安排和整理记录挪到 dst，src 的项目记忆接在 dst 的项目记忆后面，
// src 从项目列表里收起来。合并前 dst 的项目记忆存成旧版本。src 还有安排在跑时不并。
func (s *Store) MergeProjects(src, dst int64) (*Merge, error) {
	if src == dst {
		return nil, errors.New("不能把项目并进它自己")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var sName, sMem, dMem string
	var sMerged, dMerged int64
	if err := tx.QueryRow(`SELECT name, memory, merged_into FROM projects WHERE id = ?`, src).Scan(&sName, &sMem, &sMerged); errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("没有要并的项目")
	} else if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(`SELECT memory, merged_into FROM projects WHERE id = ?`, dst).Scan(&dMem, &dMerged); errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("没有要并进的项目")
	} else if err != nil {
		return nil, err
	}
	if sMerged != 0 || dMerged != 0 {
		return nil, errors.New("有一个项目已并进别的项目")
	}
	var working int
	tx.QueryRow(`SELECT COUNT(*) FROM runs WHERE project_id = ? AND status = 'working'`, src).Scan(&working)
	if working > 0 {
		return nil, errors.New("「" + sName + "」还有安排在跑，做完或停下后再合并")
	}
	runs, err := ids(tx, `SELECT id FROM runs WHERE project_id = ? ORDER BY id`, src)
	if err != nil {
		return nil, err
	}
	var imp []importedKey
	rows, err := tx.Query(`SELECT harness, session FROM imported WHERE project_id = ?`, src)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k importedKey
		if err := rows.Scan(&k.Harness, &k.Session); err != nil {
			rows.Close()
			return nil, err
		}
		imp = append(imp, k)
	}
	rows.Close()

	t := now()
	var added string
	if m := strings.TrimSpace(sMem); m != "" {
		added = "\n\n## 并入的「" + sName + "」\n\n" + m + "\n"
		if strings.TrimSpace(dMem) != "" {
			if _, err := tx.Exec(`INSERT INTO memory_versions (project_id, memory, created_at) VALUES (?, ?, ?)`, dst, dMem, t); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(`UPDATE projects SET memory = ? WHERE id = ?`, strings.TrimRight(dMem, "\n")+added, dst); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(`UPDATE runs SET project_id = ? WHERE project_id = ?`, dst, src); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE imported SET project_id = ? WHERE project_id = ?`, dst, src); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE projects SET merged_into = ?, updated_at = ? WHERE id = ?`, dst, t, src); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, t, dst); err != nil {
		return nil, err
	}
	rj, _ := json.Marshal(runs)
	ij, _ := json.Marshal(imp)
	res, err := tx.Exec(`INSERT INTO merges (src, dst, runs, imported, added, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		src, dst, string(rj), string(ij), added, t)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Merge{ID: id, Src: src, Dst: dst, Runs: len(runs), CreatedAt: t}, nil
}

// UndoMerge 撤销一次合并：挪过去的安排和整理记录挪回来，src 回到项目列表。
// 加进 dst 项目记忆的那一段还原样在时删掉；被改过时留着，kept 为 true。
func (s *Store) UndoMerge(id int64) (m *Merge, kept bool, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var rj, ij, added string
	m = &Merge{ID: id}
	err = tx.QueryRow(`SELECT src, dst, runs, imported, added, undone_at, created_at FROM merges WHERE id = ?`, id).
		Scan(&m.Src, &m.Dst, &rj, &ij, &added, &m.UndoneAt, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, errors.New("没有这次合并")
	} else if err != nil {
		return nil, false, err
	}
	if m.UndoneAt != "" {
		return nil, false, errors.New("这次合并已经撤销过")
	}
	var runs []int64
	var imp []importedKey
	json.Unmarshal([]byte(rj), &runs)
	json.Unmarshal([]byte(ij), &imp)
	m.Runs = len(runs)
	t := now()
	for _, r := range runs { // 合并后又被挪去别处的安排不动
		if _, err := tx.Exec(`UPDATE runs SET project_id = ? WHERE id = ? AND project_id = ?`, m.Src, r, m.Dst); err != nil {
			return nil, false, err
		}
	}
	for _, k := range imp {
		if _, err := tx.Exec(`UPDATE imported SET project_id = ? WHERE harness = ? AND session = ? AND project_id = ?`, m.Src, k.Harness, k.Session, m.Dst); err != nil {
			return nil, false, err
		}
	}
	if added != "" {
		var mem string
		if err := tx.QueryRow(`SELECT memory FROM projects WHERE id = ?`, m.Dst).Scan(&mem); err != nil {
			return nil, false, err
		}
		if i := strings.LastIndex(mem, added); i >= 0 {
			if _, err := tx.Exec(`INSERT INTO memory_versions (project_id, memory, created_at) VALUES (?, ?, ?)`, m.Dst, mem, t); err != nil {
				return nil, false, err
			}
			mem = mem[:i] + mem[i+len(added):]
			if _, err := tx.Exec(`UPDATE projects SET memory = ? WHERE id = ?`, mem, m.Dst); err != nil {
				return nil, false, err
			}
		} else {
			kept = true
		}
	}
	if _, err := tx.Exec(`UPDATE projects SET merged_into = 0, updated_at = ? WHERE id = ?`, t, m.Src); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(`UPDATE merges SET undone_at = ? WHERE id = ?`, t, id); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	m.UndoneAt = t
	return m, kept, nil
}

// LastMerge 返回最近一次还没撤销的合并；没有时返回 nil。
func (s *Store) LastMerge() (*Merge, error) {
	m := &Merge{}
	var rj string
	err := s.db.QueryRow(`SELECT id, src, dst, runs, created_at FROM merges WHERE undone_at = '' ORDER BY id DESC LIMIT 1`).
		Scan(&m.ID, &m.Src, &m.Dst, &rj, &m.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var runs []int64
	json.Unmarshal([]byte(rj), &runs)
	m.Runs = len(runs)
	return m, nil
}

func ids(tx *sql.Tx, q string, args ...any) ([]int64, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
