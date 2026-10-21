package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"morula/internal/journal"
	"morula/internal/store"
)

// 换日后盘点只存着、不发；到了发盘点的时刻只发最近一天，更早的不发。
func TestBrief(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	dir, _ := store.Dir()
	days := filepath.Join(dir, "days")
	os.MkdirAll(days, 0o755)
	today, _ := time.ParseInLocation("2006-01-02", journal.Today(), time.Local)
	d1, d2 := today.AddDate(0, 0, -2).Format("2006-01-02"), today.AddDate(0, 0, -1).Format("2006-01-02")
	for _, d := range []string{d1, d2} {
		os.WriteFile(filepath.Join(days, d+".jsonl"), []byte(`{"type":"user","text":"你好"}`+"\n"), 0o644)
	}
	s := New()
	s.Summarize = func(_ context.Context, day, _ string) (string, error) { return day + " 的盘点", nil }
	at := "08:30"
	s.BriefAt = func() string { return at }
	defer func() { now = time.Now }()
	now = func() time.Time { return journal.BriefTime(journal.Today(), at).Add(-time.Minute) }
	s.maintain()
	if got := journal.Briefs(); len(got) != 2 {
		t.Fatalf("待发的盘点 = %q，两天都应存着", got)
	}
	sent := func() []string {
		var out []string
		for _, e := range s.events {
			if e["type"] == "summary" {
				out = append(out, e["of"].(string))
			}
		}
		return out
	}
	if got := sent(); len(got) != 0 {
		t.Fatalf("没到时刻就发了 %q", got)
	}
	now = func() time.Time { return journal.BriefTime(journal.Today(), at) }
	s.maintain()
	if got := sent(); len(got) != 1 || got[0] != d2 {
		t.Fatalf("到了时刻应只发 %s，发了 %q", d2, got)
	}
	if len(journal.Briefs()) != 0 {
		t.Error("发过的盘点应去掉待发标记")
	}
}

func TestBriefTime(t *testing.T) {
	for at, want := range map[string]string{
		"":      "2026-10-19 08:30",
		"9:05":  "2026-10-19 09:05",
		"02:00": "2026-10-20 02:00", // 早于换日时刻：算到换日后的那个钟点
		"坏":     "2026-10-19 08:30",
	} {
		if got := journal.BriefTime("2026-10-19", at).Format("2006-01-02 15:04"); got != want {
			t.Errorf("BriefTime(%q) = %s，应为 %s", at, got, want)
		}
	}
}
