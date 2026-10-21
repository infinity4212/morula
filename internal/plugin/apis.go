package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// API key 来源：用户自己填的 OpenAI 兼容接口（base URL + key）。不经插件运行器，
// 网关直接转发 Chat Completions 请求。它在模型列表里和插件 provider 一样写作 <id>/<模型>。

// APIKey 是一个 API key 来源。Key 只存在 api-keys.json（权限 0600）里，不进接口回复。
type APIKey struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	BaseURL string   `json:"baseUrl"`
	Key     string   `json:"key"`
	Models  []string `json:"models"`
}

var apiMu sync.Mutex

func apiPath() (string, error) {
	h, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, "api-keys.json"), nil
}

// APIKeys 读全部 API key 来源。文件不存在时返回空。
func APIKeys() ([]APIKey, error) {
	apiMu.Lock()
	defer apiMu.Unlock()
	return readAPIs()
}

func readAPIs() ([]APIKey, error) {
	p, err := apiPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return []APIKey{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []APIKey
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("api-keys.json 格式不对：%w", err)
	}
	return out, nil
}

func writeAPIs(list []APIKey) error {
	p, err := apiPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

var idRe = regexp.MustCompile(`[^a-z0-9-]+`)

// AddAPIKey 校验后存下一个 API key 来源。models 为空时向接口要模型列表；要不到就报错，让用户手填。
// id 已存在时覆盖。
func AddAPIKey(ctx context.Context, a APIKey) (APIKey, error) {
	a.Name = strings.TrimSpace(a.Name)
	a.BaseURL = strings.TrimRight(strings.TrimSpace(a.BaseURL), "/")
	a.Key = strings.TrimSpace(a.Key)
	u, err := url.Parse(a.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return a, errors.New("接口地址要写完整的 http(s) 地址，例如 https://api.deepseek.com/v1")
	}
	if a.Key == "" {
		return a, errors.New("API 密钥不能为空")
	}
	if a.Name == "" {
		a.Name = u.Host
	}
	if a.ID = strings.Trim(idRe.ReplaceAllString(strings.ToLower(a.ID), "-"), "-"); a.ID == "" {
		a.ID = strings.Trim(idRe.ReplaceAllString(strings.ToLower(strings.TrimPrefix(u.Hostname(), "api.")), "-"), "-")
	}
	var models []string
	for _, m := range a.Models {
		if m = strings.TrimSpace(m); m != "" && !slices.Contains(models, m) {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		if models, err = listModels(ctx, a.BaseURL, a.Key); err != nil {
			return a, fmt.Errorf("读不到模型列表（%v），请手填模型名", err)
		}
	}
	a.Models = models
	apiMu.Lock()
	defer apiMu.Unlock()
	list, err := readAPIs()
	if err != nil {
		return a, err
	}
	for _, p := range Cached() {
		if p.ID == a.ID && !p.Builtin {
			return a, fmt.Errorf("名字 %q 已被插件的模型来源用了，换一个", a.ID)
		}
	}
	list = slices.DeleteFunc(list, func(x APIKey) bool { return x.ID == a.ID })
	list = append(list, a)
	if err := writeAPIs(list); err != nil {
		return a, err
	}
	Forget()
	return a, nil
}

// RemoveAPIKey 删掉一个 API key 来源和它存的 key。
func RemoveAPIKey(id string) error {
	apiMu.Lock()
	defer apiMu.Unlock()
	list, err := readAPIs()
	if err != nil {
		return err
	}
	n := len(list)
	list = slices.DeleteFunc(list, func(x APIKey) bool { return x.ID == id })
	if len(list) == n {
		return errors.New("没有这个 API 密钥：" + id)
	}
	Forget()
	return writeAPIs(list)
}

// listModels 调 GET <base>/models。
func listModels(ctx context.Context, base, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("状态 %d：%s", res.StatusCode, clip(strings.TrimSpace(string(b)), 200))
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, errors.New("回复不是模型列表")
	}
	var out []string
	for _, m := range r.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("模型列表是空的")
	}
	return out, nil
}

// apiProviders 把 API key 来源转成 provider，和插件的 provider 一起列。
func apiProviders() []Provider {
	list, _ := APIKeys()
	out := []Provider{}
	for _, a := range list {
		p := Provider{ID: a.ID, Name: a.Name, API: a.BaseURL, SignedIn: true, AuthType: "api", Builtin: true, Methods: []Method{}}
		for _, m := range a.Models {
			p.Models = append(p.Models, Model{ID: m, Name: m, Efforts: []string{}})
		}
		out = append(out, p)
	}
	return out
}

// APIKeyFor 返回一个 API key 来源；不是 API key 来源时 ok 为假。
func APIKeyFor(id string) (APIKey, bool) {
	list, _ := APIKeys()
	for _, a := range list {
		if a.ID == id {
			return a, true
		}
	}
	return APIKey{}, false
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
