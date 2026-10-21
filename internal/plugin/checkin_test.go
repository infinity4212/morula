package plugin

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCheckin 用一个假插件走一遍每日签到：列出来源时标出有签到，签到结果记进 checkin.json，
// 今天签过的不再签，失败的原因记下来。要装了 Bun，没装时跳过。
func TestCheckin(t *testing.T) {
	if _, err := Bun(); err != nil {
		t.Skip("没装 Bun")
	}
	home := t.TempDir()
	t.Setenv("MORULA_HOME", home)
	Restart()
	Forget()
	t.Cleanup(func() { Restart(); Forget() })

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "node_modules", "fake")
	os.MkdirAll(pkg, 0o755)
	// 第一次签到领到 100，之后回今天已领过；fail 来源总是失败
	src := `let n = 0
export const Fake = async () => ({
  auth: { provider: "fake", methods: [], checkin: async (getAuth) => (await getAuth()) ? (n++ ? { outcome: "done" } : { outcome: "claimed", credit: 100 }) : { outcome: "failed", message: "no auth" } },
})
export const Fail = async () => ({
  auth: { provider: "fail", methods: [], checkin: async () => ({ outcome: "failed", message: "quota page refused", signIn: "expired" }) },
})
export const Plain = async () => ({ auth: { provider: "plain", methods: [] } })
`
	os.WriteFile(filepath.Join(pkg, "index.mjs"), []byte(src), 0o644)
	os.WriteFile(filepath.Join(home, "plugins.json"), []byte(`{"plugins":[{"spec":"fake","name":"fake"}]}`), 0o644)
	os.WriteFile(filepath.Join(home, "plugin-auth.json"), []byte(`{"fake":{"type":"oauth","access":"x"},"fail":{"type":"oauth","access":"y"},"plain":{"type":"oauth","access":"z"}}`), 0o600)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ps, err := Catalog(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, p := range ps {
		has[p.ID] = p.Checkin
	}
	if !has["fake"] || !has["fail"] || has["plain"] {
		t.Fatalf("签到标记不对：%v", has)
	}

	// 没打开时不签
	CheckinDue(ctx)
	if len(Checkins()) != 0 {
		t.Fatalf("没打开就签了：%v", Checkins())
	}
	SetCheckin("fake", true)
	SetCheckin("fail", true)
	CheckinDue(ctx)
	got := Checkins()
	today := time.Now().Format(time.DateOnly)
	if f := got["fake"]; f.Outcome != "claimed" || f.Credit != 100 || f.Day != today || !f.On {
		t.Fatalf("fake 的结果不对：%+v", f)
	}
	if f := got["fail"]; f.Outcome != "failed" || f.Day != "" || f.Message != "登录过期了，要退出后重新登录" {
		t.Fatalf("fail 的结果不对：%+v", f)
	}

	// 今天签过的、刚失败的都不再签：fake 的第二次会回 done，结果没变说明没再调
	CheckinDue(ctx)
	if f := Checkins()["fake"]; f.Outcome != "claimed" {
		t.Fatalf("今天签过还又签了：%+v", f)
	}
	// 手动签到照常调插件
	if s, err := Checkin(ctx, "fake"); err != nil || s.Outcome != "done" {
		t.Fatalf("手动签到：%+v %v", s, err)
	}
	// 没有签到钩子的来源报错，错误记进状态
	if s, err := Checkin(ctx, "plain"); err == nil || s.Outcome != "failed" {
		t.Fatalf("plain 应该失败：%+v %v", s, err)
	}
}
