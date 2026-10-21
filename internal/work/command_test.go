package work

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"morula/internal/chat"
	"morula/internal/journal"
)

// 批准后才执行；拒绝和用户另写回答时不执行。
func TestAskCommand(t *testing.T) {
	e := staffEngine(t)
	dir := t.TempDir()
	mark := filepath.Join(dir, "done")
	in := commandArgs{Command: "touch done && echo 好了", Dir: dir, Reason: "建一个标记文件"}
	for _, c := range []struct {
		ans  chat.Answer
		ran  bool
		want string
	}{
		{chat.Answer{Allow: false}, false, "用户拒绝了"},
		{chat.Answer{Text: "先别动"}, false, "先别动"},
		{chat.Answer{Allow: true}, true, "退出码 0"},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		go answerAll(ctx, e.Chat, c.ans)
		out, bad := e.askCommand(ctx, in)
		cancel()
		_, err := os.Stat(mark)
		if bad || !strings.Contains(out, c.want) || (err == nil) != c.ran {
			t.Errorf("回答 %+v：执行了=%v，返回 %q", c.ans, err == nil, out)
		}
	}
	if out, bad := e.askCommand(context.Background(), commandArgs{Command: "ls", Reason: "看看", Dir: filepath.Join(dir, "没有")}); !bad || !strings.Contains(out, "不存在") {
		t.Errorf("目录不存在时应拒收：%q", out)
	}
	if _, bad := e.askCommand(context.Background(), commandArgs{Command: "ls"}); !bad {
		t.Error("没写 reason 应拒收")
	}
}

