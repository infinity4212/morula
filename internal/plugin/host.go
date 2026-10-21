package plugin

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

//go:embed host.js
var hostJS []byte

// Provider 是插件提供的一个模型来源（一个订阅或一个 API）。
type Provider struct {
	ID       string   `json:"id"`
	Spec     string   `json:"spec"`
	Name     string   `json:"name"`
	NPM      string   `json:"npm"`
	API      string   `json:"api"`
	Methods  []Method `json:"methods"`
	Usage    bool     `json:"usage"`
	Checkin  bool     `json:"checkin"` // 有每日签到（checkin.go）
	SignedIn bool     `json:"signedIn"`
	AuthType string   `json:"authType"`
	Account  string   `json:"account"`
	Models   []Model  `json:"models"`
	Builtin  bool     `json:"builtin"` // API key 来源，不是插件提供的（apis.go）
}

// Method 是一种登录方式：oauth（浏览器或读桌面端）或 api（填 key）。
type Method struct {
	Type    string   `json:"type"`
	Label   string   `json:"label"`
	Prompts []Prompt `json:"prompts"`
}

// Prompt 是登录前要用户填的一项。
type Prompt struct {
	Type        string `json:"type"` // text 或 select
	Key         string `json:"key"`
	Message     string `json:"message"`
	Placeholder string `json:"placeholder,omitempty"`
	Options     []struct {
		Label string `json:"label"`
		Value string `json:"value"`
	} `json:"options,omitempty"`
}

type Model struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Context   int      `json:"context"`
	Reasoning bool     `json:"reasoning"`
	Efforts   []string `json:"efforts"`
	Free      bool     `json:"free,omitempty"` // 插件标了免费
	Rate      float64  `json:"rate,omitempty"` // 插件给的计费倍率，例如 0.29；0 表示没给
}

// Loaded 是一个插件的加载结果。
type Loaded struct {
	Spec  string `json:"spec"`
	Error string `json:"error,omitempty"`
}

// Host 是一个运行中的 host.js 进程。
type Host struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	wmu    sync.Mutex
	mu     sync.Mutex
	next   int
	calls  map[int]chan msg
	fetchs map[int]*fetchReply
	done   chan struct{}
	err    error
	Loaded []Loaded
}

type msg struct {
	ID      int                       `json:"id"`
	Event   string                    `json:"event"`
	Result  json.RawMessage           `json:"result"`
	Error   *struct{ Message string } `json:"error"`
	Status  int                       `json:"status"`
	Headers map[string]string         `json:"headers"`
	Data    string                    `json:"data"`
	Message string                    `json:"message"`
}

type fetchReply struct {
	head chan msg
	pw   *io.PipeWriter
}

var (
	cur   *Host
	curMu sync.Mutex
)

// Get 返回运行中的 host；没有时按 plugins.json 启动一个。
func Get(ctx context.Context) (*Host, error) {
	curMu.Lock()
	defer curMu.Unlock()
	if cur != nil && !cur.exited() {
		return cur, nil
	}
	h, err := start(ctx)
	if err != nil {
		return nil, err
	}
	cur = h
	return h, nil
}

// Restart 在插件增删、启停后调用：停掉旧 host，下次 Get 时按新清单启动。
func Restart() {
	curMu.Lock()
	defer curMu.Unlock()
	if cur != nil {
		cur.Close()
		cur = nil
	}
}

func (h *Host) exited() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// Close 关掉 stdin，host 读到 EOF 后退出；2 秒内没退就杀掉。
func (h *Host) Close() {
	h.in.Close()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.cmd.Process.Kill()
	}
}

func start(ctx context.Context) (*Host, error) {
	bun, err := Bun()
	if err != nil {
		return nil, err
	}
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	script := filepath.Join(dir, ".morula-host.js")
	if b, _ := os.ReadFile(script); string(b) != string(hostJS) {
		if err := os.WriteFile(script, hostJS, 0o644); err != nil {
			return nil, err
		}
	}
	list, err := List()
	if err != nil {
		return nil, err
	}
	plugins := []map[string]string{}
	for _, e := range list {
		if !e.Off {
			plugins = append(plugins, map[string]string{"spec": e.Spec, "target": filepath.Join(dir, "node_modules", e.Name)})
		}
	}
	authPath, err := AuthPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bun, script)
	cmd.Dir = dir
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动插件运行器失败：%w", err)
	}
	h := &Host{cmd: cmd, in: in, calls: map[int]chan msg{}, fetchs: map[int]*fetchReply{}, done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			log.Println("plugin:", sc.Text())
		}
	}()
	go h.read(out)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var r struct {
		Plugins []Loaded `json:"plugins"`
	}
	if err := h.Call(ctx, "init", map[string]any{"authPath": authPath, "directory": dir, "plugins": plugins}, &r); err != nil {
		h.Close()
		return nil, fmt.Errorf("插件运行器初始化失败：%w", err)
	}
	h.Loaded = r.Plugins
	return h, nil
}

