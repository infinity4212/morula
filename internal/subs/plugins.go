package subs

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"morula/internal/plugin"
)

// usage 是插件 usage 钩子的返回。
// 窗口长度可以给 minutes，也可以给 span（秒）。balance 可以是文字或数字。
type usage struct {
	Plan    string `json:"plan"`
	Until   string `json:"until"`  // 套餐到期的时间
	SignIn  string `json:"signIn"` // expired：登录已过期
	Error   string `json:"error"`
	Balance any    `json:"balance"`
	Windows []struct {
		Name      string  `json:"name"`
		Used      float64 `json:"used"`
		Display   string  `json:"display"`
		ResetsAt  string  `json:"resetsAt"`
		Minutes   int     `json:"minutes"`
		Span      int     `json:"span"`
		Amount    float64 `json:"amount"`
		Limit     float64 `json:"limit"`
		Unit      string  `json:"unit"`
		RunsOut   float64 `json:"runsOut"`
		RunsOutAt string  `json:"runsOutAt"`
	} `json:"windows"`
}

// balanceText 把 balance 换成文字；没有时为空。
func balanceText(v any) string {
	switch b := v.(type) {
	case nil:
		return ""
	case string:
		return b
	case float64:
		return strconv.FormatFloat(b, 'f', -1, 64)
	default:
		bs, _ := json.Marshal(b)
		return string(bs)
	}
}

// readPlugins 对每个已登录、有 usage 钩子的插件 provider 读额度。没有启用的插件时不启动插件运行器。
func readPlugins(ctx context.Context) ([]Account, error) {
	ps, err := plugin.Catalog(ctx, false)
	var want []plugin.Provider
	for _, p := range ps {
		if !p.Builtin && p.SignedIn {
			want = append(want, p)
		}
	}
	if len(want) == 0 {
		return nil, err
	}
	h, herr := plugin.Get(ctx)
	if herr != nil {
		return nil, herr
	}
	out := []Account{}
	for _, p := range want {
		a := Account{Source: "plugin", Agent: p.Name, Provider: p.ID, User: p.Account, Active: true, On: true, Windows: []Window{}, ReadAt: time.Now().UTC().Format(time.RFC3339)}
		if a.Agent == "" {
			a.Agent = p.ID
		}
		if !p.Usage {
			a.Error = "这个插件不提供额度"
			out = append(out, a)
			continue
		}
		var u *usage
		if err := h.Call(ctx, "usage", map[string]string{"provider": p.ID}, &u); err != nil {
			a.Error = err.Error()
		} else if u != nil {
			a.Plan, a.Until, a.Error, a.Balance = u.Plan, u.Until, u.Error, balanceText(u.Balance)
			if u.SignIn == "expired" {
				a.Error = "登录过期了，要退出后重新登录"
			}
			for _, w := range u.Windows {
				if w.Minutes == 0 && w.Span > 0 {
					w.Minutes = w.Span / 60
				}
				a.Windows = append(a.Windows, Window{Name: w.Name, Used: w.Used, Remaining: max(0, 100-w.Used), Display: w.Display, ResetsAt: w.ResetsAt, Minutes: w.Minutes,
					Amount: w.Amount, Limit: w.Limit, Unit: w.Unit, RunsOut: w.RunsOut, RunsOutAt: w.RunsOutAt})
			}
		}
		out = append(out, a)
	}
	return out, err
}
