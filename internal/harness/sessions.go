package harness

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Local 是本机 harness 留下的一个会话，首次整理时用来归项目。只读，不改 harness 的文件。
type Local struct {
	Harness string    `json:"harness"`
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	First   string    `json:"first"` // 用户的第一条消息，截断
	Dir     string    `json:"dir"`
	Updated time.Time `json:"updated"`
}

// SessionSource 是整理页能读会话的一个 harness：Dir 是读的位置，Found 报告本机有没有这份数据。
type SessionSource struct {
	Name  string `json:"name"`
	Dir   string `json:"dir"`
	Found bool   `json:"found"`
}

// SessionSources 列出 LocalSessions 读的几个位置。
func SessionSources() []SessionSource {
	home, _ := os.UserHomeDir()
	out := []SessionSource{{Name: "codex", Dir: filepath.Join(codexHome(), "sessions")}, {Name: "opencode", Dir: opencodeDB()},
		{Name: "pi", Dir: piSessions()}, {Name: "workbuddy", Dir: filepath.Join(codebuddyHome(), "projects")},
		{Name: "workbuddy", Dir: filepath.Join(home, ".workbuddy", "projects")}}
	for i := range out {
		_, err := os.Stat(out[i].Dir)
		out[i].Found = err == nil
	}
	return out
}

// LocalSessions 列出 codex、opencode、pi、WorkBuddy 在本机留下的会话，按最近更新排序。
// 同一个会话只列一次。跳过子 agent 的会话和 morula 自己派出去的会话（提示词以「你是 morula 」开头）。
// 也跳过临时目录里的会话（一次性的测试和试运行）。一个 harness 读失败时跳过它，读到的照常返回。
func LocalSessions() []Local {
	var out []Local
	for _, s := range slices.Concat(codexSessions(), opencodeSessions(), piLocal(), workbuddyLocal()) {
		if !temporary(s.Dir) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	// codex 续跑、分叉时会把同一个会话写进多个 rollout 文件，只留最近更新的那个
	seen := map[string]bool{}
	uniq := out[:0]
	for _, s := range out {
		if k := s.Harness + "/" + s.ID; !seen[k] {
			seen[k] = true
			uniq = append(uniq, s)
		}
	}
	return uniq
}

func temporary(dir string) bool {
	for _, t := range []string{os.TempDir(), "/tmp", "/private/tmp", "/private/var/folders", "/var/folders"} {
		t = strings.TrimSuffix(t, "/")
		if dir == t || strings.HasPrefix(dir, t+"/") {
			return true
		}
	}
	return false
}

// opencode 存的第一条消息可能带引号，先去掉。
func morulaPrompt(s string) bool {
	s = strings.TrimLeft(s, "\"'“「 ")
	return strings.HasPrefix(s, "你是 morula ")
}

// codexSessions 读 sessions 和 archived_sessions 下的 rollout 文件：第一行是 session_meta，
// 再往下找第一条用户消息。标题取 session_index.jsonl 里的 thread_name。
func codexSessions() []Local {
	names := map[string]string{}
	if f, err := os.Open(filepath.Join(codexHome(), "session_index.jsonl")); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var l struct {
				ID   string `json:"id"`
				Name string `json:"thread_name"`
			}
			if json.Unmarshal(sc.Bytes(), &l) == nil && l.Name != "" {
				names[l.ID] = l.Name
			}
		}
		f.Close()
	}
	var out []Local
	for _, sub := range []string{"sessions", "archived_sessions"} {
		filepath.WalkDir(filepath.Join(codexHome(), sub), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			if s, ok := readRollout(p); ok {
				if n := names[s.ID]; n != "" {
					s.Title = n
				}
				if info, err := d.Info(); err == nil {
					s.Updated = info.ModTime()
				}
				out = append(out, s)
			}
			return nil
		})
	}
	return out
}

