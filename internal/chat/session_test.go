package chat

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"morula/internal/journal"
)

// 清空对话页：页面上只剩 clear，助理读的最近对话和重启后读回的对话都从清空之后开始，当天记录还在。
func TestClear(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	s.Emit(Event{"type": "user", "text": "清空前说的话"})
	before := s.epoch
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if s.epoch == before {
		t.Error("清空后 epoch 没变，前端不会重画")
	}
	if len(s.events) != 1 || s.events[0]["type"] != "clear" {
		t.Errorf("清空后的事件 = %v", s.events)
	}
	s.Emit(Event{"type": "user", "text": "清空后说的话", "files": []File{{Name: "a.png", Path: "/x/a.png"}}})

	day := journal.Today()
	all, _ := journal.Transcript(day, 0)
	if !strings.Contains(all, "清空前说的话") {
		t.Error("当天记录里应该还有清空前的话")
	}
	recent, _ := journal.Recent(day, 0)
	if strings.Contains(recent, "清空前说的话") || !strings.Contains(recent, "清空后说的话") {
		t.Errorf("最近对话 = %q，应只有清空之后的", recent)
	}
	if !strings.Contains(recent, "a.png /x/a.png") {
		t.Errorf("最近对话 = %q，应带上附件路径", recent)
	}
	again := New()
	if len(again.events) != 1 || again.events[0]["text"] != "清空后说的话" {
		t.Errorf("重启后读回 %v，应只有清空之后的一条", again.events)
	}
}

func TestClearRefusedWhileBusy(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	s.active = []*turn{{cancel: func() {}}}
	if err := s.Clear(); err == nil {
		t.Error("助理在处理消息时不该清空")
	}
	s.active = nil
	s.waits["w"] = &waiter{ch: make(chan Answer, 1)}
	if err := s.Clear(); err == nil {
		t.Error("有卡片等用户回答时不该清空")
	}
}

// 收件箱里的卡片不挡清空；清空后重新发一遍，界面上还在，回答照样交回去。
func TestInboxCardSurvivesClear(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	got := make(chan Answer, 1)
	go func() {
		a, _ := s.Wait(context.Background(), Event{"type": "ask", "text": "要提议吗", "inbox": true})
		got <- a
	}()
	var id string
	for id == "" {
		s.mu.Lock()
		for k := range s.waits {
			id = k
		}
		s.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("收件箱的卡片不该挡清空：%v", err)
	}
	if n := len(s.events); n != 2 || s.events[1]["id"] != id || s.events[1]["seq"] != 2 {
		t.Fatalf("清空后应只剩 clear 和重新发的卡片：%v", s.events)
	}
	if err := s.Answer(id, Answer{Choice: "go"}); err != nil {
		t.Fatal(err)
	}
	if a := <-got; a.Choice != "go" {
		t.Fatalf("回答没交回去：%+v", a)
	}
}

func TestTalking(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	if s.Talking() {
		t.Fatal("没发过消息不算正在说话")
	}
	s.InDirect = func() bool { return true }
	if !s.Talking() {
		t.Fatal("在和 agent 对话算正在说话")
	}
	s.InDirect = nil
	s.lastUser = time.Now().Add(-time.Minute)
	if !s.Talking() {
		t.Fatal("1 分钟前发过消息算正在说话")
	}
	s.lastUser = time.Now().Add(-talkWindow - time.Second)
	if s.Talking() {
		t.Fatal("超过 talkWindow 不算")
	}
}

