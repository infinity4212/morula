package work

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestDanger 检查清单文件：不存在时写默认清单，注释和空行不算，改了以后子 agent 规则跟着变。
func TestDanger(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	list, err := Danger()
	if err != nil || !slices.Equal(list, DefaultDanger) {
		t.Fatalf("默认清单：%v %v", list, err)
	}
	p, _ := DangerPath()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("默认清单要写成文件：%v", err)
	}
	if err := os.WriteFile(p, []byte("# 注释\n\n删除文件\n  装新软件  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if list, _ = Danger(); !slices.Equal(list, []string{"删除文件", "装新软件"}) {
		t.Fatalf("直接改文件后：%v", list)
	}
	if r := dangerRule(); !strings.Contains(r, "删除文件；装新软件") || strings.Contains(r, "强制推送") {
		t.Fatalf("规则：%s", r)
	}
	if err := SaveDanger([]string{"a", " ", "a", "#x", "b  c"}); err != nil {
		t.Fatal(err)
	}
	if list, _ = Danger(); !slices.Equal(list, []string{"a", "b c"}) {
		t.Fatalf("保存后：%v", list)
	}
	if err := SaveDanger(nil); err != nil {
		t.Fatal(err)
	}
	if list, _ = Danger(); len(list) != 0 || !strings.Contains(dangerRule(), "不必先上报") {
		t.Fatalf("清空后：%v %s", list, dangerRule())
	}
}

// 三个档位的第 3 条各不相同；删除这类操作在按清单问和全部放行时都要上报，用户从清单里删掉也一样。
func TestTierRule(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	if err := SaveDanger([]string{"装新软件"}); err != nil {
		t.Fatal(err)
	}
	ask, list, all := tierRule(TierAsk), tierRule(TierList), tierRule(TierAll)
	if ask == list || list == all || ask == all {
		t.Fatal("三个档位的规则应各不相同")
	}
	for _, r := range []string{list, all} {
		if !strings.Contains(r, "删除文件或目录") || !strings.Contains(r, "强制推送") || !strings.Contains(r, "status=approval") {
			t.Errorf("删除和改写历史要上报：%s", r)
		}
	}
	if !strings.Contains(list, "装新软件") || !strings.Contains(all, "清单上的事直接做") || strings.Contains(all, "装新软件") {
		t.Errorf("清单只在按清单问时上报：\n%s\n%s", list, all)
	}
}
