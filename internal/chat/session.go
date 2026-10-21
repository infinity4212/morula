// Package chat 是用户和助理之间那一条不断的对话：事件流、按天记录、等用户回答。
//
// 它不知道助理怎么干活。收到用户消息后交给 Turn（由 internal/work 提供：路由到项目、
// 安排子 agent）；助理要用户拍板时用 Wait 发一张卡片并等回答。事件按天写进 journal，
// 前端用长轮询取。
package chat

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"morula/internal/journal"
)

// Event 是推给前端的一条事件。Seq 从 1 递增；Epoch 在 morula 每次启动时变化。
type Event map[string]any

// File 是用户随消息发来的一个附件，已复制进 morula 数据目录。
type File struct {
	ID    string `json:"id"` // 相对附件目录的路径，前端按它取缩略图
	Name  string `json:"name"`
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	Image bool   `json:"image"`
}

// Answer 是用户对一张卡片的回答：审批用 Allow，选项用 Choice。Text 是用户没选选项、自己写的补充。
type Answer struct {
	Allow  bool   `json:"allow"`
	Choice string `json:"choice"`
	Text   string `json:"text"`
}

type Session struct {
	mu      sync.Mutex
	epoch   int64
	events  []Event
	changed chan struct{}

	active []*turn  // 正在处理的消息（路由、安排），按开始的顺序；用户选了并行时不止一条
	queue  []Queued // 助理在忙时用户新发的消息，等当前的处理完再接着处理
	waits  map[string]*waiter
	seq    int

	lastUser time.Time // 用户最近一次发消息的时间，判断用户是不是正在说话（Talking）

	day    string
	failed map[string]bool // 总结写失败的日子，换日或重启前不再重试

	// Turn 处理一条用户消息和它带的附件。ctx 在用户点停止时取消。
	Turn func(ctx context.Context, text string, files []File)
	// OnEmit 在每条事件发出后调用，用来发系统通知。拿到的是事件的副本，在新的 goroutine 里调用，不持有 s.mu。
	OnEmit func(Event)
	// Summarize 读某天的记录，返回当天总结；同时可以更新长期记忆。nil 表示不写总结。
	Summarize func(ctx context.Context, day, transcript string) (string, error)
	// BriefAt 是早上发前一天盘点的钟点（「时:分」）；nil 或空用 08:30。
	BriefAt func() string
	// InDirect 报告用户是不是正在和某个 agent 对话，Talking 用。nil 表示没有对话。不持有 s.mu 时调用。
	InDirect func() bool
}

// waiter 是一张等用户回答的卡片。inbox 的卡片放在收件箱里，不挡清空和撤回。
type waiter struct {
	ch    chan Answer
	ev    Event
	inbox bool
}

// talkWindow：用户这么久之内发过消息，就算正在说话。
const talkWindow = 3 * time.Minute

// Talking 报告用户是不是正在说话：助理在处理消息、用户在和某个 agent 对话，或 talkWindow 内发过消息。
// 正在说话时，汇报这类不急的事进收件箱，不插进对话。
func (s *Session) Talking() bool {
	s.mu.Lock()
	busy := len(s.active) > 0 || time.Since(s.lastUser) < talkWindow
	s.mu.Unlock()
	return busy || s.InDirect != nil && s.InDirect()
}

// Queued 是排队等着的一条消息。
type Queued struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Files   []File `json:"files,omitempty"`
	Project int64  `json:"project,omitempty"` // 在项目页里发的：这条消息属于这个项目
}

// turn 是正在处理的一条消息。id 是这条用户消息的编号，助理不等用户发消息自己处理的事没有编号。
// supp 收用户选「补充到当前这条」的排队消息，Turn 在能停下的地方读。
type turn struct {
	id      string
	cancel  context.CancelFunc
	supp    chan string
	project int64
}

type suppKey struct{}

type turnKey struct{}

// WithTurn 标明 ctx 里做的事属于哪条用户消息。EmitIn、Say、Wait 发出的事件带上 turn 字段，
// 界面按它把处理过程、卡片和回复挂在那条消息下面，几条消息并行时不按到达顺序交错。
func WithTurn(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, turnKey{}, id)
}

// TurnOf 返回 ctx 里做的事属于哪条用户消息，不属于哪一条时返回空串。
func TurnOf(ctx context.Context) string {
	id, _ := ctx.Value(turnKey{}).(string)
	return id
}

type projectKey struct{}

