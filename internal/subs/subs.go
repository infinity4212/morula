// Package subs 汇总订阅账号和额度。来源有两个：codex 会话记录里的额度、已登录订阅插件的 usage 钩子。
package subs

import (
	"context"
	"sync"
	"time"
)

type Window struct {
	Name      string  `json:"name"`
	Used      float64 `json:"used"`      // 百分比
	Remaining float64 `json:"remaining"` // 百分比
	ResetsAt  string  `json:"resetsAt,omitempty"`
	Display   string  `json:"display,omitempty"` // 来源给的用量文字，例如 120 / 1000
	Minutes   int     `json:"minutes,omitempty"` // 窗口长度；和 ResetsAt 一起算节奏，0 表示不知道
	// 按额度计的窗口（例如 credits）：Amount 是剩下的量，Limit 是总量，Unit 是单位。
	Amount float64 `json:"amount,omitempty"`
	Limit  float64 `json:"limit,omitempty"`
	Unit   string  `json:"unit,omitempty"`
	// 有一部分额度会先到期：RunsOut 是这部分的量，RunsOutAt 是到期时间。
	RunsOut   float64 `json:"runsOut,omitempty"`
	RunsOutAt string  `json:"runsOutAt,omitempty"`
}

type Account struct {
	Source string `json:"source"` // codex 或 plugin
	Agent  string `json:"agent"`
	// Provider 是插件订阅的 provider id（codex 时为空），账号页按它把额度放到对应的那一行。
	Provider string   `json:"provider,omitempty"`
	User     string   `json:"user"`
	Plan     string   `json:"plan"`
	Until    string   `json:"until,omitempty"` // 套餐到期的时间，来源给了才有
	Active   bool     `json:"active"`
	On       bool     `json:"on"`
	Windows  []Window `json:"windows"`
	ReadAt   string   `json:"readAt"`
	Balance  string   `json:"balance,omitempty"` // 来源给的余额文字，例如「余额 120 credits」
	Error    string   `json:"error,omitempty"`   // 这个账号的额度读失败的原因
}

// source 读一个来源。返回的 error 是整个来源读不了；单个账号读失败写在 Account.Error 里。
type source struct {
	name string
	read func(ctx context.Context) ([]Account, error)
}

var sources = []source{{"codex", readCodex}, {"插件", readPlugins}}

const ttl = 60 * time.Second

var (
	mu     sync.Mutex
	cached []Account
	errs   []string
	readAt time.Time
)

// Accounts 返回所有来源的账号；60 秒内重复调用直接用缓存，force 时重新读。
// errs 是读不了的来源和原因，一个来源读不了不影响别的。
func Accounts(force bool) ([]Account, time.Time, []string) {
	mu.Lock()
	defer mu.Unlock()
	if !force && !readAt.IsZero() && time.Since(readAt) < ttl {
		return cached, readAt, errs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	list, bad := []Account{}, []string{}
	for _, s := range sources {
		as, err := s.read(ctx)
		if err != nil {
			bad = append(bad, s.name+"："+err.Error())
		}
		list = append(list, as...)
	}
	cached, errs, readAt = list, bad, time.Now()
	return cached, readAt, errs
}
