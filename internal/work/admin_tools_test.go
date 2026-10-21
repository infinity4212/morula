package work

import "testing"

func TestDaysText(t *testing.T) {
	for d, want := range map[int]string{127: "每天", 62: "工作日", 65: "周末", 1<<1 | 1<<3: "周一、周三", 1: "周日"} {
		if got := daysText(d); got != want {
			t.Errorf("daysText(%d) = %q, want %q", d, got, want)
		}
	}
}
