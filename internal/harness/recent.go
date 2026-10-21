package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Chat 是在用户主目录里找到的一个对话记录（jsonl），首次使用时让助理看用户最近在做什么。
// 不认 harness：codex、WorkBuddy 等常见的对话格式都按「角色 + 文字」读。只读，不改文件。
type Chat struct {
	Source    string    `json:"source"` // 记录所在的工具，取主目录下第一层目录名（例如 .codex、.workbuddy）
	Path      string    `json:"path"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Dir       string    `json:"dir"`
	Updated   time.Time `json:"updated"`
	Users     int       `json:"users"`            // 用户发了几条消息
	First     string    `json:"first"`            // 用户的第一条消息，截断
	LastUser  string    `json:"lastUser"`         // 用户的最后一条消息，截断
	LastReply string    `json:"lastReply"`        // 最后一条回复，截断
	Morula    bool      `json:"morula,omitempty"` // morula 派出去的会话（子 agent 干活的记录）
	Agent     string    `json:"agent,omitempty"`  // morula 派出去的会话是哪个 agent 跑的，从职责说明的开头读
	Hit       string    `json:"hit,omitempty"`    // FindChats 找到的命中附近的文字
}

// skipDirs 是扫描时不进去的目录名：系统资料、依赖、缓存，不会有用户的对话记录。
var skipDirs = map[string]bool{
	"Library": true, "node_modules": true, ".git": true, ".Trash": true, ".cache": true, ".npm": true,
	".nvm": true, ".cargo": true, ".rustup": true, "go": true, "Applications": true, ".venv": true,
	"venv": true, "site-packages": true, "subagents": true, "tool-results": true, "Pictures": true,
	"Movies": true, "Music": true, "miniforge3": true, ".bun": true, "aiusage": true, "binaries": true,
	"logs": true, "traces": true, "audit-log": true, "modify_backup": true, ".modify_backup_meta": true,
}

// chatRoots 是常见工具存对话记录的目录（相对主目录），先扫这些，剩下的时间再扫主目录的其他地方。
var chatRoots = []string{
	".codex/sessions", ".codex/archived_sessions", ".workbuddy/projects", ".claude/projects",
	".local/share/opencode", ".pi",
}

// scanBudget 是找 jsonl 文件最多花的时间，到时用已经找到的。
const scanBudget = 8 * time.Second

// RecentChats 在用户主目录里找全部 .jsonl 文件，按修改时间从新到旧读，返回最近的 n 个对话记录。
// 跳过 skip 下的文件（morula 自己的数据目录）、morula 派出去的会话、子 agent 的会话、临时目录里的会话，
// 以及读不出用户消息的文件（日志之类）。同一个会话写进多个文件时只留最新的。
func RecentChats(ctx context.Context, n int, skip ...string) []Chat {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []Chat
	seen := map[string]bool{}
	for _, f := range jsonlFiles(ctx, home, skip) {
		if len(out) >= n || ctx.Err() != nil {
			break
		}
		c, ok := readChat(f.path)
		if !ok || temporary(c.Dir) {
			continue
		}
		c.Updated, c.Source = f.mod, sourceOf(home, f.path)
		if k := c.Source + "/" + c.ID; !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

// findFiles 是 FindChats 最多打开看内容的文件数（从新到旧）。
const findFiles = 400

// FindChats 在用户主目录的对话记录里找包含 query 的（不分大小写），从新到旧返回最多 n 个。
// 和 RecentChats 不同，morula 派出去的会话（子 agent 干活的记录）也算，Morula 标为 true；Hit 是第一处命中附近的文字。
// agent 不是空串时只要这个 agent 跑的 morula 会话，query 可以是空串。
func FindChats(ctx context.Context, query, agent string, n int) []Chat {
	q := strings.ToLower(strings.TrimSpace(query))
	agent = strings.TrimSpace(agent)
	home, err := os.UserHomeDir()
	if q == "" && agent == "" || err != nil {
		return nil
	}
	if q == "" {
		q = strings.ToLower("「" + agent + "」")
	}
	var out []Chat
	seen := map[string]bool{}
	for i, f := range jsonlFiles(ctx, home, nil) {
		if len(out) >= n || i >= findFiles || ctx.Err() != nil {
			break
		}
		hit := grepFile(f.path, q)
		if hit == "" {
			continue
		}
		c, ok := scanChat(f.path, true)
		if !ok || agent != "" && c.Agent != agent {
			continue
		}
		c.Updated, c.Source, c.Hit = f.mod, sourceOf(home, f.path), hit
		if k := c.Source + "/" + c.ID; !seen[k] {
			seen[k] = true
			out = append(out, c)
		}
	}
	return out
}

// grepFile 返回文件里第一处包含 q（已转小写）的那一行里命中附近的文字，没有时返回空串。
func grepFile(p, q string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for sc.Scan() {
		t := sc.Text()
		low := strings.ToLower(t)
		i := strings.Index(low, q)
		if i < 0 {
			continue
		}
		if len(low) != len(t) { // 转小写后字节数变了，下标对不上原文，从行首截
			i = 0
		}
		r := []rune(t[:i])
		from := max(0, len(r)-80)
		return clip(oneLine(string(r[from:])+t[i:]), 240)
	}
	return ""
}

type jsonlFile struct {
	path string
	mod  time.Time
}

// jsonlFiles 找主目录下全部非空的 .jsonl 文件，从新到旧。先扫 chatRoots，剩下的时间再扫主目录的其他地方，最多花 scanBudget。
func jsonlFiles(ctx context.Context, home string, skip []string) []jsonlFile {
	var files []jsonlFile
	deadline := time.Now().Add(scanBudget)
	var roots []string
	for _, r := range chatRoots {
		roots = append(roots, filepath.Join(home, r))
	}
	walk := func(root string, skip []string) {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || ctx.Err() != nil || time.Now().After(deadline) {
				if d != nil && d.IsDir() && p != root {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if p != root && skipDirs[d.Name()] || under(p, skip) {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), ".jsonl") && d.Type().IsRegular() {
				if info, err := d.Info(); err == nil && info.Size() > 0 {
					files = append(files, jsonlFile{p, info.ModTime()})
				}
			}
			return nil
		})
	}
	for _, r := range roots {
		walk(r, skip)
	}
	walk(home, append(roots, skip...))
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	return files
}

// sourceOf 是记录所在的工具：主目录下第一层目录名。
func sourceOf(home, p string) string {
	if rel, err := filepath.Rel(home, p); err == nil {
		return strings.Split(rel, string(filepath.Separator))[0]
	}
	return ""
}

func under(p string, dirs []string) bool {
	for _, d := range dirs {
		if d != "" && (p == d || strings.HasPrefix(p, strings.TrimSuffix(d, "/")+"/")) {
			return true
		}
	}
	return false
}

// line 是对话记录里一行可能有的字段。codex 的消息在 payload 里，有的工具在 message 里，有的直接在顶层。
type line struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"sessionId"`
	AITitle   string          `json:"aiTitle"`
	Summary   string          `json:"summary"`
	Sidechain bool            `json:"isSidechain"`
	Meta      bool            `json:"isMeta"`
	Payload   *line           `json:"payload"`
	Message   *line           `json:"message"`
	ID        string          `json:"id"`
	Source    json.RawMessage `json:"source"`
	ToolUse   json.RawMessage `json:"toolUseResult"` // 有这个字段的用户消息是工具结果，不是用户写的
}

