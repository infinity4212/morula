package store

import "testing"

func TestRoutines(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, bad := range []Routine{
		{Name: "", Prompt: "做", Days: 127, At: "08:30"},
		{Name: "a", Prompt: "做", Days: 0, At: "08:30"},
		{Name: "a", Prompt: "做", Days: 127, At: "8:30"},
		{Name: "a", Prompt: "做", Days: 127, At: "24:00"},
	} {
		if _, err := db.AddRoutine(bad); err == nil {
			t.Errorf("应拒收 %+v", bad)
		}
	}
	r, err := db.AddRoutine(Routine{Name: "晨报", Prompt: "汇总昨天", Days: 127, At: "08:30", Enabled: true})
	if err != nil || r == nil || !r.Enabled {
		t.Fatalf("新建：%v %+v", err, r)
	}
	id, _ := db.AddRoutineRun(r.ID, "2026-10-20T08:30:00+08:00")
	if again, _ := db.AddRoutineRun(r.ID, "2026-10-20T08:30:00+08:00"); id == 0 || again != 0 {
		t.Fatalf("同一个时间点应只记一次：%d %d", id, again)
	}
	db.SetRoutineTurn(id, "m1")
	db.Recover()
	list, _ := db.Routines()
	if len(list) != 1 || list[0].Last == nil || list[0].Last.Status != "stopped" || list[0].Last.Turn != "m1" {
		t.Fatalf("重启后没做完的应记为停止：%+v", list[0].Last)
	}
	r.Enabled = false
	if u, err := db.UpdateRoutine(*r); err != nil || u.Enabled {
		t.Fatalf("关掉：%v %+v", err, u)
	}
	db.RemoveRoutine(r.ID)
	if runs, _ := db.RoutineRuns(0, 10); len(runs) != 0 {
		t.Fatalf("删掉事项后记录应一起删：%d", len(runs))
	}
}
