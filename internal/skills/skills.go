// Package skills 列出本机装的 skill：一个目录里有 SKILL.md，开头的 frontmatter 写 name 和 description。
// 只读，不改 skill 文件。
package skills

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Skill 是本机的一个 skill。
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`   // SKILL.md 所在目录，已解开软链接
	Source      string `json:"source"` // 从哪个目录读到的，例如 ~/.codex/skills
}

// ExtraDirs 返回排在全局目录后面的 skill 目录，morula 用它加上各项目目录里的 .agents/skills：
// 子 agent 只能写项目目录，装 skill 时常装在那里。nil 表示没有。
var ExtraDirs func() []string

// Dirs 是读 skill 的目录，按优先级排：同名 skill 只留排在前面的那个。
// codex 自己加载 CODEX_HOME/skills；其他目录里的 skill 只能靠 morula 在提示词里写出路径。
func Dirs() []string {
	home, _ := os.UserHomeDir()
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	out := []string{filepath.Join(codex, "skills"), filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".workbuddy", "skills")}
	if ExtraDirs != nil {
		out = append(out, ExtraDirs()...)
	}
	return out
}

// backup 匹配手动备份的副本，例如 mtdr-target-discovery.backup-20260804-131421。
var backup = regexp.MustCompile(`\.(backup|bak|old)([-_.]|$)`)

// List 读 Dirs 里的全部 skill。没有 SKILL.md、没有 name 的目录跳过；备份副本跳过。
func List() []Skill {
	home, _ := os.UserHomeDir()
	seen := map[string]bool{}
	out := []Skill{}
	for _, d := range Dirs() {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		src := d
		if home != "" && strings.HasPrefix(d, home+"/") {
			src = "~" + strings.TrimPrefix(d, home)
		}
		for _, e := range ents {
			if backup.MatchString(e.Name()) {
				continue
			}
			dir, err := filepath.EvalSymlinks(filepath.Join(d, e.Name()))
			if err != nil {
				continue
			}
			s, ok := read(filepath.Join(dir, "SKILL.md"))
			if !ok || seen[s.Name] {
				continue
			}
			seen[s.Name] = true
			s.Path, s.Source = dir, src
			out = append(out, s)
		}
	}
	return out
}

// Find 按名字找 skill。
func Find(list []Skill, name string) (Skill, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Skill{}, false
}

// read 读 frontmatter 里的 name 和 description。只认这两个顶层键；description 可以是
// 单行，也可以是 > 或 | 开头的多行块。
func read(p string) (Skill, bool) {
	f, err := os.Open(p)
	if err != nil {
		return Skill{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return Skill{}, false
	}
	var s Skill
	var block *string // 正在读的多行值
	var parts []string
	flush := func() {
		if block != nil {
			*block = strings.Join(parts, " ")
			block, parts = nil, nil
		}
	}
	for i := 0; i < 200 && sc.Scan(); i++ {
		l := sc.Text()
		if strings.TrimSpace(l) == "---" {
			break
		}
		if block != nil && (strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") || strings.TrimSpace(l) == "") {
			if t := strings.TrimSpace(l); t != "" {
				parts = append(parts, t)
			}
			continue
		}
		flush()
		k, v, ok := strings.Cut(l, ":")
		if !ok || strings.HasPrefix(l, " ") {
			continue
		}
		var dst *string
		switch strings.TrimSpace(k) {
		case "name":
			dst = &s.Name
		case "description":
			dst = &s.Description
		default:
			continue
		}
		v = strings.TrimSpace(v)
		if v == "" || strings.HasPrefix(v, ">") || strings.HasPrefix(v, "|") {
			block = dst
			continue
		}
		*dst = strings.Trim(v, `"'`)
	}
	flush()
	s.Name = strings.TrimSpace(s.Name)
	return s, s.Name != ""
}
