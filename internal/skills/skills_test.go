package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestList 检查单行和多行 description、备份副本跳过、软链接解开、同名按目录优先级只留一个。
func TestList(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "cx"))
	codex := filepath.Join(home, "cx", "skills")
	agents := filepath.Join(home, ".agents", "skills")
	wb := filepath.Join(home, ".workbuddy", "skills")

	write(t, filepath.Join(codex, "a"), "---\nname: alpha\ndescription: \"单行说明\"\n---\n正文\n")
	write(t, filepath.Join(codex, "a.backup-20260101"), "---\nname: alpha-old\ndescription: 备份\n---\n")
	write(t, filepath.Join(wb, "b"), "---\nname: beta\ndescription: >\n  第一行\n  第二行\nmetadata:\n  x: 1\n---\n")
	write(t, filepath.Join(wb, "dup"), "---\nname: alpha\ndescription: 排在后面的同名 skill\n---\n")
	write(t, filepath.Join(wb, "noname"), "---\ndescription: 没有名字\n---\n")
	os.MkdirAll(agents, 0o755)
	if err := os.Symlink(filepath.Join(wb, "b"), filepath.Join(agents, "b-link")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(wb, "x.zip"), []byte("zip"), 0o644)

	list := List()
	if len(list) != 2 {
		t.Fatalf("应有 2 个 skill，得到 %+v", list)
	}
	a, ok := Find(list, "alpha")
	if !ok || a.Description != "单行说明" || a.Source != "~/cx/skills" {
		t.Fatalf("alpha：%+v", a)
	}
	b, ok := Find(list, "beta")
	real, _ := filepath.EvalSymlinks(filepath.Join(wb, "b"))
	if !ok || b.Description != "第一行 第二行" || b.Path != real || b.Source != "~/.agents/skills" {
		t.Fatalf("beta：%+v", b)
	}
	if _, ok := Find(list, "alpha-old"); ok {
		t.Fatal("备份副本不应列出")
	}
}