// 撤回：那条消息和它之后的从页面、最近对话、重启后读回的对话里都去掉，之前的留着，当天记录不删。
func TestRewind(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	s.Emit(Event{"type": "user", "text": "第一条", "id": "m1"})
	s.Emit(Event{"type": "reply", "text": "回第一条"})
	s.Emit(Event{"type": "user", "text": "打错的第二条", "id": "m2"})
	s.Emit(Event{"type": "reply", "text": "回第二条"})
	before := s.epoch
	if _, err := s.Rewind("m2"); err != nil {
		t.Fatal(err)
	}
	if s.epoch == before {
		t.Error("撤回后 epoch 没变，前端不会重画")
	}
	if len(s.events) != 3 || s.events[1]["text"] != "回第一条" || s.events[2]["type"] != "rewind" {
		t.Errorf("撤回后的事件 = %v", s.events)
	}
	for i, e := range s.events {
		if e["seq"] != i+1 {
			t.Errorf("第 %d 条的 seq = %v，前端按 seq 续取", i, e["seq"])
		}
	}
	day := journal.Today()
	recent, _ := journal.Recent(day, 0)
	if strings.Contains(recent, "第二条") || !strings.Contains(recent, "回第一条") {
		t.Errorf("最近对话 = %q", recent)
	}
	if all, _ := journal.Transcript(day, 0); !strings.Contains(all, "打错的第二条") {
		t.Error("当天记录里应该还有撤回的消息")
	}
	if again := New(); len(again.events) != 2 {
		t.Errorf("重启后读回 %v，应只有撤回之前的两条", again.events)
	}
	if _, err := s.Rewind("m9"); err == nil {
		t.Error("不在页面上的消息不该能撤回")
	}
}

// 点停止后马上空闲，能接着发消息；被停掉的那条收尾时不再发 idle，也不显示回复。
func TestStopIsImmediate(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	release := make(chan struct{})
	done := make(chan struct{})
	s.Turn = func(ctx context.Context, text string, _ []File) {
		if text != "慢的" {
			return
		}
		<-ctx.Done()
		<-release // 模拟 harness 进程迟迟不退出
		s.Say(ctx, "停止后才到的回复", nil)
		close(done)
	}
	if _, err := s.Send("慢的", nil); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	s.mu.Lock()
	busy := len(s.active) > 0
	s.mu.Unlock()
	if busy {
		t.Fatal("点停止后会话仍是忙")
	}
	if q, err := s.Send("快的", nil); err != nil || q {
		t.Fatalf("停止后发不了新消息：排队 %v，%v", q, err)
	}
	close(release)
	<-done
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e["type"] == "reply" {
			t.Errorf("被停掉的那条不该再显示回复：%v", e)
		}
	}
}

// 排队：助理在忙时新消息排队，处理完一条接下一条；补充交给正在处理的那条；并行马上开始；去掉的不处理。
func TestQueue(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	var mu sync.Mutex
	var started []string
	gate := map[string]chan struct{}{"一": make(chan struct{}), "并行": make(chan struct{})}
	got := make(chan string, 4) // 「一」收到的补充
	s.Turn = func(ctx context.Context, text string, _ []File) {
		mu.Lock()
		started = append(started, text)
		mu.Unlock()
		if text == "一" {
			got <- <-Supplements(ctx)
		}
		if g := gate[text]; g != nil {
			<-g
		}
	}
	seen := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, started...) }
	wait := func(n int) []string {
		for i := 0; i < 200 && len(seen()) < n; i++ {
			time.Sleep(5 * time.Millisecond)
		}
		return seen()
	}
	if q, _ := s.Send("一", nil); q {
		t.Fatal("空闲时不该排队")
	}
	for _, x := range []string{"二", "补充", "并行", "不要了"} {
		if q, _ := s.Send(x, nil); !q {
			t.Fatalf("「%s」应该排队", x)
		}
	}
	id := func(text string) string {
		for _, q := range s.Queue() {
			if q.Text == text {
				return q.ID
			}
		}
		t.Fatalf("排队里没有「%s」", text)
		return ""
	}
	if err := s.Dequeue(id("补充"), "merge"); err != nil {
		t.Fatal(err)
	}
	if x := <-got; x != "补充" {
		t.Errorf("「一」收到的补充 = %q", x)
	}
	if err := s.Dequeue(id("并行"), "parallel"); err != nil {
		t.Fatal(err)
	}
	if err := s.Dequeue(id("不要了"), "drop"); err != nil {
		t.Fatal(err)
	}
	if got := wait(2); strings.Join(got, ",") != "一,并行" {
		t.Fatalf("开始的顺序 = %v，「二」要等前面的都做完", got)
	}
	close(gate["一"])
	time.Sleep(20 * time.Millisecond)
	if got := seen(); len(got) != 2 {
		t.Fatalf("「并行」还没做完，「二」不该开始：%v", got)
	}
	close(gate["并行"])
	if got := wait(3); strings.Join(got, ",") != "一,并行,二" || len(s.Queue()) != 0 {
		t.Errorf("开始的顺序 = %v，排队还剩 %v", got, s.Queue())
	}
	var merged bool
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e["type"] == "user" && e["text"] == "补充" && e["merged"] == true {
			merged = true
		}
		if e["type"] == "user" && e["text"] == "不要了" {
			t.Error("去掉的消息不该显示")
		}
	}
	if !merged {
		t.Error("补充的消息应该显示在对话里，并标成补充")
	}
}