// WithProject 标明这条消息是在项目页里发的、属于哪个项目；project 为 0 时不标。
func WithProject(ctx context.Context, project int64) context.Context {
	if project == 0 {
		return ctx
	}
	return context.WithValue(ctx, projectKey{}, project)
}

// ProjectOf 是这条消息所在的项目页；在助理页发的返回 0，由助理判断归属。
func ProjectOf(ctx context.Context) int64 {
	p, _ := ctx.Value(projectKey{}).(int64)
	return p
}

// Supplements 返回用户追加给当前这条消息的补充；ctx 不是某条消息的处理时返回 nil。
func Supplements(ctx context.Context) <-chan string { return suppOf(ctx) }

// WithoutSupplements 返回一个不收补充的 ctx。agent 方式的助理在工具里发卡片时用它：
// 补充一律停掉这次运行、续跑会话，不让卡片和运行抢同一条补充。
func WithoutSupplements(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppKey{}, (chan string)(nil))
}

func suppOf(ctx context.Context) chan string {
	c, _ := ctx.Value(suppKey{}).(chan string)
	return c
}

func New() *Session {
	s := &Session{epoch: time.Now().UnixMilli(), changed: make(chan struct{}), waits: map[string]*waiter{}}
	s.day = journal.Today()
	// 把今天已有的记录读回来，重启 morula 后对话接着显示；清空和撤回掉的不读（见 journal.Visible）。
	// 更早的天由前端按需加载。
	evs, _ := journal.Load(s.day)
	for _, e := range journal.Visible(evs) {
		e["seq"] = len(s.events) + 1
		s.events = append(s.events, Event(e))
	}
	// 上次退出时没回答的卡片已经没人等了；没收尾的过程也不会再有下文，补一条结束，界面照常折叠。
	open := map[string]bool{}
	inbox := map[string]bool{} // 放收件箱的那组过程，补的结束也带上 inbox
	var order []string
	for _, e := range s.events {
		switch str(e["type"]) {
		case "ask", "permission":
			e["stale"] = true
		case "activity":
			g := str(e["group"])
			if str(e["kind"]) == "end" {
				delete(open, g)
			} else if !open[g] {
				open[g] = true
				inbox[g] = e["inbox"] == true
				order = append(order, g)
			}
		}
	}
	for _, g := range order {
		if open[g] {
			end := Event{"type": "activity", "group": g, "kind": "end", "state": "stopped", "seq": len(s.events) + 1, "day": s.day}
			if inbox[g] {
				end["inbox"] = true
			}
			s.events = append(s.events, end)
		}
	}
	return s
}

// 这些事件写进当天记录；note、progress 之类只用于当下显示。
var persisted = map[string]bool{
	"user": true, "reply": true, "route": true, "reorg": true, "ask": true, "permission": true,
	"permission_done": true, "error": true, "task": true, "clear": true, "rewind": true,
	"activity": true, // 助理的处理过程：重启后照样显示，journal.Line 不把它写进给助理读的对话
	"direct":   true, // 开始、结束和某个 agent 的对话
}

// emit 追加一条事件并唤醒等待中的轮询。调用方须持有 s.mu。
func (s *Session) emit(e Event) {
	e["ts"] = time.Now().Format(time.RFC3339)
	e["day"] = s.day
	if persisted[str(e["type"])] {
		if err := journal.Append(s.day, e); err != nil {
			e["unsaved"] = err.Error()
		}
	}
	e["seq"] = len(s.events) + 1
	s.events = append(s.events, e)
	if s.OnEmit != nil {
		c := make(Event, len(e))
		for k, v := range e {
			c[k] = v
		}
		go s.OnEmit(c)
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Session) Emit(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit(e)
}

// EmitIn 和 Emit 一样，事件带上 ctx 所属的用户消息（见 WithTurn）。
func (s *Session) EmitIn(ctx context.Context, e Event) {
	stamp(ctx, e)
	s.Emit(e)
}

func stamp(ctx context.Context, e Event) {
	if id := TurnOf(ctx); id != "" {
		e["turn"] = id
	}
}

// Poll 返回 seq > after 的事件；没有新事件时最多等 wait。
// epoch 和前端手里的不同时，前端应清空后从 0 取。
func (s *Session) Poll(ctx context.Context, have int64, after int, wait time.Duration) (int64, []Event, bool) {
	deadline := time.After(wait)
	for {
		s.mu.Lock()
		epoch, busy, ch := s.epoch, len(s.active) > 0, s.changed
		if have != epoch {
			after = 0
		}
		out := []Event{}
		if after < len(s.events) {
			out = append(out, s.events[after:]...)
		}
		s.mu.Unlock()
		if len(out) > 0 || have != epoch {
			return epoch, out, busy
		}
		select {
		case <-ch:
		case <-deadline:
			return epoch, []Event{}, busy
		case <-ctx.Done():
			return epoch, []Event{}, busy
		}
	}
}

func (s *Session) Close() { s.Stop() }

// Send 收下一条用户消息和它带的附件，交给 Turn 在后台处理。助理在忙时先排队，返回 true。
func (s *Session) Send(text string, files []File) (queued bool, err error) {
	return s.SendTo(text, files, 0)
}

// SendTo 和 Send 一样，project 不为 0 时这条消息是在那个项目的页面里发的，不用助理判断归属。
func (s *Session) SendTo(text string, files []File, project int64) (queued bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Turn == nil {
		return false, errors.New("助理还没准备好")
	}
	if len(s.active) > 0 {
		s.seq++
		s.queue = append(s.queue, Queued{ID: fmt.Sprintf("q%d-%d", s.epoch, s.seq), Text: text, Files: files, Project: project})
		s.emit(Event{"type": "queue"})
		return true, nil
	}
	s.start(text, files, "", project)
	return false, nil
}

// Queue 返回排队等着的消息。
func (s *Session) Queue() []Queued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Queued{}, s.queue...)
}

