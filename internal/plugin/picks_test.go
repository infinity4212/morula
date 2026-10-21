package plugin

import (
	"slices"
	"testing"
)

func TestModelsOff(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir()) // 不读用户的 codex 登录信息
	if len(OffModels()) != 0 || ModelOff("demo", "a") {
		t.Fatal("新库不该有取消勾选的模型")
	}
	if err := SetModelsOff("demo", []string{"b", "a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got := OffModels()["demo"]; !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("OffModels = %v", got)
	}
	if !ModelOff("demo", "a") || ModelOff("codex", "a") {
		t.Fatal("ModelOff 按来源分开")
	}
	if err := SetModelsOff("demo", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := OffModels()["demo"]; ok {
		t.Fatal("全选后应删掉这个来源的记录")
	}
	if SetModelsOff("", []string{"a"}) == nil {
		t.Fatal("缺来源应报错")
	}
}

func TestSearchFilter(t *testing.T) {
	for name, want := range map[string]bool{
		"opencode-qoder-auth": true, "@acme/opencode-grok-auth": false,
		"opencode-kiro": false, "@velocirouter/opencode-auth": false, "pegasus-opencode": false,
	} {
		if authName.MatchString(name) != want {
			t.Errorf("%s 应为 %v", name, want)
		}
	}
	for spec, want := range map[string]bool{"/tmp/x": true, "~/code/p": true, "./p": true, "file:../p": true, "opencode-foo-auth@1.2.0": false, "@a/b": false} {
		if local(spec) != want {
			t.Errorf("local(%s) 应为 %v", spec, want)
		}
	}
}
