// Package journal 按天保存助理的完整记录、每日总结和长期记忆。
//
// 对用户：助理只有一条不断的对话。
// 对内：每条消息都由助理按项目处理，这里只存对话本身（路由、安排、汇报、审批）；
// 子 agent 的轨迹留在各自的 harness 里。凌晨 4 点换日，换日后助理盘点前一天，早上把盘点发给用户。
//
// 文件布局（在 store.Dir() 下）：
//
//	days/2026-10-12.jsonl       当天全部事件，一行一条
//	days/2026-10-12.summary.md  助理写的当天总结（盘点）
//	days/2026-10-12.brief       盘点写好了、还没发给用户
//	memory.md                   助理自己维护的长期记忆
package journal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"morula/internal/store"
)

// DayStart 是每天从几点算起，之前的时间算前一天：凌晨 2 点还在干活，仍记在前一天。
// 默认凌晨 4 点；环境变量 MORULA_DAY_START（例如 21:05）只给测试换日用。
var DayStart = clockOf(os.Getenv("MORULA_DAY_START"), 4*time.Hour)

// clockOf 把「时:分」转成从零点起的时长，格式不对时用 def。
func clockOf(s string, def time.Duration) time.Duration {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

func DayOf(t time.Time) string {
	return t.Add(-DayStart).Format("2006-01-02")
}

// BriefTime 是 day 这天早上发前一天盘点的时刻。at 是「时:分」，格式不对时用 08:30；早于换日时刻时算到换日后的那个钟点。
func BriefTime(day, at string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", day, time.Local)
	if err != nil {
		return time.Time{}
	}
	start, t := d.Add(DayStart), d.Add(clockOf(at, 8*time.Hour+30*time.Minute))
	if t.Before(start) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func Today() string { return DayOf(time.Now()) }

func base() string {
	d, err := store.Dir()
	if err != nil {
		home, _ := os.UserHomeDir()
		d = filepath.Join(home, "Library/Application Support/morula")
	}
	return d
}

func daysDir() string { return filepath.Join(base(), "days") }

// DaysDir 是按天记录对话的目录。
func DaysDir() string { return daysDir() }

func eventsPath(day string) string  { return filepath.Join(daysDir(), day+".jsonl") }
func summaryPath(day string) string { return filepath.Join(daysDir(), day+".summary.md") }
func MemoryPath() string            { return filepath.Join(base(), "memory.md") }

var wmu sync.Mutex

// Append 把一条事件追加到当天文件。
func Append(day string, ev map[string]any) error {
	wmu.Lock()
	defer wmu.Unlock()
	if err := os.MkdirAll(daysDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(eventsPath(day), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Load 读出某天的全部事件；没有记录时返回空列表。
func Load(day string) ([]map[string]any, error) {
	list := []map[string]any{}
	f, err := os.Open(eventsPath(day))
	if os.IsNotExist(err) {
		return list, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			list = append(list, m)
		}
	}
	return list, sc.Err()
}

// Days 返回有记录的日期，从早到晚。
func Days() []string {
	files, _ := filepath.Glob(filepath.Join(daysDir(), "*.jsonl"))
	days := make([]string, 0, len(files))
	for _, f := range files {
		days = append(days, strings.TrimSuffix(filepath.Base(f), ".jsonl"))
	}
	sort.Strings(days)
	return days
}

func Summary(day string) (string, bool) {
	b, err := os.ReadFile(summaryPath(day))
	if err != nil {
		return "", false
	}
	return string(b), true
}

func SaveSummary(day, text string) error {
	if err := os.MkdirAll(daysDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(summaryPath(day), []byte(text), 0o644)
}

func Memory() string {
	b, _ := os.ReadFile(MemoryPath())
	return string(b)
}

func SaveMemory(text string) error {
	if err := os.MkdirAll(base(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(MemoryPath(), []byte(text), 0o644)
}

func briefPath(day string) string { return filepath.Join(daysDir(), day+".brief") }

// MarkBrief 记下 day 的盘点写好了、还没发给用户。
func MarkBrief(day string) error { return os.WriteFile(briefPath(day), nil, 0o644) }

// Briefs 是写好了还没发的盘点，按日期从旧到新。
func Briefs() []string {
	files, _ := filepath.Glob(filepath.Join(daysDir(), "*.brief"))
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, strings.TrimSuffix(filepath.Base(f), ".brief"))
	}
	sort.Strings(out)
	return out
}

// BriefSent 去掉 day 的待发标记。
func BriefSent(day string) { os.Remove(briefPath(day)) }

func str(v any) string { s, _ := v.(string); return s }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Line 把一条事件写成一行人能读的文字；不需要进记录的事件返回空串。
func Line(e map[string]any) string {
	t := ""
	if ts := str(e["ts"]); len(ts) >= 16 {
		t = ts[11:16] + " "
	}
	switch str(e["type"]) {
	case "user":
		return t + "用户：" + str(e["text"]) + fileNote(e["files"])
	case "reply":
		if a := str(e["agent"]); a != "" { // 用户直接和某个 agent 对话时，回复的是它
			return t + a + "：" + str(e["text"])
		}
		return t + "助理：" + str(e["text"])
	case "direct":
		return t + "[对话] " + str(e["text"])
	case "tool":
		in, _ := json.Marshal(e["input"])
		return t + "[调用工具] " + str(e["name"]) + " " + clip(string(in), 300)
	case "tool_result":
		if e["error"] == true {
			return t + "[工具失败] " + clip(str(e["text"]), 500)
		}
		return t + "[工具结果] " + clip(str(e["text"]), 500)
	case "route":
		return t + "[归入项目] " + str(e["name"])
	case "reorg":
		switch str(e["op"]) {
		case "rename":
			return t + "[项目改名] 「" + str(e["old"]) + "」改为「" + str(e["name"]) + "」"
		case "merge":
			return t + "[合并项目] 「" + str(e["srcName"]) + "」并进「" + str(e["name"]) + "」，合并编号 " + fmt.Sprint(e["merge"])
		case "unmerge":
			return t + "[撤销合并] 「" + str(e["srcName"]) + "」从「" + str(e["name"]) + "」分了出来"
		case "move":
			return t + "[挪安排] " + fmt.Sprint(e["runs"]) + " 次安排挪到「" + str(e["name"]) + "」"
		case "remove":
			return t + "[删除项目] 「" + str(e["name"]) + "」"
		case "routine":
			act := map[string]string{"add": "新建", "update": "修改", "remove": "删除"}[str(e["act"])]
			return t + "[" + act + "例行事项] 「" + str(e["routineName"]) + "」" + str(e["when"])
		}
	case "ask":
		return t + "[助理提问] " + str(e["text"])
	case "task":
		return t + "[任务] " + str(e["agent"]) + "「" + str(e["title"]) + "」" + str(e["text"])
	case "permission":
		return t + "[请求批准] " + str(e["display"]) + " " + str(e["description"])
	case "permission_done":
		if c := str(e["choice"]); c != "" {
			return t + "[用户选择] " + c
		}
		if e["allow"] == true {
			return t + "[用户批准]"
		}
		return t + "[用户拒绝]"
	case "clear":
		return t + "[用户清空了对话页]"
	case "rewind":
		return t + "[用户撤回了 " + str(e["at"]) + " 那条消息和它之后的对话]"
	case "error", "exit":
		return t + "[出错] " + clip(str(e["text"]), 300)
	}
	return ""
}

// fileNote 把用户消息带的附件写成「（附件：名字 路径；…）」，后面的对话还能按路径找到文件。
func fileNote(v any) string {
	list, _ := v.([]any)
	var parts []string
	for _, x := range list {
		if f, ok := x.(map[string]any); ok {
			parts = append(parts, str(f["name"])+" "+str(f["path"]))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "（附件：" + strings.Join(parts, "；") + "）"
}

// Visible 返回对话页上还看得见的事件：最后一条 clear 之前的不要；rewind 撤回的那条用户消息
// 和它之后、这条 rewind 之前的不要。clear 和 rewind 本身也不返回。
func Visible(evs []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, e := range evs {
		switch str(e["type"]) {
		case "clear":
			out = out[:0]
		case "rewind":
			for i := len(out) - 1; i >= 0; i-- {
				if str(out[i]["type"]) == "user" && str(out[i]["id"]) == str(e["to"]) {
					out = out[:i]
					break
				}
			}
		default:
			out = append(out, e)
		}
	}
	return out
}

// LastClear 返回最近一次清空对话页是在哪一天；没清空过时返回空串。
func LastClear() string {
	days := Days()
	for i := len(days) - 1; i >= 0; i-- {
		evs, _ := Load(days[i])
		for _, e := range evs {
			if str(e["type"]) == "clear" {
				return days[i]
			}
		}
	}
	return ""
}

// Transcript 把某天的记录整理成纯文本；超过 max 个字时只保留最后 max 个字。
func Transcript(day string, max int) (string, error) {
	evs, err := Load(day)
	if err != nil {
		return "", err
	}
	return transcript(evs, max), nil
}

// Recent 和 Transcript 一样，但只取对话页上还看得见的部分（见 Visible）。
func Recent(day string, max int) (string, error) {
	evs, err := Load(day)
	if err != nil {
		return "", err
	}
	return transcript(Visible(evs), max), nil
}

func transcript(evs []map[string]any, max int) string {
	var b strings.Builder
	for _, e := range evs {
		if l := Line(e); l != "" {
			b.WriteString(l)
			b.WriteString("\n")
		}
	}
	r := []rune(b.String())
	if max > 0 && len(r) > max {
		return "…（前面已省略）\n" + string(r[len(r)-max:])
	}
	return string(r)
}

type Hit struct {
	Day  string `json:"day"`
	Line string `json:"line"`
}

// Search 在全部记录的用户原话和助理回复里找包含 query 的行（不分大小写），从新到旧，最多 limit 条。
func Search(query string, limit int) []Hit {
	q := strings.ToLower(strings.TrimSpace(query))
	hits := []Hit{}
	if q == "" {
		return hits
	}
	days := Days()
	for i := len(days) - 1; i >= 0 && len(hits) < limit; i-- {
		evs, _ := Load(days[i])
		for j := len(evs) - 1; j >= 0 && len(hits) < limit; j-- {
			// 只搜对话本身（用户原话和助理回复），不搜工具调用，免得搜到搜索自己。
			if t := str(evs[j]["type"]); t != "user" && t != "reply" {
				continue
			}
			if l := Line(evs[j]); l != "" && strings.Contains(strings.ToLower(l), q) {
				hits = append(hits, Hit{Day: days[i], Line: clip(l, 400)})
			}
		}
	}
	return hits
}

// Context 是新会话开场时给助理的背景：长期记忆 + 最近 n 天的总结（不含 today）。
func Context(today string, n int) string {
	var b strings.Builder
	mem := strings.TrimSpace(Memory())
	if mem == "" {
		mem = "（还是空的）"
	}
	fmt.Fprintf(&b, "## 长期记忆（%s）\n\n%s\n", MemoryPath(), mem)
	days := Days()
	var recent []string
	for i := len(days) - 1; i >= 0 && len(recent) < n; i-- {
		if days[i] >= today {
			continue
		}
		if s, ok := Summary(days[i]); ok {
			recent = append(recent, fmt.Sprintf("### %s\n\n%s", days[i], strings.TrimSpace(s)))
		}
	}
	if len(recent) > 0 {
		b.WriteString("\n## 最近几天的总结（新的在前）\n\n")
		b.WriteString(strings.Join(recent, "\n\n"))
		b.WriteString("\n")
	}
	return b.String()
}
