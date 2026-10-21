package store

import (
	"encoding/json"
	"slices"
)

// 启动流程里用户的选择：勾了哪些 AI 工具、每个工具勾了哪些模型。没勾的工具和模型在 morula 里不出现。
// 存在 settings 表：tools.on 是工具名的 JSON 数组；models.<工具> 是模型 id 的 JSON 数组。
// 没设置过时全部算勾上。

// Tools 返回勾上的工具，按 Runtimes 的顺序。没设置过时返回全部 Runtimes。
func (s *Store) Tools() []string {
	var on []string
	if json.Unmarshal([]byte(s.Setting("tools.on", "")), &on) != nil {
		return slices.Clone(Runtimes)
	}
	return slices.DeleteFunc(slices.Clone(Runtimes), func(n string) bool { return !slices.Contains(on, n) })
}

// SetTools 记下勾上的工具。不认识的名字丢掉。
func (s *Store) SetTools(on []string) error {
	keep := slices.DeleteFunc(slices.Clone(on), func(n string) bool { return !slices.Contains(Runtimes, n) })
	b, _ := json.Marshal(keep)
	return s.SetSetting("tools.on", string(b))
}

// Picked 返回这个工具勾上的模型 id；set 为 false 表示没设置过，全部算勾上。
func (s *Store) Picked(tool string) (ids []string, set bool) {
	if json.Unmarshal([]byte(s.Setting("models."+tool, "")), &ids) != nil {
		return nil, false
	}
	return ids, true
}

// SetPicked 记下这个工具勾上的模型。
func (s *Store) SetPicked(tool string, ids []string) error {
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(ids)
	return s.SetSetting("models."+tool, string(b))
}

// Choice 返回勾上的工具，以及设置过的工具各勾了哪些模型，交给 harness.Chosen 过滤。
func (s *Store) Choice() ([]string, map[string][]string) {
	pick := map[string][]string{}
	for _, n := range Runtimes {
		if ids, ok := s.Picked(n); ok {
			pick[n] = ids
		}
	}
	return s.Tools(), pick
}