// Dequeue 处理一条排队的消息。how 是 merge（补充到最早开始、还在处理的那条）、
// parallel（现在就单独处理）或 drop（不发了）。助理已空闲时 merge 和 parallel 都是现在就处理。
func (s *Session) Dequeue(id, how string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := -1
	for i, q := range s.queue {
		if q.ID == id {
			at = i
		}
	}
	if at < 0 {
		return errors.New("这条消息已经不在排队里")
	}
	q := s.queue[at]
	s.queue = append(s.queue[:at:at], s.queue[at+1:]...)
	switch {
	case how == "drop":
		s.emit(Event{"type": "queue"})
	case how == "merge" && len(s.active) > 0 && len(q.Files) == 0:
		select {
		case s.active[0].supp <- q.Text:
			e := Event{"type": "user", "text": q.Text, "id": s.msgID(), "merged": true}
			if t := s.active[0].id; t != "" {
				e["turn"] = t // 补充挂在它补充的那条消息下面
			}
			s.emit(e)
		default: // 补充攒得太多还没读，改成单独处理
			s.start(q.Text, q.Files, "", q.Project)
		}
	case how == "merge" || how == "parallel":
		s.start(q.Text, q.Files, "", q.Project)
	default:
		s.queue = append(s.queue[:at:at], append([]Queued{q}, s.queue[at:]...)...)
		return errors.New("不认识的处理方式：" + how)
	}
	return nil
}

// msgID 给一条用户消息编号，撤回和编辑靠它认消息。调用方须持有 s.mu。
func (s *Session) msgID() string {
	s.seq++
	return fmt.Sprintf("m%d-%d", time.Now().UnixNano(), s.seq)
}

// start 开始处理一条消息。turn 不为空表示这条消息已经显示过（补充没赶上，改成单独处理），
// 处理过程照样挂在原来那条消息下面。project 不为 0 时这条消息属于那个项目（SendTo）。调用方须持有 s.mu。
func (s *Session) start(text string, files []File, turn string, project int64) {
	if today := journal.Today(); today != s.day {
		s.day = today
		s.failed = nil
	}
	if turn == "" {
		// id 给撤回和编辑用：前端按它指出是哪一条，也按它把处理过程和回复挂在这条下面。
		turn = s.msgID()
		s.lastUser = time.Now()
		e := Event{"type": "user", "text": text, "id": turn}
		if len(files) > 0 {
			e["files"] = files
		}
		if project != 0 {
			e["project"] = project
		}
		s.emit(e)
	}
	s.runIn(turn, project, func(ctx context.Context) { s.Turn(WithProject(ctx, project), text, files) })
}

// SendRoutine 把到点的例行事项当成一条消息发给助理，对话里显示成「例行事项 name」发来的。
// 助理正忙（有消息在处理）时不发，返回 ok 为 false，调用方过一会儿再试。done 在这条消息处理完时调用，stopped 表示被停止了。
func (s *Session) SendRoutine(name, text string, done func(stopped bool)) (turn string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Turn == nil || len(s.active) > 0 {
		return "", false
	}
	if today := journal.Today(); today != s.day {
		s.day = today
		s.failed = nil
	}
	turn = s.msgID()
	s.emit(Event{"type": "user", "text": text, "id": turn, "routine": name})
	s.run(turn, func(ctx context.Context) {
		s.Turn(ctx, text, nil)
		if done != nil {
			done(ctx.Err() != nil)
		}
	})
	return turn, true
}