// 补充到的时候正在等卡片：卡片按用户写了补充收尾；处理完才到的补充当成新消息。
func TestSupplementAnswersCard(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	ans := make(chan Answer, 1)
	var mu sync.Mutex
	var texts []string
	s.Turn = func(ctx context.Context, text string, _ []File) {
		mu.Lock()
		texts = append(texts, text)
		mu.Unlock()
		if text == "删掉它" {
			a, _ := s.Wait(ctx, Event{"type": "ask", "text": "确认吗？"})
			ans <- a
		}
	}
	s.Send("删掉它", nil)
	for len(s.Queue()) == 0 {
		s.Send("是另一个", nil)
	}
	for i := 0; i < 200; i++ {
		s.mu.Lock()
		n := len(s.waits)
		s.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.Dequeue(s.Queue()[0].ID, "merge")
	if a := <-ans; a.Text != "是另一个" {
		t.Errorf("卡片收到的回答 = %+v", a)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e["type"] == "permission_done" && e["text"] != "是另一个" {
			t.Errorf("卡片收尾 = %v", e)
		}
	}
}

// 重启后没收尾的过程补一条 stopped 的结束，已收尾的不补。
func TestOpenActivityEnded(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	s.Emit(Event{"type": "activity", "group": "a1", "kind": "start"})
	s.Emit(Event{"type": "activity", "group": "a1", "kind": "end", "state": "done"})
	s.Emit(Event{"type": "activity", "group": "a2", "kind": "start"})
	s.Emit(Event{"type": "activity", "group": "a2", "kind": "tool", "tool": "list_projects"})
	again := New()
	var ends []string
	for _, e := range again.events {
		if e["kind"] == "end" {
			ends = append(ends, str(e["group"])+":"+str(e["state"]))
		}
	}
	if strings.Join(ends, ",") != "a1:done,a2:stopped" {
		t.Errorf("结束事件 = %v", ends)
	}
}

// Begin：morula 自己开始的处理（验收打回后重新安排）算忙，新消息排队；点停止能停掉它。
func TestBegin(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	s.Turn = func(context.Context, string, []File) {}
	stopped := make(chan struct{})
	s.Begin("", func(ctx context.Context) {
		<-ctx.Done()
		close(stopped)
	})
	if q, err := s.Send("新消息", nil); err != nil || !q {
		t.Fatalf("重新安排时新消息应排队：排队 %v，%v", q, err)
	}
	s.Stop()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("点停止没停掉 morula 开始的处理")
	}
}

