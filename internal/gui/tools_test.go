package gui

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"morula/internal/store"
)

// 「设置 / 账号」里 codex 一行取消勾选，改的是 codex 这个工具勾的模型；经网关借来的模型勾没勾不变。
func TestCodexOff(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cx := t.TempDir()
	t.Setenv("CODEX_HOME", cx)
	cache := `{"models":[{"slug":"a","visibility":"list"},{"slug":"b","visibility":"list"}]}`
	if err := os.WriteFile(filepath.Join(cx, "models_cache.json"), []byte(cache), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	if db, err = store.Open(); err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if off := codexOff(); off != nil {
		t.Fatalf("没设过时是 %v，要是 nil", off)
	}
	db.SetPicked("codex", []string{"a", "b", "src/x"})
	if err := setCodexOff("codex", []string{"b"}); err != nil {
		t.Fatal(err)
	}
	ids, _ := db.Picked("codex")
	if !slices.Equal(ids, []string{"a", "src/x"}) {
		t.Errorf("勾的模型是 %v，要是 [a src/x]", ids)
	}
	if off := codexOff(); !slices.Equal(off, []string{"b"}) {
		t.Errorf("codexOff = %v，要是 [b]", off)
	}
}
