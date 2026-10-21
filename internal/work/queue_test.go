package work

import (
	"context"
	"testing"
	"time"

	"morula/internal/chat"
)

// 用户把排队的消息补充进来时，正在跑的那一步被停掉，补充的文字交回调用方；没有补充时照常跑完。
func TestInterruptible(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	s := chat.New()
	got := make(chan string, 2)
	s.Turn = func(ctx context.Context, text string, _ []chat.File) {
		more, err := interruptible(ctx, func(c context.Context) error {
			<-c.Done() // 模拟一直没返回的模型调用
			return c.Err()
		})
		if err != nil {
			t.Errorf("被补充打断不该算失败：%v", err)
		}
		got <- more
		if more, err := interruptible(ctx, func(context.Context) error { return nil }); more != "" || err != nil {
			t.Errorf("没有补充时 = %q, %v", more, err)
		}
	}
	s.Send("一", nil)
	s.Send("补充的话", nil)
	s.Dequeue(s.Queue()[0].ID, "merge")
	select {
	case x := <-got:
		if x != "补充的话" {
			t.Errorf("交回的补充 = %q", x)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("补充没有打断正在跑的一步")
	}
}
