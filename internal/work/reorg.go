package work

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"morula/internal/chat"
	"morula/internal/store"
)

// 助理整理项目：改名直接改，合并先发卡片等用户确认，合并能撤销。
// 每次整理都在对话里留一条记录（reorg 事件），改名和合并的记录上带一个撤销按钮。
// 用户在界面上点的（ctx 不属于哪条消息）另外记给助理，助理下次处理消息时知道。

// RenameProject 改项目名，在对话里留一条记录。
func (e *Engine) RenameProject(ctx context.Context, id int64, name string) (old string, err error) {
	if old, err = e.DB.RenameProject(id, name); err != nil || old == strings.TrimSpace(name) {
		return old, err
	}
	e.Chat.EmitIn(ctx, chat.Event{"type": "reorg", "op": "rename", "project": id, "name": strings.TrimSpace(name), "old": old})
	if chat.TurnOf(ctx) == "" {
		e.noteMissed("用户在界面上把项目「" + old + "」改名为「" + strings.TrimSpace(name) + "」")
	}
	return old, nil
}

// MergeAnswer 是助理提出合并后的结果。
type MergeAnswer struct {
	Merge *store.Merge
	Text  string // 用户没选、自己写的话
	Err   error  // 卡片失效
}

// proposeMerge 发卡片问用户要不要把 src 并进 dst，用户确认后合并。默认项目不能并进别的项目。
func (e *Engine) proposeMerge(ctx context.Context, src, dst int64, reason string) (*MergeAnswer, error) {
	s, _ := e.DB.Project(src)
	d, _ := e.DB.Project(dst)
	switch {
	case s == nil || s.MergedInto != 0:
		return nil, fmt.Errorf("没有编号为 %d 的项目", src)
	case d == nil || d.MergedInto != 0:
		return nil, fmt.Errorf("没有编号为 %d 的项目", dst)
	case src == dst:
		return nil, errors.New("不能把项目并进它自己")
	}
	if def := e.DefaultProject(); def != nil && def.ID == src {
		return nil, errors.New("「" + s.Name + "」是默认项目，不能并进别的项目；要把里面的几次安排挪走，用挪安排")
	}
	runs, _ := e.DB.Runs(src, 1000)
	items := []string{
		fmt.Sprintf("「%s」的 %d 次安排挪到「%s」", s.Name, len(runs), d.Name),
		"「" + s.Name + "」的项目记忆接在「" + d.Name + "」的项目记忆后面",
		"「" + s.Name + "」从项目列表里收起来，项目文件夹 " + s.Dir + " 里的文件不动",
		"合并后可以撤销",
	}
	ans, err := e.Chat.Wait(ctx, chat.Event{"type": "ask", "text": "把「" + s.Name + "」并进「" + d.Name + "」吗？", "reason": reason,
		"items": items, "free": true,
		"options": []map[string]string{{"value": "go", "label": "合并"}, {"value": "cancel", "label": "不合并"}}})
	if err != nil {
		return &MergeAnswer{Err: err}, nil
	}
	if t := strings.TrimSpace(ans.Text); t != "" {
		return &MergeAnswer{Text: t}, nil
	}
	if ans.Choice != "go" {
		return &MergeAnswer{}, nil
	}
	m, err := e.DB.MergeProjects(src, dst)
	if err != nil {
		return nil, err
	}
	e.Chat.EmitIn(ctx, chat.Event{"type": "reorg", "op": "merge", "merge": m.ID, "src": src, "srcName": s.Name,
		"project": dst, "name": d.Name, "runs": m.Runs})
	return &MergeAnswer{Merge: m}, nil
}

// UndoMerge 撤销一次合并，在对话里留一条记录。kept 为 true 时并进去的那段项目记忆被改过，留着没删。
func (e *Engine) UndoMerge(ctx context.Context, id int64) (*store.Merge, bool, error) {
	m, kept, err := e.DB.UndoMerge(id)
	if err != nil {
		return nil, false, err
	}
	ev := chat.Event{"type": "reorg", "op": "unmerge", "merge": id, "src": m.Src, "project": m.Dst, "kept": kept}
	if s, _ := e.DB.Project(m.Src); s != nil {
		ev["srcName"] = s.Name
	}
	if d, _ := e.DB.Project(m.Dst); d != nil {
		ev["name"] = d.Name
	}
	e.Chat.EmitIn(ctx, ev)
	if chat.TurnOf(ctx) == "" {
		e.noteMissed(fmt.Sprintf("用户在界面上撤销了合并：「%v」从「%v」里分了出来，%d 次安排挪了回去", ev["srcName"], ev["name"], m.Runs))
	}
	return m, kept, nil
}

// MoveRuns 把几次安排挪到另一个项目，在对话里留一条记录。
func (e *Engine) MoveRuns(ctx context.Context, runs []int64, dst int64) (*store.Project, error) {
	d, _ := e.DB.Project(dst)
	if d == nil || d.MergedInto != 0 {
		return nil, fmt.Errorf("没有编号为 %d 的项目", dst)
	}
	if len(runs) == 0 {
		return nil, errors.New("没有要挪的安排")
	}
	if err := e.DB.MoveRuns(runs, dst); err != nil {
		return nil, err
	}
	e.Chat.EmitIn(ctx, chat.Event{"type": "reorg", "op": "move", "project": dst, "name": d.Name, "runs": len(runs)})
	return d, nil
}