var (
	reminderRe = regexp.MustCompile(`(?s)<system-reminder[^>]*>.*?</system-reminder>`)
	queryRe    = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)
	agentRe    = regexp.MustCompile(`^你是 morula ?(?:团队)?里的([^「，。\n]{0,12}?)(?:「([^」]+)」|，|。)`)
)

// readChat 读一个 jsonl 文件。认不出对话格式、没有用户消息、或是 morula 派出去的会话时返回 false。
func readChat(p string) (Chat, bool) { return scanChat(p, false) }

// scanChat 读一个 jsonl 文件；keepMorula 为 true 时 morula 派出去的会话也读，Morula 标为 true。
func scanChat(p string, keepMorula bool) (Chat, bool) {
	f, err := os.Open(p)
	if err != nil {
		return Chat{}, false
	}
	defer f.Close()
	c := Chat{Path: p, ID: strings.TrimSuffix(filepath.Base(p), ".jsonl")}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for i := 0; sc.Scan(); i++ {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			if i == 0 {
				return Chat{}, false // 第一行就不是 JSON 对象：不是对话记录
			}
			continue
		}
		if l.Type == "session_meta" && l.Payload != nil { // codex：source 是对象时是子 agent 的会话
			if !strings.HasPrefix(string(l.Payload.Source), `"`) {
				return Chat{}, false
			}
			c.ID, c.Dir = orStr(l.Payload.ID, c.ID), orStr(l.Payload.Cwd, c.Dir)
			continue
		}
		if l.SessionID != "" {
			c.ID = l.SessionID
		}
		c.Dir = orStr(l.Cwd, c.Dir)
		if t := orStr(l.AITitle, l.Summary); t != "" && l.Role == "" && l.Message == nil {
			c.Title = oneLine(t)
		}
		if l.Sidechain || l.Meta || len(l.ToolUse) > 0 {
			continue
		}
		m := &l
		switch {
		case l.Message != nil:
			m = l.Message
		case l.Payload != nil:
			if l.Payload.Type != "message" {
				continue
			}
			m = l.Payload
		}
		role, text := m.Role, textOf(m.Content)
		if text == "" {
			continue
		}
		switch role {
		case "user":
			if text = userText(text); text == "" {
				continue
			}
			if c.Users == 0 && morulaPrompt(text) {
				if !keepMorula {
					return Chat{}, false
				}
				c.Morula = true
				if m := agentRe.FindStringSubmatch(text); m != nil {
					c.Agent = orStr(m[2], strings.TrimSpace(m[1])) // 「」里的名字；没有时是角色，例如助理
				}
				text = clip(oneLine(text), 120)
			}
			c.Users++
			if c.First == "" {
				c.First = clip(text, 300)
			}
			c.LastUser = clip(text, 300)
		case "assistant":
			c.LastReply = clip(text, 500)
		}
	}
	if c.Users == 0 {
		return Chat{}, false
	}
	if c.Title == "" {
		c.Title = clip(oneLine(c.First), 60)
	}
	return c, true
}

// textOf 取消息内容里的文字：内容是字符串，或是带 text 的片段列表；工具调用、工具结果、思考不算。
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, x := range parts {
		switch x.Type {
		case "", "text", "input_text", "output_text":
			if t := strings.TrimSpace(x.Text); t != "" {
				out = append(out, t)
			}
		}
	}
	return strings.Join(out, "\n")
}

// userText 去掉工具塞进用户消息的系统提示，只留用户自己写的。剩下的是环境说明、规则文件、命令输出时返回空串。
func userText(s string) string {
	if m := queryRe.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	s = strings.TrimSpace(reminderRe.ReplaceAllString(s, ""))
	if s == "" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "# AGENTS.md") || strings.HasPrefix(s, "Caveat:") ||
		strings.HasPrefix(s, "This session is being continued") || strings.HasPrefix(s, "[Request interrupted") ||
		s == "Tool loaded." {
		return ""
	}
	return s
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
