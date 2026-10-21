package gui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// 打开链接：网页原样；路径去掉行号；能运行的只在访达里选中；不存在的、相对路径、别的协议拒绝。
func TestOpenArgs(t *testing.T) {
	dir := t.TempDir()
	doc := filepath.Join(dir, "数据 方案.html")
	script := filepath.Join(dir, "run.command")
	bin := filepath.Join(dir, "tool")
	os.WriteFile(doc, []byte("x"), 0o600)
	os.WriteFile(script, []byte("x"), 0o600)
	os.WriteFile(bin, []byte("x"), 0o700)
	cases := []struct {
		link string
		want []string
	}{
		{"https://example.com/a?b=1", []string{"https://example.com/a?b=1"}},
		{doc, []string{doc}},
		{doc + ":173", []string{doc}},
		{doc + ":12:5", []string{doc}},
		{doc + "#L40", []string{doc}},
		{"file://" + doc, []string{doc}},
		{dir, []string{dir}},
		{script, []string{"-R", script}},
		{bin, []string{"-R", bin}},
	}
	for _, c := range cases {
		got, err := openArgs(c.link)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("openArgs(%q) = %v %v，应为 %v", c.link, got, err, c.want)
		}
	}
	for _, bad := range []string{"a.html", filepath.Join(dir, "没有.html:3"), "javascript:alert(1)", "ftp://x/y", ""} {
		if got, err := openArgs(bad); err == nil {
			t.Errorf("openArgs(%q) 应拒绝，得到 %v", bad, got)
		}
	}
}
