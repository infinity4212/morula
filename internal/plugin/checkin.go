package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 每日签到：插件的 auth.checkin 钩子（例如 Qoder 每天领一次积分）。用户在「设置 / 账号」里给一个来源打开后，
// morula 每天替它领一次。状态记在 checkin.json：{"<provider>": CheckinState}。默认不打开：领奖励是替用户在对方网站上做的事。

// CheckinState 是一个来源的签到开关和最近一次结果。
type CheckinState struct {
	On      bool    `json:"on"`
	Day     string  `json:"day,omitempty"`     // 已经不用再领的那天（本机日期）：领到了、今天领过了、或今天没有可领的
	Outcome string  `json:"outcome,omitempty"` // claimed、done、inactive、failed
	Credit  float64 `json:"credit,omitempty"`  // 领到的数额，插件给了才有
	Message string  `json:"message,omitempty"` // 失败的原因
	At      string  `json:"at,omitempty"`      // 最近一次签到的时间（RFC 3339）
}

// checkinRetry 是失败后隔多久再试。
const checkinRetry = time.Hour

var checkinMu sync.Mutex

func checkinPath() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "checkin.json"), nil
}

func readCheckins() map[string]CheckinState {
	out := map[string]CheckinState{}
	if p, err := checkinPath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			json.Unmarshal(b, &out)
		}
	}
	return out
}

func writeCheckins(m map[string]CheckinState) error {
	p, err := checkinPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// Checkins 读全部来源的签到状态。文件不存在时返回空表。
func Checkins() map[string]CheckinState {
	checkinMu.Lock()
	defer checkinMu.Unlock()
	return readCheckins()
}

// SetCheckin 打开或关掉一个来源的每日签到。
func SetCheckin(provider string, on bool) error {
	if provider == "" {
		return errors.New("缺来源")
	}
	checkinMu.Lock()
	defer checkinMu.Unlock()
	m := readCheckins()
	s := m[provider]
	s.On = on
	m[provider] = s
	return writeCheckins(m)
}

// Checkin 让插件现在签一次到，记下结果。
func Checkin(ctx context.Context, provider string) (CheckinState, error) {
	h, err := Get(ctx)
	if err != nil {
		return CheckinState{}, err
	}
	var r struct {
		Outcome string  `json:"outcome"`
		Credit  float64 `json:"credit"`
		Message string  `json:"message"`
		SignIn  string  `json:"signIn"`
	}
	now := time.Now()
	callErr := h.Call(ctx, "checkin", map[string]string{"provider": provider}, &r)
	if callErr != nil {
		r.Outcome, r.Message = "failed", callErr.Error()
	} else if r.SignIn == "expired" {
		r.Outcome, r.Message = "failed", "登录过期了，要退出后重新登录"
	}
	checkinMu.Lock()
	defer checkinMu.Unlock()
	m := readCheckins()
	s := m[provider]
	s.Outcome, s.Credit, s.Message, s.At = r.Outcome, r.Credit, r.Message, now.Format(time.RFC3339)
	if r.Outcome == "failed" || r.Outcome == "" {
		s.Outcome = "failed"
		if s.Message == "" {
			s.Message = "插件没有说签到的结果"
		}
	} else {
		s.Day = now.Format(time.DateOnly)
	}
	m[provider] = s
	if err := writeCheckins(m); err != nil {
		return s, err
	}
	if callErr != nil {
		return s, fmt.Errorf("签到失败：%w", callErr)
	}
	return s, nil
}

// CheckinDue 给打开了签到、今天还没签、已登录的来源各签一次到；失败的隔 checkinRetry 再试。
// 没有打开的来源时不启动插件运行器。
func CheckinDue(ctx context.Context) {
	now := time.Now()
	today := now.Format(time.DateOnly)
	var due []string
	for id, s := range Checkins() {
		if !s.On || s.Day == today {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s.At); err == nil && s.Outcome == "failed" && now.Sub(t) < checkinRetry {
			continue
		}
		due = append(due, id)
	}
	if len(due) == 0 {
		return
	}
	ps, _ := Catalog(ctx, false)
	for _, id := range due {
		for _, p := range ps {
			if p.ID == id && p.SignedIn && p.Checkin {
				Checkin(ctx, id)
			}
		}
	}
}