func (h *Host) read(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 64<<20)
	for sc.Scan() {
		var m msg
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.ID == 0 {
			if m.Event == "log" && m.Message != "" {
				log.Println("plugin:", m.Message)
			}
			continue
		}
		h.mu.Lock()
		f := h.fetchs[m.ID]
		c := h.calls[m.ID]
		h.mu.Unlock()
		if f != nil {
			h.onFetch(m, f)
			continue
		}
		if c != nil {
			c <- m
		}
	}
	h.err = sc.Err()
	if h.err == nil {
		h.err = errors.New("插件运行器退出了")
	}
	h.cmd.Wait()
	close(h.done)
	h.mu.Lock()
	for id, f := range h.fetchs {
		select {
		case f.head <- msg{Error: &struct{ Message string }{h.err.Error()}}:
		default:
		}
		f.pw.CloseWithError(h.err)
		delete(h.fetchs, id)
	}
	h.mu.Unlock()
}

func (h *Host) send(v any) error {
	b, _ := json.Marshal(v)
	h.wmu.Lock()
	defer h.wmu.Unlock()
	_, err := h.in.Write(append(b, '\n'))
	return err
}

func (h *Host) id() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	return h.next
}

// Call 调一个方法，等它回复，把结果解析进 out。
func (h *Host) Call(ctx context.Context, method string, params, out any) error {
	id := h.id()
	c := make(chan msg, 1)
	h.mu.Lock()
	h.calls[id] = c
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.calls, id)
		h.mu.Unlock()
	}()
	if err := h.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case m := <-c:
		if m.Error != nil {
			return errors.New(m.Error.Message)
		}
		if out != nil && len(m.Result) > 0 {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-h.done:
		return h.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Host) onFetch(m msg, f *fetchReply) {
	switch {
	case m.Event == "head":
		f.head <- m
	case m.Event == "chunk":
		b, _ := base64.StdEncoding.DecodeString(m.Data)
		f.pw.Write(b)
	default: // 结束或出错
		h.mu.Lock()
		delete(h.fetchs, m.ID)
		h.mu.Unlock()
		if m.Error != nil {
			select {
			case f.head <- m:
			default:
			}
			f.pw.CloseWithError(errors.New(m.Error.Message))
			return
		}
		f.pw.Close()
	}
}

// Request 是交给插件 fetch 的一次请求。Path 接在插件给出的 baseURL 后面，例如 /chat/completions。
type Request struct {
	Provider string
	Method   string
	Path     string
	Header   http.Header
	Body     []byte
}

// Fetch 通过插件发请求。返回的 Body 边收边读；ctx 取消时请求也取消。
func (h *Host) Fetch(ctx context.Context, r Request) (*http.Response, error) {
	id := h.id()
	pr, pw := io.Pipe()
	f := &fetchReply{head: make(chan msg, 1), pw: pw}
	h.mu.Lock()
	h.fetchs[id] = f
	h.mu.Unlock()
	hs := map[string]string{}
	for k, v := range r.Header {
		if len(v) > 0 {
			hs[k] = v[0]
		}
	}
	err := h.send(map[string]any{"id": id, "method": "fetch", "params": map[string]any{
		"provider": r.Provider, "method": r.Method, "path": r.Path, "headers": hs,
		"body": base64.StdEncoding.EncodeToString(r.Body),
	}})
	if err != nil {
		return nil, err
	}
	abort := func() { h.send(map[string]any{"method": "abort", "params": map[string]any{"id": id}}) }
	select {
	case m := <-f.head:
		if m.Error != nil {
			return nil, errors.New(m.Error.Message)
		}
		res := &http.Response{StatusCode: m.Status, Header: http.Header{}, Body: pr}
		for k, v := range m.Headers {
			res.Header.Set(k, v)
		}
		go func() {
			<-ctx.Done()
			abort()
			pw.CloseWithError(ctx.Err())
		}()
		return res, nil
	case <-ctx.Done():
		abort()
		return nil, ctx.Err()
	case <-h.done:
		return nil, h.err
	}
}

// Providers 列出已加载插件提供的 provider 和它们的模型。
func (h *Host) Providers(ctx context.Context) ([]Provider, error) {
	var out []Provider
	if err := h.Call(ctx, "providers", nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Provider{}
	}
	return out, nil
}
