package work

import (
	"testing"
	"time"

	"morula/internal/store"
)

func TestRoutineSlots(t *testing.T) {
	loc := time.FixedZone("x", 8*3600)
	mon := time.Date(2026, 10, 19, 9, 0, 0, 0, loc) // 星期一 09:00
	weekly := store.Routine{Days: 1 << 1, At: "10:00", UpdatedAt: "2026-10-01T00:00:00+08:00"}
	if got := NextSlot(weekly, mon); !got.Equal(time.Date(2026, 10, 19, 10, 0, 0, 0, loc)) {
		t.Errorf("下次：%v", got)
	}
	if got := LastSlot(weekly, mon); !got.Equal(time.Date(2026, 10, 12, 10, 0, 0, 0, loc)) {
		t.Errorf("上次：%v", got)
	}
	if !dueSlot(weekly, mon).IsZero() {
		t.Error("一周前的点不该补跑")
	}
	daily := store.Routine{Days: 127, At: "08:30", UpdatedAt: "2026-10-01T00:00:00+08:00"}
	if got := dueSlot(daily, mon); !got.Equal(time.Date(2026, 10, 19, 8, 30, 0, 0, loc)) {
		t.Errorf("半小时前的点应补跑：%v", got)
	}
	// 醒来时离点超过 12 小时：不补
	if !dueSlot(daily, time.Date(2026, 10, 19, 21, 0, 0, 0, loc)).IsZero() {
		t.Error("超过 12 小时不该补跑")
	}
	// 点在设好之前：不跑
	daily.UpdatedAt = "2026-10-19T08:45:00+08:00"
	if !dueSlot(daily, mon).IsZero() {
		t.Error("设好之前的点不该跑")
	}
}
