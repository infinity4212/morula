package gui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFiles(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	f, err := saveFile("../../截图 1.png", bytes.NewReader(png))
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "截图 1.png" || !f.Image || f.Size != int64(len(png)) {
		t.Errorf("附件 = %+v", f)
	}
	root, _ := filesDir()
	if !strings.HasPrefix(f.Path, root+string(filepath.Separator)) {
		t.Errorf("附件存到了附件目录外：%s", f.Path)
	}
	got, err := attachments([]string{f.ID})
	if err != nil || len(got) != 1 || got[0].Path != f.Path {
		t.Errorf("按 id 取附件 = %+v, %v", got, err)
	}
	// 改名成 .png 的文本不算图片
	txt, _ := saveFile("假图.png", strings.NewReader("<html><script>x</script>"))
	if txt.Image {
		t.Error("内容不是图片的文件被当成了图片")
	}
	for _, id := range []string{"../morula.db", "/etc/passwd", "x/../../morula.db", ""} {
		if _, err := attachments([]string{id}); err == nil {
			t.Errorf("id %q 应该被拒绝", id)
		}
	}
}

func TestFileTooLarge(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	if _, err := saveFile("big.bin", bytes.NewReader(make([]byte, maxFile+1))); err == nil {
		t.Fatal("超过上限的文件应该被拒绝")
	}
	root, _ := filesDir()
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*"))
	for _, m := range matches {
		if st, _ := os.Stat(m); st != nil && st.Mode().IsRegular() {
			t.Errorf("写了一半的文件没删：%s", m)
		}
	}
}

func TestEmptyAndCopy(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	if _, err := saveFile("空.html", strings.NewReader("")); err == nil {
		t.Error("空内容的文件不该存下")
	}
	src := filepath.Join(t.TempDir(), "方案.html")
	os.WriteFile(src, []byte("<p>16 KB 的方案</p>"), 0o600)
	f, err := copyFile(src)
	if err != nil || f.Name != "方案.html" || f.Size == 0 {
		t.Errorf("按路径复制 = %+v, %v", f, err)
	}
	if _, err := copyFile(filepath.Dir(src)); err == nil {
		t.Error("文件夹不该被当成文件")
	}
}