// Begin 让助理不等用户发消息就处理一件事（例如验收打回后重新安排）。和用户的消息一样算忙、能停止、
// 能收补充；不在对话里显示成用户的话。turn 是这件事起因的那条用户消息，过程和回复挂在它下面；没有时填空串。
func (s *Session) Begin(turn string, fn func(ctx context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run(turn, fn)
}

// run 把 fn 登记成一条正在处理的消息，在后台跑。调用方须持有 s.mu。
func (s *Session) run(id string, fn func(ctx context.Context)) { s.runIn(id, 0, fn) }

func (s *Session) runIn(id string, project int64, fn func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &turn{id: id, cancel: cancel, supp: make(chan string, 8), project: project}
	ctx = WithTurn(context.WithValue(ctx, suppKey{}, t.supp), id)
	s.active = append(s.active, t)
	go func() {
		defer cancel()
		fn(ctx)
		s.mu.Lock()
		s.done(t)
		s.mu.Unlock()
	}()
}

// done 收尾一条处理完的消息：没赶上的补充当成新消息接着处理；都空闲了就接排队的下一条。调用方须持有 s.mu。
func (s *Session) done(t *turn) {
	at := -1
	for i, x := range s.active {
		if x == t {
			at = i
		}
	}
	if at < 0 { // 用户点过停止，Stop 已经收尾
		return
	}
	s.active = append(s.active[:at:at], s.active[at+1:]...)
	var left []string
	for len(t.supp) > 0 {
		left = append(left, <-t.supp)
	}
	if len(left) > 0 {
		s.start(strings.Join(left, "\n\n"), nil, t.id, t.project)
		return
	}
	if len(s.active) > 0 {
		return
	}
	if len(s.queue) > 0 {
		q := s.queue[0]
		s.queue = s.queue[1:]
		s.start(q.Text, q.Files, "", q.Project)
		return
	}
	s.emit(Event{"type": "idle"})
}

// Clear 清空对话页：在当天记录里记一条 clear，换一个 epoch，前端随之清空重画。
// 当天记录留在磁盘上；助理读「今天最近的对话」、重启后读回今天的对话，都只读这条之后的部分。
// 助理正在处理消息或有卡片等用户回答时不清空，免得卡片被清掉后没人能回答。
func (s *Session) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) > 0 {
		return errors.New("助理还在处理消息，等它处理完或点停止后再清空")
	}
	if s.pending() {
		return errors.New("还有等你回答的卡片，回答后再清空")
	}
	if today := journal.Today(); today != s.day {
		s.day = today
		s.failed = nil
	}
	s.events, s.queue = nil, nil
	s.newEpoch()
	s.emit(Event{"type": "clear"})
	s.keepInbox()
	return nil
}

// Stop 停掉正在处理的消息，并马上把会话标成空闲：用户可以接着发消息，不用等 harness 进程真正退出。
// 排队的消息留着，等用户自己发出或去掉。后台在跑的安排不受影响，要停由调用方另外停。
func (s *Session) Stop() error {
	s.mu.Lock()
	active := s.active
	if len(active) > 0 {
		s.active = nil
		s.emit(Event{"type": "note", "text": "已停止"})
		s.emit(Event{"type": "idle"})
	}
	s.mu.Unlock()
	for _, t := range active {
		t.cancel()
	}
	return nil
}

// StopTurn 只停 id 那条消息的处理，同一条消息下的其他处理（例如验收打回后重新安排）一起停；
// id 为空时停最后开始的那一条。其余在处理的消息照跑，排队的消息留着。返回还在处理的条数。
func (s *Session) StopTurn(id string) int {
	s.mu.Lock()
	var stop, keep []*turn
	for i, t := range s.active {
		if id == "" && i == len(s.active)-1 || id != "" && t.id == id {
			stop = append(stop, t)
		} else {
			keep = append(keep, t)
		}
	}
	if len(stop) > 0 {
		s.active = keep
		e := Event{"type": "note", "text": "已停止"}
		if t := stop[0].id; t != "" {
			e["turn"] = t
		}
		s.emit(e)
		if len(keep) == 0 {
			s.emit(Event{"type": "idle"})
		}
	}
	n := len(s.active)
	s.mu.Unlock()
	for _, t := range stop {
		t.cancel()
	}
	return n
}