// 几条消息并行处理时，每条消息的过程、卡片和回复都带上它自己的 turn；补充带被补充那条的 turn；
// Begin 指定的 turn 也跟着走。界面按 turn 把它们挂在对应的消息下面。
func TestTurnTags(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	release := make(chan struct{})
	s.Turn = func(ctx context.Context, text string, _ []File) {
		s.EmitIn(ctx, Event{"type": "activity", "group": text, "kind": "start"})
		<-release
		s.Say(ctx, "回复"+text, nil)
	}
	s.Send("一", nil)
	s.Send("二", nil)
	for _, q := range s.Queue() {
		s.Dequeue(q.ID, "parallel")
	}
	s.Send("补", nil)
	for _, q := range s.Queue() {
		s.Dequeue(q.ID, "merge")
	}
	ids := map[string]string{} // 用户消息原文 -> id
	s.mu.Lock()
	for _, e := range s.events {
		if e["type"] == "user" && e["merged"] == nil {
			ids[str(e["text"])] = str(e["id"])
		}
	}
	s.mu.Unlock()
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.active)
		s.mu.Unlock()
		if n == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := map[string]string{} // 事件说明 -> turn
	s.mu.Lock()
	for _, e := range s.events {
		switch e["type"] {
		case "activity":
			got["过程"+str(e["group"])] = str(e["turn"])
		case "reply":
			got[str(e["text"])] = str(e["turn"])
		case "user":
			if e["merged"] == true {
				got["补充"] = str(e["turn"])
			}
		}
	}
	s.mu.Unlock()
	want := map[string]string{"过程一": ids["一"], "回复一": ids["一"], "过程二": ids["二"], "回复二": ids["二"], "补充": ids["一"]}
	for k, v := range want {
		if v == "" || got[k] != v {
			t.Errorf("%s 的 turn = %q，应为 %q（全部：%v）", k, got[k], v, got)
		}
	}

	done := make(chan string, 1)
	s.Begin("m-起因", func(ctx context.Context) { done <- TurnOf(ctx) })
	if g := <-done; g != "m-起因" {
		t.Errorf("Begin 的 ctx 里 turn = %q", g)
	}
}

// 输入框的停止只停最后开始的那一条，别的照跑；按编号停只停那一条。
func TestStopTurn(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	stopped := make(chan string, 2)
	for _, id := range []string{"a", "b"} {
		s.Begin(id, func(ctx context.Context) {
			<-ctx.Done()
			stopped <- TurnOf(ctx)
		})
	}
	if n := s.StopTurn(""); n != 1 {
		t.Fatalf("停最后一条后还剩 %d 条，应剩 1 条", n)
	}
	if got := <-stopped; got != "b" {
		t.Fatalf("停掉的是 %q，应是最后开始的 b", got)
	}
	s.mu.Lock()
	last := s.events[len(s.events)-1]
	left := s.active[0].id
	s.mu.Unlock()
	if left != "a" || last["type"] != "note" || last["turn"] != "b" {
		t.Fatalf("还在跑的是 %q，最后一条事件是 %v", left, last)
	}
	if n := s.StopTurn("a"); n != 0 {
		t.Fatalf("按编号停 a 后还剩 %d 条", n)
	}
	if got := <-stopped; got != "a" {
		t.Fatalf("停掉的是 %q，应是 a", got)
	}
}

// 在项目页里发的消息：用户消息带上项目，处理时 ctx 里能取到；排队后再处理也带着。在助理页发的不带。
func TestSendTo(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := New()
	got := make(chan int64, 3)
	hold := make(chan struct{})
	s.Turn = func(ctx context.Context, text string, _ []File) {
		got <- ProjectOf(ctx)
		if text == "一" {
			<-hold
		}
	}
	s.SendTo("一", nil, 7)
	if p := <-got; p != 7 {
		t.Errorf("第一条的项目 = %d，应为 7", p)
	}
	if q, _ := s.SendTo("二", nil, 9); !q || s.Queue()[0].Project != 9 {
		t.Fatal("忙时应排队并记下项目")
	}
	close(hold)
	if p := <-got; p != 9 {
		t.Errorf("排队的那条的项目 = %d，应为 9", p)
	}
	s.Send("三", nil)
	if p := <-got; p != 0 {
		t.Errorf("助理页发的项目 = %d，应为 0", p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ps []any
	for _, e := range s.events {
		if e["type"] == "user" {
			ps = append(ps, e["project"])
		}
	}
	if len(ps) != 3 || ps[0] != int64(7) || ps[1] != int64(9) || ps[2] != nil {
		t.Errorf("用户消息上的项目 = %v", ps)
	}
}
