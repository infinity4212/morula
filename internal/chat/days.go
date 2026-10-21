package chat

import (
	"context"
	"strings"
	"time"

	"morula/internal/journal"
)

// Watch 在后台盯着换日：morula 开着时，过了换日时刻（默认凌晨 4 点）且助理空闲，就盘点前一天；
// 到了早上发盘点的时刻（BriefAt，默认 08:30）把盘点发给用户。启动时也补写、补发之前没做的（例如 morula 关着过了夜）。
func (s *Session) Watch(ctx context.Context) {
	s.maintain()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.maintain()
		}
	}
}

func (s *Session) maintain() {
	if s.Summarize == nil {
		return
	}
	s.sendBrief()
	defer s.sendBrief() // 过了发的时刻才盘点完的（例如早上才打开 morula），盘点完就发
	s.mu.Lock()
	if len(s.active) > 0 {
		s.mu.Unlock()
		return
	}
	if today := journal.Today(); today != s.day {
		s.day = today
		s.failed = nil
	}
	todo := s.unsummarized()
	if len(todo) == 0 {
		s.mu.Unlock()
		return
	}
	// 写总结期间算助理在忙：新消息排队。补充没处可去，supp 不带缓冲，补充会改成单独处理。
	t := &turn{cancel: func() {}, supp: make(chan string)}
	s.active = append(s.active, t)
	s.mu.Unlock()

	s.catchUp(todo)

	s.mu.Lock()
	s.done(t)
	s.mu.Unlock()
}

// unsummarized 是今天以前、有用户消息、还没有总结的日子。调用方须持有 s.mu。
func (s *Session) unsummarized() []string {
	var out []string
	for _, d := range journal.Days() {
		if d >= s.day || s.failed[d] {
			continue
		}
		if _, ok := journal.Summary(d); ok {
			continue
		}
		if t, _ := journal.Transcript(d, 0); strings.Contains(t, "用户：") {
			out = append(out, d)
		}
	}
	return out
}

// catchUp 依次给没有总结的日子写总结。调用方已把 busy 设为 true。
func (s *Session) catchUp(days []string) {
	for _, d := range days {
		s.Emit(Event{"type": "note", "text": "换日：助理正在盘点 " + d + "，并整理长期记忆…"})
		tr, err := journal.Transcript(d, 120000)
		var text string
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			text, err = s.Summarize(ctx, d, tr)
			cancel()
		}
		if err != nil {
			s.mu.Lock()
			if s.failed == nil {
				s.failed = map[string]bool{}
			}
			s.failed[d] = true
			s.mu.Unlock()
			// summaryOf 让前端在这天的总结后来写成时不再显示这张报错卡片
			s.Emit(Event{"type": "error", "summaryOf": d, "text": d + " 的总结没写成：" + err.Error() + "\n（下次换日或重启 morula 时会再试）"})
			return
		}
		if err := journal.SaveSummary(d, text); err != nil {
			s.Emit(Event{"type": "error", "text": "保存总结失败：" + err.Error()})
			return
		}
		journal.MarkBrief(d)
	}
}

// now 是 sendBrief 看的时钟，测试里换掉。
var now = time.Now

// sendBrief 到了发盘点的时刻，把最近一天的盘点发给用户。更早的没发的只存着、不发。
func (s *Session) sendBrief() {
	days := journal.Briefs()
	if len(days) == 0 {
		return
	}
	last := days[len(days)-1]
	at := ""
	if s.BriefAt != nil {
		at = s.BriefAt()
	}
	d, err := time.ParseInLocation("2006-01-02", last, time.Local)
	if err != nil || now().Before(journal.BriefTime(d.AddDate(0, 0, 1).Format("2006-01-02"), at)) {
		return
	}
	for _, x := range days {
		journal.BriefSent(x)
	}
	if text, ok := journal.Summary(last); ok {
		s.Emit(Event{"type": "summary", "of": last, "text": text, "brief": true})
	}
}