func readRollout(p string) (Local, bool) {
	f, err := os.Open(p)
	if err != nil {
		return Local{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	s := Local{Harness: "codex"}
	for i := 0; i < 300 && sc.Scan(); i++ {
		var l struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if i == 0 {
			var m struct {
				ID     string          `json:"id"`
				Cwd    string          `json:"cwd"`
				Source json.RawMessage `json:"source"`
			}
			// source 是对象时是子 agent（thread_spawn、guardian 之类）
			if l.Type != "session_meta" || json.Unmarshal(l.Payload, &m) != nil || m.ID == "" || !strings.HasPrefix(string(m.Source), `"`) {
				return Local{}, false
			}
			s.ID, s.Dir = m.ID, m.Cwd
			continue
		}
		if l.Type != "response_item" {
			continue
		}
		var m struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(l.Payload, &m) != nil || m.Type != "message" || m.Role != "user" {
			continue
		}
		var parts []string
		for _, c := range m.Content {
			parts = append(parts, c.Text)
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text == "" || strings.HasPrefix(text, "# AGENTS.md") || strings.HasPrefix(text, "<") {
			continue
		}
		if morulaPrompt(text) {
			return Local{}, false
		}
		s.First = clip(text, 300)
		break
	}
	if s.ID == "" || s.First == "" {
		return Local{}, false
	}
	s.Title = clip(strings.Join(strings.Fields(s.First), " "), 60)
	return s, true
}

// opencodeSessions 只读打开 opencode 的 opencode.db。子 agent 的会话有 parent_id，跳过。
func opencodeSessions() []Local {
	p := opencodeDB()
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT s.id, s.title, s.directory, s.time_updated,
		COALESCE((SELECT json_extract(pt.data, '$.text') FROM message m JOIN part pt ON pt.message_id = m.id
			WHERE m.session_id = s.id AND json_extract(m.data, '$.role') = 'user' AND json_extract(pt.data, '$.type') = 'text'
			ORDER BY m.time_created, pt.id LIMIT 1), '')
		FROM session s WHERE s.parent_id IS NULL`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Local
	for rows.Next() {
		var s Local
		var ms int64
		if rows.Scan(&s.ID, &s.Title, &s.Dir, &ms, &s.First) != nil || morulaPrompt(s.First) {
			continue
		}
		s.Harness, s.Updated, s.First = "opencode", time.UnixMilli(ms), clip(strings.TrimSpace(s.First), 300)
		if s.First == "" && strings.HasPrefix(s.Title, "New session - ") {
			continue // 开了没说话
		}
		out = append(out, s)
	}
	return out
}

// piLocal 读 pi 的会话：第一行是 type 为 session 的头（带 cwd），往下找第一条用户消息。pi 没有标题，标题留空。
func piLocal() []Local {
	files, _ := filepath.Glob(filepath.Join(piSessions(), "*", "*.jsonl"))
	var out []Local
	for _, p := range files {
		s, ok := readPiSession(p)
		if !ok {
			continue
		}
		if info, err := os.Stat(p); err == nil {
			s.Updated = info.ModTime()
		}
		out = append(out, s)
	}
	return out
}

func readPiSession(p string) (Local, bool) {
	f, err := os.Open(p)
	if err != nil {
		return Local{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	s := Local{Harness: "pi"}
	for i := 0; i < 300 && sc.Scan(); i++ {
		var l struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Cwd     string `json:"cwd"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Type == "session" {
			s.ID, s.Dir = l.ID, l.Cwd
			continue
		}
		if l.Type == "message" && l.Message.Role == "user" {
			if t := strings.TrimSpace(contentText(l.Message.Content)); t != "" {
				s.First = clip(t, 300)
				break
			}
		}
	}
	return s, s.ID != "" && s.First != "" && !morulaPrompt(s.First)
}

// workbuddyLocal 读终端版 codebuddy（~/.codebuddy/projects）和桌面版 WorkBuddy（~/.workbuddy/projects）的会话，
// 两处格式一样，都算 WorkBuddy 的。标题取 type 为 ai-title 的行；用户消息里的 <system-reminder> 去掉，斜杠命令跳过。
func workbuddyLocal() []Local {
	home, _ := os.UserHomeDir()
	var out []Local
	for _, d := range []string{filepath.Join(codebuddyHome(), "projects"), filepath.Join(home, ".workbuddy", "projects")} {
		files, _ := filepath.Glob(filepath.Join(d, "*", "*.jsonl"))
		for _, p := range files {
			s, ok := readWorkbuddySession(p)
			if !ok {
				continue
			}
			if info, err := os.Stat(p); err == nil {
				s.Updated = info.ModTime()
			}
			out = append(out, s)
		}
	}
	return out
}

func readWorkbuddySession(p string) (Local, bool) {
	f, err := os.Open(p)
	if err != nil {
		return Local{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	s := Local{Harness: "workbuddy", ID: strings.TrimSuffix(filepath.Base(p), ".jsonl")}
	for i := 0; i < 400 && sc.Scan() && (s.First == "" || s.Title == ""); i++ {
		var l struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Cwd     string          `json:"cwd"`
			Title   string          `json:"aiTitle"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if s.Dir == "" {
			s.Dir = l.Cwd
		}
		switch {
		case l.Type == "ai-title" && s.Title == "":
			s.Title = l.Title
		case l.Type == "message" && l.Role == "user" && s.First == "":
			t := contentText(l.Content)
			if m := queryRe.FindStringSubmatch(t); m != nil { // 桌面版把用户的话包在 <user_query> 里
				t = m[1]
			}
			if t = strings.TrimSpace(reminderRe.ReplaceAllString(t, "")); t != "" && !strings.HasPrefix(t, "<command-") {
				s.First = clip(t, 300)
			}
		}
	}
	return s, s.First != "" && !morulaPrompt(s.First)
}

// contentText 取消息内容里的文字：可能是字符串，也可能是带 text 的内容块数组。
func contentText(raw json.RawMessage) string {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var bs []struct {
		Text string `json:"text"`
	}
	json.Unmarshal(raw, &bs)
	var parts []string
	for _, b := range bs {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
