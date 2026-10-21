package subs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// codex 每轮回复后在会话记录里写一条 token_count 事件，带当时的额度（rate_limits）。
// 只读这个字段，不读 auth.json，所以账号名不显示。

const codexScan = 30 // 最多看最近改过的这么多个会话文件

func codexHome() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

type rateWin struct {
	Used    float64 `json:"used_percent"`
	Minutes int     `json:"window_minutes"`
	Resets  int64   `json:"resets_at"`
}

type rateLimits struct {
	Primary   *rateWin `json:"primary"`
	Secondary *rateWin `json:"secondary"`
	Plan      *string  `json:"plan_type"`
}

func readCodex(ctx context.Context) ([]Account, error) {
	dir := filepath.Join(codexHome(), "sessions")
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".jsonl" {
			return nil
		}
		if info, err := d.Info(); err == nil {
			files = append(files, file{p, info.ModTime()})
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		return nil, nil // 没用过 codex
	}
	slices.SortFunc(files, func(a, b file) int { return b.mod.Compare(a.mod) })
	for i, f := range files {
		if i >= codexScan || ctx.Err() != nil {
			break
		}
		if a, ok := lastRate(f.path); ok {
			return []Account{a}, nil
		}
	}
	return nil, nil
}

// lastRate 取一个会话文件里最后一条带额度的事件。
func lastRate(path string) (Account, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Account{}, false
	}
	defer f.Close()
	var line []byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if bytes.Contains(sc.Bytes(), []byte(`"rate_limits":{`)) {
			line = append(line[:0], sc.Bytes()...)
		}
	}
	if line == nil {
		return Account{}, false
	}
	var ev struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			RateLimits rateLimits `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return Account{}, false
	}
	return codexAccount(ev.Payload.RateLimits, ev.Timestamp), true
}

func codexAccount(r rateLimits, at string) Account {
	a := Account{Source: "codex", Agent: "codex", Plan: "ChatGPT", Active: true, On: true, Windows: []Window{}, ReadAt: at}
	if r.Plan != nil && *r.Plan != "" {
		a.Plan = *r.Plan
	}
	for _, w := range []*rateWin{r.Primary, r.Secondary} {
		if w == nil {
			continue
		}
		x := Window{Name: winName(w.Minutes), Used: w.Used, Remaining: max(0, 100-w.Used), Minutes: w.Minutes}
		if w.Resets > 0 {
			x.ResetsAt = time.Unix(w.Resets, 0).UTC().Format(time.RFC3339)
		}
		a.Windows = append(a.Windows, x)
	}
	return a
}

// winName 把窗口长度写成中文：300 分钟是「5 小时」，10080 分钟是「7 天」。
func winName(min int) string {
	switch {
	case min <= 0:
		return "额度"
	case min%1440 == 0:
		return fmt.Sprint(min/1440, " 天")
	case min%60 == 0:
		return fmt.Sprint(min/60, " 小时")
	}
	return fmt.Sprint(min, " 分钟")
}
