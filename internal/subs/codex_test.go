package subs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 取最近改过的会话文件里最后一条带额度的事件；窗口长度写成中文；没有会话时返回空、不报错。
func TestReadCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if as, err := readCodex(context.Background()); len(as) != 0 || err != nil {
		t.Fatalf("没有会话：%v %v", as, err)
	}
	day := filepath.Join(home, "sessions", "2026", "10", "17")
	os.MkdirAll(day, 0o755)
	old := `{"timestamp":"2026-10-17T01:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":90,"window_minutes":300,"resets_at":1}}}}`
	newer := `{"timestamp":"2026-10-17T02:00:00Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":4,"window_minutes":300,"resets_at":1794823068},"secondary":{"used_percent":30.5,"window_minutes":10080,"resets_at":0},"plan_type":"plus"}}}` + "\n" +
		`{"timestamp":"2026-10-17T02:00:01Z","type":"event_msg","payload":{"type":"agent_message"}}`
	a, b := filepath.Join(day, "a.jsonl"), filepath.Join(day, "b.jsonl")
	os.WriteFile(a, []byte(old+"\n"+newer+"\n"), 0o644)
	os.WriteFile(b, []byte(old+"\n"), 0o644)
	os.Chtimes(b, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)) // a 是最近改过的
	as, err := readCodex(context.Background())
	if err != nil || len(as) != 1 {
		t.Fatalf("%v %v", as, err)
	}
	got := as[0]
	if got.Plan != "plus" || got.ReadAt != "2026-10-17T02:00:00Z" || len(got.Windows) != 2 {
		t.Fatalf("%+v", got)
	}
	w0, w1 := got.Windows[0], got.Windows[1]
	if w0.Name != "5 小时" || w0.Used != 4 || w0.Remaining != 96 || w0.ResetsAt != "2026-11-16T09:57:48Z" {
		t.Errorf("主窗口：%+v", w0)
	}
	if w1.Name != "7 天" || w1.ResetsAt != "" {
		t.Errorf("次窗口：%+v", w1)
	}
	if winName(43200) != "30 天" || winName(90) != "90 分钟" {
		t.Error("winName")
	}
}
