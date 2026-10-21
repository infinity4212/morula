package store

import (
	"slices"
	"testing"
)

// 没选过时全部工具、全部模型都算勾上；选过以后只留勾上的，不认识的工具名丢掉。
func TestChoice(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if on, pick := db.Choice(); !slices.Equal(on, Runtimes) || len(pick) != 0 {
		t.Fatalf("没选过时是 %v %v", on, pick)
	}
	db.SetTools([]string{"workbuddy", "codex", "claude"})
	db.SetPicked("codex", nil)
	db.SetPicked("workbuddy", []string{"hy3"})
	on, pick := db.Choice()
	if !slices.Equal(on, []string{"codex", "workbuddy"}) {
		t.Errorf("勾上的工具是 %v", on)
	}
	if ids, ok := pick["codex"]; !ok || len(ids) != 0 {
		t.Errorf("codex 一个都没勾，读出 %v %v", ids, ok)
	}
	if !slices.Equal(pick["workbuddy"], []string{"hy3"}) {
		t.Errorf("workbuddy 勾的是 %v", pick["workbuddy"])
	}
	if _, ok := pick["opencode"]; ok {
		t.Error("opencode 没选过，不该有记录")
	}
}
