package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseOpencodeModels(t *testing.T) {
	in := `opencode/big-pickle
{
  "id": "big-pickle",
  "name": "Big Pickle",
  "limit": {"context": 200000},
  "variants": {"high": {}, "low": {}, "max": {}}
}
anthropic/claude-x
{
  "name": "Claude X",
  "variants": {}
}
deepseek/deepseek-chat
{
  "name": "DeepSeek Chat"
}
`
	got := parseOpencodeModels(in)
	if len(got) != 2 {
		t.Fatalf("要两个模型（去掉 Claude），得到 %+v", got)
	}
	if got[0].ID != "opencode/big-pickle" || got[0].Name != "Big Pickle" || !slices.Equal(got[0].Efforts, []string{"low", "high", "max"}) {
		t.Errorf("第一个模型 %+v", got[0])
	}
	if got[1].ID != "deepseek/deepseek-chat" || len(got[1].Efforts) != 0 {
		t.Errorf("第二个模型 %+v", got[1])
	}
}

func TestParsePiModels(t *testing.T) {
	in := `provider       model                 context  max-out  thinking  images
acme           fast-1                128K     16.4K    no        yes
acme           claude/claude-x       1M       128K     yes       yes
relay          codex/gpt-y           400K     128K     yes       yes
`
	got := parsePiModels(in)
	if len(got) != 2 || got[0].ID != "acme/fast-1" || len(got[0].Efforts) != 0 {
		t.Fatalf("模型 %+v", got)
	}
	if got[1].ID != "relay/codex/gpt-y" || got[1].Efforts[0] != "minimal" {
		t.Errorf("带思考的模型 %+v", got[1])
	}
}

func TestPiEvents(t *testing.T) {
	lines := []string{
		`{"type":"session","version":3,"id":"s-1","cwd":"/w"}`,
		`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"读 a.txt"}]}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"read","arguments":{"path":"/w/a.txt"}}],"usage":{"input":100,"output":10,"cacheRead":0},"stopReason":"toolUse"}}`,
		`{"type":"tool_execution_start","toolCallId":"c1","toolName":"read","args":{"path":"/w/a.txt"}}`,
		`{"type":"tool_execution_end","toolCallId":"c1","toolName":"read","result":{"content":[{"type":"text","text":"暗号：甲"}]},"isError":false}`,
		`{"type":"tool_execution_start","toolCallId":"c2","toolName":"bash","args":{"command":"ls"}}`,
		`{"type":"tool_execution_end","toolCallId":"c2","toolName":"bash","result":{"content":[{"type":"text","text":"a.txt"}]},"isError":false}`,
		`{"type":"tool_execution_start","toolCallId":"c3","toolName":"write","args":{"path":"/w/b.txt","content":"x"}}`,
		`{"type":"tool_execution_end","toolCallId":"c3","toolName":"write","result":{"content":[{"type":"text","text":"ok"}]},"isError":false}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"甲"}],"usage":{"input":5,"output":1,"cacheRead":100},"stopReason":"stop"}}`,
	}
	st := piRun{args: map[string]json.RawMessage{}}
	var evs []Event
	for _, l := range lines {
		st.event([]byte(l), func(e Event) { evs = append(evs, e) })
	}
	res, failure := st.res, st.failure
	if failure != "" || res.Session != "s-1" || res.Final != "甲" || res.Tokens != 216 {
		t.Fatalf("结果 %+v 失败 %q", res, failure)
	}
	kinds := []string{}
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Equal(kinds, []string{"tool", "tool_done", "command", "done", "file", "message"}) || evs[2].Text != "ls" || evs[4].Text != "/w/b.txt" {
		t.Errorf("事件 %v", kinds)
	}
	st.event([]byte(`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"API error (400)"}}`), func(Event) {})
	if failure = st.failure; failure != "API error (400)" {
		t.Errorf("出错的回复要记成失败，得到 %q", failure)
	}
}

func TestPiTrajectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dir)
	sub := filepath.Join(dir, "--w--")
	os.MkdirAll(sub, 0o755)
	body := strings.Join([]string{
		`{"type":"session","version":3,"id":"abc","cwd":"/w"}`,
		`{"type":"model_change","provider":"acme","modelId":"m"}`,
		`{"type":"message","timestamp":"2026-10-17T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"问"}]}}`,
		`{"type":"message","timestamp":"2026-10-17T00:00:01Z","message":{"role":"assistant","content":[{"type":"toolCall","name":"read","arguments":{"path":"a"}}]}}`,
		`{"type":"message","timestamp":"2026-10-17T00:00:02Z","message":{"role":"toolResult","content":[{"type":"text","text":"内容"}]}}`,
		`{"type":"message","timestamp":"2026-10-17T00:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"答"}]}}`,
	}, "\n")
	os.WriteFile(filepath.Join(sub, "2026-10-17T00-00-00-000Z_abc.jsonl"), []byte(body), 0o644)
	steps, err := pi{}.Trajectory("abc")
	if err != nil {
		t.Fatal(err)
	}
	roles := []string{}
	for _, s := range steps {
		roles = append(roles, s.Role)
	}
	if !slices.Equal(roles, []string{"user", "tool", "output", "agent"}) || steps[3].Text != "答" {
		t.Errorf("轨迹 %+v", steps)
	}
	if _, err := (pi{}).Trajectory("nope"); err != ErrNoSession {
		t.Errorf("没有的会话要返回 ErrNoSession，得到 %v", err)
	}
}

func TestOpencodeToolEvents(t *testing.T) {
	var kinds []string
	on := func(e Event) { kinds = append(kinds, e.Kind+":"+e.Text) }
	toolEvents("write", []byte(`{"filePath":"/w/b.txt","content":"x"}`), "", false, on)
	toolEvents("bash", []byte(`{"command":"go test"}`), "ok", true, on)
	want := []string{"file:/w/b.txt", "command:go test", "done:ok"}
	if !slices.Equal(kinds, want) {
		t.Errorf("事件 %v，要 %v", kinds, want)
	}
	if got := errText([]byte(`{"name":"APIError","data":{"message":"额度用完"}}`)); got != "额度用完" {
		t.Errorf("errText %q", got)
	}
}
