// Package plugin 加载 OpenCode 格式的 provider 插件：登录、列模型、发请求、读额度。
//
// 插件是 npm 包或本地目录，用 Bun 装进 morula 数据目录下的 plugins/，由 host.js 在 Bun 里运行。
// 插件在 package.json 的 morula 字段里写自己在插件页怎么显示（见 Meta）。
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"morula/internal/store"
)

// Entry 是 plugins.json 里的一行。
type Entry struct {
	Spec string `json:"spec"`          // 装的时候用的写法：npm 包名（可带版本）或本地目录
	Name string `json:"name"`          // 包名，node_modules 里的目录
	Off  bool   `json:"off,omitempty"` // 停用：不加载，但不删
}

var mu sync.Mutex // 改 plugins.json 和 plugins/ 时持有

func home() (string, error) {
	d, err := store.Dir()
	if err != nil {
		return "", err
	}
	return d, nil
}

// Dir 是装插件的目录（有自己的 package.json 和 node_modules）。
func Dir() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	d := filepath.Join(h, "plugins")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	pkg := filepath.Join(d, "package.json")
	if _, err := os.Stat(pkg); errors.Is(err, os.ErrNotExist) {
		os.WriteFile(pkg, []byte(`{"name":"morula-plugins","private":true,"type":"module","dependencies":{}}`+"\n"), 0o644)
	}
	return d, nil
}

func listPath() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "plugins.json"), nil
}

// AuthPath 是插件存凭据的文件，形状与 OpenCode 的 auth.json 相同。
func AuthPath() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "plugin-auth.json"), nil
}

// List 返回装过的插件。
func List() ([]Entry, error) {
	p, err := listPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Plugins []Entry `json:"plugins"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("plugins.json 读不懂：%w", err)
	}
	if f.Plugins == nil {
		f.Plugins = []Entry{}
	}
	return f.Plugins, nil
}

func save(list []Entry) error {
	p, err := listPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(map[string]any{"plugins": list}, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Bun 找本机的 bun。
func Bun() (string, error) {
	if p, err := exec.LookPath("bun"); err == nil {
		return p, nil
	}
	h, _ := os.UserHomeDir()
	if p := filepath.Join(h, ".bun", "bin", "bun"); fileExists(p) {
		return p, nil
	}
	return "", errors.New("没找到 bun：插件要用 Bun 运行，先装 Bun（https://bun.sh）")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func deps(dir string) map[string]string {
	var pkg struct {
		Deps map[string]string `json:"dependencies"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	json.Unmarshal(b, &pkg)
	if pkg.Deps == nil {
		pkg.Deps = map[string]string{}
	}
	return pkg.Deps
}

func bunRun(ctx context.Context, dir string, args ...string) error {
	bun, err := Bun()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bun %s 失败：%w\n%s", strings.Join(args, " "), err, tail(string(out), 1200))
	}
	return nil
}

// Add 用 bun add 装一个插件，记进 plugins.json。已装过同一个包时按新写法重装。
func Add(ctx context.Context, spec string) (Entry, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Entry{}, errors.New("插件名是空的")
	}
	mu.Lock()
	defer mu.Unlock()
	dir, err := Dir()
	if err != nil {
		return Entry{}, err
	}
	before := deps(dir)
	if err := bunRun(ctx, dir, "add", spec); err != nil {
		return Entry{}, err
	}
	name := nameOf(spec)
	after := deps(dir)
	if _, ok := after[name]; !ok {
		// git、本地目录这类写法看不出包名：取新增的那个依赖
		for k := range after {
			if _, was := before[k]; !was {
				name = k
			}
		}
	}
	if _, ok := after[name]; !ok {
		return Entry{}, fmt.Errorf("装完了，但找不到 %s 的包名", spec)
	}
	list, err := List()
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Spec: spec, Name: name}
	i := slices.IndexFunc(list, func(x Entry) bool { return x.Name == name })
	if i >= 0 {
		e.Off = list[i].Off
		list[i] = e
	} else {
		list = append(list, e)
	}
	if err := save(list); err != nil {
		return Entry{}, err
	}
	return e, nil
}

// nameOf 从 npm 写法里取包名：@scope/name@1.2 → @scope/name。
func nameOf(spec string) string {
	s := spec
	if strings.HasPrefix(s, "npm:") {
		s = s[4:]
	}
	at := strings.LastIndex(s, "@")
	if at > 0 {
		s = s[:at]
	}
	return s
}

// Remove 卸掉一个插件。它存下的凭据留在 plugin-auth.json 里，重装后不用再登录；要清掉就先在插件页退出。
func Remove(ctx context.Context, name string) error {
	mu.Lock()
	defer mu.Unlock()
	list, err := List()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(x Entry) bool { return x.Name == name })
	if i < 0 {
		return errors.New("没有这个插件：" + name)
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := bunRun(ctx, dir, "remove", name); err != nil {
		return err
	}
	return save(slices.Delete(list, i, i+1))
}

// SetOff 停用或启用一个插件。
func SetOff(name string, off bool) error {
	mu.Lock()
	defer mu.Unlock()
	list, err := List()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(x Entry) bool { return x.Name == name })
	if i < 0 {
		return errors.New("没有这个插件：" + name)
	}
	list[i].Off = off
	return save(list)
}

// Info 是插件页显示的一个插件：版本取 package.json 的 version，其余取 morula 字段，
// morula 字段里没写的退回 package.json 的 description、homepage。
//
//	"morula": {"title": "WorkBuddy", "description": "…", "homepage": "https://…", "notice": "…"}
//
// notice 是装之前要用户知道的事，例如经第三方调用订阅可能违反厂商条款。
type Info struct {
	Version     string `json:"version"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Homepage    string `json:"homepage,omitempty"`
	Notice      string `json:"notice,omitempty"`
}

// Meta 读装好的包的 package.json。读不到时返回空的 Info。
func Meta(name string) Info {
	dir, err := Dir()
	if err != nil {
		return Info{}
	}
	var pkg struct {
		Info
		Morula Info `json:"morula"`
	}
	b, _ := os.ReadFile(filepath.Join(dir, "node_modules", name, "package.json"))
	json.Unmarshal(b, &pkg)
	in, m := pkg.Info, pkg.Morula
	pick := func(to *string, v string) {
		if v != "" {
			*to = v
		}
	}
	pick(&in.Title, m.Title)
	pick(&in.Description, m.Description)
	pick(&in.Homepage, m.Homepage)
	pick(&in.Notice, m.Notice)
	return in
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
