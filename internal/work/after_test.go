package work

import "testing"

// 后续按安排记：取出后清掉，空的不记。
func TestAfter(t *testing.T) {
	e := staffEngine(t)
	p, _ := e.DB.AddProject("一", "", t.TempDir())
	run, _ := e.DB.AddRun(p.ID, "装 skill")
	e.setAfter(run, "  用它建 agent ")
	if got := e.peekAfter(run); got != "用它建 agent" {
		t.Fatalf("peekAfter = %q", got)
	}
	if got := e.takeAfter(run); got != "用它建 agent" {
		t.Fatalf("takeAfter = %q", got)
	}
	if got := e.takeAfter(run); got != "" {
		t.Fatalf("取出后应清掉，得到 %q", got)
	}
	e.setAfter(run, " ")
	if got := e.peekAfter(run); got != "" {
		t.Fatalf("空的后续不记，得到 %q", got)
	}
}