func TestRunShell(t *testing.T) {
	code, out := runShell(context.Background(), t.TempDir(), "echo 出错 >&2; exit 3")
	if code != 3 || out != "出错" {
		t.Errorf("退出码 %d，输出 %q", code, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, _ := runShell(ctx, t.TempDir(), "sleep 5"); code == 0 {
		t.Error("取消后不该返回 0")
	}
}

// 挂给 harness 的工具名单要和服务器上登记的工具一致：登记了却没进名单，codex 看不到这个工具。
func TestAgentToolNames(t *testing.T) {
	e := staffEngine(t)
	ctx := context.Background()
	r := &agentRun{e: e, ctx: ctx}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := r.server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range res.Tools {
		got = append(got, x.Name)
	}
	want := slices.Clone(agentToolNames)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("登记的工具 %v，名单 %v", got, want)
	}
}

// update_memory 传的新内容比旧内容短一半以上时拒收：模型只传了新加的一条，会把旧记忆冲掉。
func TestUpdateMemory(t *testing.T) {
	e := staffEngine(t)
	ctx := context.Background()
	r := &agentRun{e: e, ctx: ctx}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := r.server().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := "用户偏好：用中文沟通，结论放第一句，表达简洁；不编造事实、数字或路径。"
	journal.SaveMemory(old + "\n")
	call := func(args map[string]any) bool {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "update_memory", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.IsError
	}
	if !call(map[string]any{"memory": "汇报先说结论"}) || strings.TrimSpace(journal.Memory()) != old {
		t.Fatalf("短的新内容该拒收，记忆是 %q", journal.Memory())
	}
	if call(map[string]any{"memory": old + "\n汇报先说结论"}) || !strings.Contains(journal.Memory(), "汇报先说结论") {
		t.Fatalf("在旧内容上加一条该保存，记忆是 %q", journal.Memory())
	}
	if call(map[string]any{"memory": "只留这条", "drop": true}) || strings.TrimSpace(journal.Memory()) != "只留这条" {
		t.Fatalf("drop 时该保存，记忆是 %q", journal.Memory())
	}
}

// 高危命令在任何档位都算高危；只读、普通命令不算。
func TestRiskOf(t *testing.T) {
	risky := []string{
		"rm -rf build", "cd x && rm a.txt", "echo $(rm a)", "sudo rm x", "/bin/rm a", "trash ~/a.txt",
		"find . -name '*.log' -delete", "find . -exec rm {} ;", "git push --force origin main", "git push origin +main",
		"git push origin :old", "git -C repo reset --hard HEAD~1", "git clean -fd", "git checkout -- .", "git branch -D x",
		"git rebase -i HEAD~3", "git commit --amend -m x", "git stash drop", "sqlite3 a.db 'DROP TABLE users'",
		"psql -c 'delete from users'", "python3 -c 'import shutil; shutil.rmtree(\"x\")'", "mv a.txt ~/.Trash/",
		"osascript -e 'tell application \"Finder\" to delete file \"a\"'", "rsync -a --delete a/ b/", "xargs rm < list",
		"FOO=1 rm x", "diskutil eraseDisk APFS x disk4",
	}
	for _, c := range risky {
		if riskOf(c) == "" {
			t.Errorf("应算高危：%s", c)
		}
	}
	safe := []string{
		"ls -la", "open -a Calculator", "mkdir -p ~/x", "cp a b", "git push origin main", "git status 2>&1",
		"xattr -dr com.apple.quarantine /Applications/X.app", "pkill -f foo", "git commit -m '加 rm 的说明'",
		"defaults write com.x a 1", "python3 a.py", "echo 'rm 是删除命令'", "grep -rn remove .",
	}
	for _, c := range safe {
		if r := riskOf(c); r != "" {
			t.Errorf("不该算高危：%s（%s）", c, r)
		}
	}
}

func TestReadOnly(t *testing.T) {
	for _, c := range []string{"ls -la ~/Downloads", "cat a | grep b | wc -l", "git status && git log -3", "find . -name x 2>/dev/null",
		"defaults read com.apple.dock", "git -C repo diff", "pgrep -fl codex", "ps aux 2>&1 | head", "git branch -a"} {
		if !readOnly(c) {
			t.Errorf("应算只读：%s", c)
		}
	}
	for _, c := range []string{"ls > a.txt", "echo x >> a", "open -a Calculator", "python3 -c 'print(1)'", "find . -exec ls {} ;",
		"sed -i '' s/a/b/ f", "git commit -m x", "defaults write a b c", "git branch -d x", "cat a &> b", ""} {
		if readOnly(c) {
			t.Errorf("不该算只读：%s", c)
		}
	}
}

// 三个档位：每次都问时都问；按清单问时只读命令不问；全部放行时只有高危命令问。
func TestCommandTier(t *testing.T) {
	e := staffEngine(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o644)
	for _, c := range []struct {
		tier, cmd string
		ask       bool
	}{
		{TierAsk, "ls", true},
		{TierList, "ls", false},
		{TierList, "touch b", true},
		{TierList, "rm a", true},
		{TierAll, "touch b", false},
		{TierAll, "rm a", true},
		{TierAll, "git clean -fd", true},
	} {
		if err := e.SetTier(c.tier); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		asked := make(chan chat.Event, 1)
		epoch, old, _ := e.Chat.Poll(ctx, 0, 0, 0) // 只看这次发的卡片
		go func(epoch int64, n int) {              // 记下卡片，然后拒绝，命令不执行
			for ctx.Err() == nil {
				ep, evs, _ := e.Chat.Poll(ctx, epoch, n, 200*time.Millisecond)
				if ep != epoch {
					epoch, n = ep, 0
				}
				for _, ev := range evs {
					n++
					if ev["type"] == "permission" && ctx.Err() == nil {
						select {
						case asked <- ev:
						default:
						}
						id, _ := ev["id"].(string)
						e.Chat.Answer(id, chat.Answer{Allow: false})
					}
				}
			}
		}(epoch, len(old))
		out, _ := e.askCommand(ctx, commandArgs{Command: c.cmd, Dir: dir, Reason: "试"})
		cancel()
		var ev chat.Event
		select {
		case ev = <-asked:
		default:
		}
		if (ev != nil) != c.ask {
			t.Errorf("%s 档 %q：问了=%v，返回 %q", c.tier, c.cmd, ev != nil, out)
		}
		if ev != nil && riskOf(c.cmd) != "" && ev["display"] != "助理想执行高危命令" {
			t.Errorf("高危命令的卡片标题：%v", ev["display"])
		}
		if !c.ask && !strings.Contains(out, "直接执行了") {
			t.Errorf("%s 档 %q 应直接执行：%q", c.tier, c.cmd, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); err != nil {
		t.Error("拒绝后 rm 不该执行")
	}
	if _, err := os.Stat(filepath.Join(dir, "b")); err != nil {
		t.Error("全部放行时 touch 应执行")
	}
}
