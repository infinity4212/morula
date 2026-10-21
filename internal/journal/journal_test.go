package journal

import "testing"

// 整理项目的记录写进对话记录，助理读得到；读回的数字是 float64。
func TestLineReorg(t *testing.T) {
	cases := []struct {
		e    map[string]any
		want string
	}{
		{map[string]any{"type": "reorg", "op": "rename", "old": "甲", "name": "乙"}, "[项目改名] 「甲」改为「乙」"},
		{map[string]any{"type": "reorg", "op": "merge", "srcName": "甲", "name": "乙", "merge": float64(3)}, "[合并项目] 「甲」并进「乙」，合并编号 3"},
		{map[string]any{"type": "reorg", "op": "unmerge", "srcName": "甲", "name": "乙"}, "[撤销合并] 「甲」从「乙」分了出来"},
		{map[string]any{"type": "reorg", "op": "move", "runs": 2, "name": "乙"}, "[挪安排] 2 次安排挪到「乙」"},
		{map[string]any{"type": "reorg", "op": "别的"}, ""},
	}
	for _, c := range cases {
		if got := Line(c.e); got != c.want {
			t.Errorf("%v：得到 %q，要 %q", c.e, got, c.want)
		}
	}
}
