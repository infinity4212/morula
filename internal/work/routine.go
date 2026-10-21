package work

import (
	"context"
	"fmt"
	"strings"
	"time"

	"morula/internal/store"
)

// 例行事项：到点由助理照原话处理，做完的结果进对话。
// 每 30 秒看一次哪件事项到了点。Mac 睡着时错过的点，醒来后在 12 小时内补跑最近的一次，更早的不补。
// 到点时助理正忙，就等它空下来再发，等过 2 小时还没空就记为跳过。

const (
	routineTick    = 30 * time.Second
	routineCatchUp = 12 * time.Hour
	routineWaitMax = 2 * time.Hour
)

// LastSlot 是 r 在 now 之前（含）最近的一个时间点；7 天里都没有时返回零值。
func LastSlot(r store.Routine, now time.Time) time.Time {
	hh, mm := 0, 0
	fmt.Sscanf(r.At, "%d:%d", &hh, &mm)
	for d := 0; d <= 7; d++ {
		day := now.AddDate(0, 0, -d)
		t := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, now.Location())
		if r.Days&(1<<int(t.Weekday())) != 0 && !t.After(now) {
			return t
		}
	}
	return time.Time{}
}

// NextSlot 是 r 在 now 之后最近的一个时间点；没选任何一天时返回零值。
func NextSlot(r store.Routine, now time.Time) time.Time {
	hh, mm := 0, 0
	fmt.Sscanf(r.At, "%d:%d", &hh, &mm)
	for d := 0; d <= 7; d++ {
		day := now.AddDate(0, 0, d)
		t := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, now.Location())
		if r.Days&(1<<int(t.Weekday())) != 0 && t.After(now) {
			return t
		}
	}
	return time.Time{}
}

// dueSlot 是这一刻要发的时间点：最近的点在事项设好（改过）之后、离现在不到 12 小时。没有时返回零值。
func dueSlot(r store.Routine, now time.Time) time.Time {
	t := LastSlot(r, now)
	if t.IsZero() || now.Sub(t) > routineCatchUp {
		return time.Time{}
	}
	if set, err := time.Parse(time.RFC3339, r.UpdatedAt); err == nil && !t.After(set) {
		return time.Time{}
	}
	return t
}

// Routines 在后台按时发例行事项，ctx 结束时停。
func (e *Engine) Routines(ctx context.Context) {
	tk := time.NewTicker(routineTick)
	defer tk.Stop()
	for {
		e.routineTick(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
	}
}

func (e *Engine) routineTick(now time.Time) {
	rs, err := e.DB.Routines()
	if err != nil {
		return
	}
	for _, r := range rs {
		if !r.Enabled {
			continue
		}
		slot := dueSlot(r, now)
		if slot.IsZero() {
			continue
		}
		key := slot.Format(time.RFC3339)
		if r.Last != nil && r.Last.Slot == key {
			continue // 这个点已经发过或记过跳过
		}
		if _, ok := e.RunRoutine(r, key); !ok && now.Sub(slot) > routineWaitMax {
			if id, _ := e.DB.AddRoutineRun(r.ID, key); id != 0 {
				e.DB.EndRoutineRun(id, "failed", "到点时助理一直在忙，等了 2 小时没发出去")
			}
		}
	}
}

// RunRoutine 把一件例行事项发给助理。slot 是对应的时间点；手动「现在跑一次」时填当前时间。
// 助理正忙时不发，ok 为 false。
func (e *Engine) RunRoutine(r store.Routine, slot string) (runID int64, ok bool) {
	id, err := e.DB.AddRoutineRun(r.ID, slot)
	if err != nil || id == 0 {
		return 0, false
	}
	turn, ok := e.Chat.SendRoutine(r.Name, routineText(e, r), func(stopped bool) {
		if stopped {
			e.DB.EndRoutineRun(id, "stopped", "被停止了")
		} else {
			e.DB.EndRoutineRun(id, "done", "")
		}
	})
	if !ok {
		e.DB.RemoveRoutineRun(id)
		return 0, false
	}
	e.DB.SetRoutineTurn(id, turn)
	return id, true
}

// routineText 是发给助理的那条消息：说明是例行事项，指定了谁做、放进哪个项目就写上，后面是原话。
func routineText(e *Engine, r store.Routine) string {
	var b strings.Builder
	fmt.Fprintf(&b, "【例行事项「%s」到点了】", r.Name)
	if a := strings.TrimSpace(r.Agent); a != "" && a != "助理" {
		fmt.Fprintf(&b, "交给 %s 做。", a)
	}
	if r.ProjectID != 0 {
		if p, _ := e.DB.Project(r.ProjectID); p != nil && p.MergedInto == 0 {
			fmt.Fprintf(&b, "放进项目「%s」（编号 %d）。", p.Name, p.ID)
		}
	}
	b.WriteString("\n" + r.Prompt)
	return b.String()
}
