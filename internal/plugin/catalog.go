package plugin

import (
	"context"
	"strings"
	"sync"
	"time"
)

// 插件列模型要问上游，结果缓存 5 分钟；登录、退出、增删插件后调 Forget 清掉。
var (
	catMu  sync.Mutex
	catAt  time.Time
	catVal []Provider
)

const catTTL = 5 * time.Minute

// Catalog 返回所有 provider 和模型：先列插件的，再列 API key 来源，最后是 codex 登录的 ChatGPT 订阅。refresh 为真时不用缓存。
// 没有启用的插件时不启动插件运行器。插件运行器出错时仍返回 API key 来源，同时返回错误。
func Catalog(ctx context.Context, refresh bool) ([]Provider, error) {
	catMu.Lock()
	defer catMu.Unlock()
	if !refresh && catVal != nil && time.Since(catAt) < catTTL {
		return catVal, nil
	}
	list := []Provider{}
	var err error
	if enabled() {
		var h *Host
		if h, err = Get(ctx); err == nil {
			list, err = h.Providers(ctx)
		}
	}
	list = append(list, apiProviders()...)
	list = append(list, chatgptProvider()...)
	if err != nil {
		return list, err
	}
	catVal, catAt = list, time.Now()
	return list, nil
}

func enabled() bool {
	l, _ := List()
	for _, e := range l {
		if !e.Off {
			return true
		}
	}
	return false
}

// Cached 只读缓存，不启动 host；没有缓存时返回 nil。
func Cached() []Provider {
	catMu.Lock()
	defer catMu.Unlock()
	return catVal
}

// Forget 清掉缓存。
func Forget() {
	catMu.Lock()
	catVal = nil
	catMu.Unlock()
}

// ModelRef 是 agent 配置里的插件模型，写作 <provider>/<model>。
type ModelRef struct {
	Provider Provider
	Model    Model
}

// ID 是 morula 里用的模型名。
func (r ModelRef) ID() string { return r.Provider.ID + "/" + r.Model.ID }

// Find 按 <provider>/<model> 找插件模型。只看已登录的 provider。
func Find(ctx context.Context, id string) (ModelRef, bool) {
	list, _ := Catalog(ctx, false)
	for _, p := range list {
		if !p.SignedIn || !strings.HasPrefix(id, p.ID+"/") {
			continue
		}
		want := strings.TrimPrefix(id, p.ID+"/")
		for _, m := range p.Models {
			if m.ID == want {
				return ModelRef{p, m}, true
			}
		}
	}
	return ModelRef{}, false
}

// IsPluginModel 判断模型名是不是 <provider>/<model> 的写法，且 provider 由插件提供。
// 只读缓存：没有缓存时按写法判断（含 /）。
func IsPluginModel(id string) bool {
	if !strings.Contains(id, "/") {
		return false
	}
	list := Cached()
	if list == nil {
		return true
	}
	for _, p := range list {
		if strings.HasPrefix(id, p.ID+"/") {
			return true
		}
	}
	return false
}
