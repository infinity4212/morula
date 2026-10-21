package harness

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// sessionFiler 由把会话存成文件的适配器实现，返回这个会话的文件路径；找不到时返回空。
type sessionFiler interface{ sessionFile(session string) string }

var exportDir struct {
	sync.Mutex
	dir string
}

// SetExportDir 设 morula 导出会话的目录（<数据目录>/sessions）。harness 不把会话存成文件时，SessionFile 导出到这里。
func SetExportDir(dir string) {
	exportDir.Lock()
	exportDir.dir = dir
	exportDir.Unlock()
}

// SessionFile 是这个会话的文件路径，助理翻旧会话用。harness 存了文件时用它的文件；
// 没存文件时（例如 opencode 存在 sqlite 里），用 Trajectory 导出成 <导出目录>/<harness>/<会话 id>.md，每次调用都重新导出。
// 找不到会话、或没设导出目录时返回空。
func SessionFile(runtime, session string) string {
	a, err := Get(runtime)
	if err != nil || session == "" {
		return ""
	}
	if f, ok := a.(sessionFiler); ok {
		if p := f.sessionFile(session); p != "" {
			return p
		}
	}
	exportDir.Lock()
	dir := exportDir.dir
	exportDir.Unlock()
	if dir == "" || strings.ContainsAny(session, `/\`) {
		return ""
	}
	steps, err := a.Trajectory(session)
	if err != nil || len(steps) == 0 {
		return ""
	}
	p := filepath.Join(dir, runtime, session+".md")
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil || os.WriteFile(p, []byte(transcript(runtime, session, steps)), 0o644) != nil {
		return ""
	}
	return p
}

// stepTitle 是导出文件里每一步的小标题。
var stepTitle = map[string]string{"user": "用户的提示词", "agent": "回复", "tool": "调用工具", "output": "工具输出"}

// transcript 把轨迹写成 Markdown：每一步一个小标题，后面是原文。
func transcript(runtime, session string, steps []Step) string {
	var b strings.Builder
	b.WriteString("# " + runtime + " 会话 " + session + "\n\nmorula 从 " + runtime + " 导出的会话原文。\n")
	for _, s := range steps {
		t := stepTitle[s.Role]
		if t == "" {
			t = s.Role
		}
		if s.TS != "" {
			t += "（" + s.TS + "）"
		}
		b.WriteString("\n## " + t + "\n\n" + strings.TrimSpace(s.Text) + "\n")
	}
	return b.String()
}