// Rewind 撤回对话页上 id 那条用户消息和它之后的全部内容：在当天记录里记一条 rewind，换一个 epoch，
// 前端随之重画。和 Clear 一样，当天记录留在磁盘上，助理读「今天最近的对话」时不再读撤回的部分。
// 返回被撤回那条消息的时间。
func (s *Session) Rewind(id string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.active) > 0 {
		return time.Time{}, errors.New("助理还在处理消息，点停止后再撤回")
	}
	if s.pending() {
		return time.Time{}, errors.New("还有等你回答的卡片，回答后再撤回")
	}
	at := -1
	for i, e := range s.events {
		if str(e["type"]) == "user" && str(e["id"]) == id && id != "" {
			at = i
		}
	}
	if at < 0 {
		return time.Time{}, errors.New("这条消息不在当前对话页里，没法撤回")
	}
	ts, _ := time.Parse(time.RFC3339, str(s.events[at]["ts"]))
	s.events = s.events[:at]
	s.newEpoch()
	s.emit(Event{"type": "rewind", "to": id, "at": ts.Format("15:04")})
	s.keepInbox()
	return ts, nil
}

// pending 报告有没有等用户回答、不在收件箱里的卡片。调用方须持有 s.mu。
func (s *Session) pending() bool {
	for _, w := range s.waits {
		if !w.inbox {
			return true
		}
	}
	return false
}

// keepInbox 在清空、撤回后把收件箱里还没回答的卡片重新发一遍，界面重画后它们还在。调用方须持有 s.mu。
func (s *Session) keepInbox() {
	var ids []string
	for id, w := range s.waits {
		if w.inbox {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		e := Event{}
		for k, v := range s.waits[id].ev {
			if k != "seq" && k != "ts" && k != "day" {
				e[k] = v
			}
		}
		s.emit(e)
	}
}

func (s *Session) newEpoch() {
	if now := time.Now().UnixMilli(); now > s.epoch {
		s.epoch = now
	} else {
		s.epoch++
	}
}

// Wait 发一张要用户回答的卡片（type 为 ask 或 permission），等用户回答或 ctx 结束。
func (s *Session) Wait(ctx context.Context, e Event) (Answer, error) {
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("w%d-%d", s.epoch, s.seq)
	ch := make(chan Answer, 1)
	e["id"] = id
	stamp(ctx, e)
	s.emit(e)
	s.waits[id] = &waiter{ch: ch, ev: e, inbox: e["inbox"] == true}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waits, id)
		s.mu.Unlock()
	}()
	select {
	case a := <-ch:
		return a, nil
	case t := <-Supplements(ctx): // 用户把排队的消息补充进来：当成写在这张卡片上的补充
		s.mu.Lock()
		_, ok := s.waits[id]
		if ok {
			delete(s.waits, id)
			s.emit(Event{"type": "permission_done", "id": id, "text": t})
		}
		s.mu.Unlock()
		if !ok { // 用户刚好点了选项：以选项为准，补充留给下一个能读它的地方
			select {
			case suppOf(ctx) <- t:
			default:
			}
			return <-ch, nil
		}
		return Answer{Text: t}, nil
	case <-ctx.Done():
		s.Emit(Event{"type": "permission_done", "id": id, "expired": true})
		return Answer{}, ctx.Err()
	}
}

// Answer 交回用户对卡片的回答。
func (s *Session) Answer(id string, a Answer) error {
	s.mu.Lock()
	w, ok := s.waits[id]
	delete(s.waits, id)
	if ok {
		s.emit(Event{"type": "permission_done", "id": id, "allow": a.Allow, "choice": a.Choice, "text": a.Text})
	}
	s.mu.Unlock()
	if !ok {
		return errors.New("这张卡片已经处理过或已失效")
	}
	w.ch <- a
	return nil
}

// Say 把助理要给用户看的一段话显示出来。extra 里的字段一并带上（例如所属项目）。
func (s *Session) Say(ctx context.Context, text string, extra Event) {
	text = strings.TrimSpace(text)
	if text == "" || ctx.Err() != nil {
		return
	}
	e := Event{}
	for k, v := range extra {
		e[k] = v
	}
	e["type"], e["text"] = "reply", text
	s.EmitIn(ctx, e)
}

func str(v any) string { x, _ := v.(string); return x }
