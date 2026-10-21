package work

import (
	"strconv"
	"sync"

	"morula/internal/store"
)

// 默认项目：闲聊、问答和不属于其他项目的零散小事归到这里，
// 不用问用户，也不新建项目。第一次用到时建一个「日常」；用户可以把别的项目设成默认。
// 用户删掉默认项目后不再自动建，没有默认项目时这类消息不归任何项目。

const (
	DefaultProjectName    = "日常"
	DefaultProjectSummary = "闲聊、问答和不属于其他项目的零散小事"
)

var defaultMu sync.Mutex

// DefaultProject 返回默认项目，没有时返回 nil。第一次调用时建「日常」。
func (e *Engine) DefaultProject() *store.Project {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if e.DB.Setting("project.default.seeded", "") == "" {
		e.DB.SetSetting("project.default.seeded", "1")
		p, err := e.CreateProject(DefaultProjectName, DefaultProjectSummary)
		if err != nil { // 已有同名项目时用它
			ps, _ := e.DB.Projects()
			for i := range ps {
				if ps[i].Name == DefaultProjectName {
					p, err = &ps[i], nil
				}
			}
		}
		if err == nil {
			e.DB.SetSetting("project.default", strconv.FormatInt(p.ID, 10))
		}
	}
	id, _ := strconv.ParseInt(e.DB.Setting("project.default", ""), 10, 64)
	if id == 0 {
		return nil
	}
	p, _ := e.DB.Project(id)
	return p
}

// SetDefaultProject 把 id 设成默认项目。
func (e *Engine) SetDefaultProject(id int64) error {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	e.DB.SetSetting("project.default.seeded", "1")
	return e.DB.SetSetting("project.default", strconv.FormatInt(id, 10))
}
