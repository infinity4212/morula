// Package chatgpt 把 codex 登录的 ChatGPT 订阅接到 morula 的网关上，让 opencode、pi 也能用这个订阅的模型。
//
// 登录信息用 codex 自己的：按 CODEX_HOME/config.toml 的 cli_auth_credentials_store 读 auth.json 或 macOS 钥匙串。
// access token 快过期时刷新，新 token 写回原处，codex 下次读到的就是新的。刷新了不写回，旧的 refresh token
// 作废，codex 会被登出（refresh_token_reused）。
//
// 读登录信息、刷新、签名请求、改写请求体的做法改写自 magpie（https://github.com/yetone/magpie，
// MIT License，Copyright (c) 2026 yetone），见仓库根目录的 NOTICE。
package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// 测试里改成假的地址。
var (
	tokenURL = "https://auth.openai.com/oauth/token"
	Base     = "https://chatgpt.com/backend-api/codex"
)

const clientID = "app_EMoamEEZ73f0CkXaXp7hrann" // codex CLI 自己的 OAuth client

// ErrSignedOut 表示 codex 没登录 ChatGPT，或登录已失效。
var ErrSignedOut = errors.New("codex 没有登录 ChatGPT 账号，或登录已失效：在终端执行 codex login 重新登录")

// Home 是 codex 的数据目录。
func Home() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// store 是登录信息存在哪：auth.json 或钥匙串。
type store struct {
	home     string
	keychain bool
}

// storeOf 读 config.toml 的 cli_auth_credentials_store。没写时是 file。
// keyring、auto 在 macOS 上读钥匙串（auto 读不到时退回 auth.json）；ephemeral 只在 codex 内存里，读不到。
func storeOf(home string) (store, bool, error) {
	setting := "file"
	if f, err := os.Open(filepath.Join(home, "config.toml")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			l := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(l, "[") {
				break // 只看顶层的键
			}
			k, v, ok := strings.Cut(l, "=")
			if ok && strings.TrimSpace(k) == "cli_auth_credentials_store" {
				v = strings.TrimSpace(v)
				if i := strings.Index(v, "#"); i >= 0 {
					v = strings.TrimSpace(v[:i])
				}
				setting = strings.Trim(v, `"'`)
			}
		}
		f.Close()
	}
	switch setting {
	case "file":
		return store{home: home}, false, nil
	case "keyring":
		if runtime.GOOS != "darwin" {
			return store{}, false, errors.New("codex 把登录信息存在系统钥匙串里，morula 只在 macOS 上读得了")
		}
		return store{home: home, keychain: true}, false, nil
	case "auto":
		return store{home: home, keychain: runtime.GOOS == "darwin"}, true, nil
	case "ephemeral":
		return store{}, false, errors.New("codex 的登录信息只存在它自己的内存里（cli_auth_credentials_store = ephemeral），morula 读不到")
	}
	return store{}, false, fmt.Errorf("config.toml 里的 cli_auth_credentials_store = %q 认不出", setting)
}

func (s store) file() string { return filepath.Join(s.home, "auth.json") }

// keychainAccount 是 codex 的钥匙串账号名：cli| 加 CODEX_HOME 规范路径的 SHA-256 前 16 位。
func keychainAccount(home string) string {
	if p, err := filepath.EvalSymlinks(home); err == nil {
		home = p
	}
	if p, err := filepath.Abs(home); err == nil {
		home = p
	}
	sum := sha256.Sum256([]byte(home))
	return "cli|" + hex.EncodeToString(sum[:])[:16]
}

const keychainService = "Codex Auth"

// 测试里换成假的，不碰用户的钥匙串。
var keychain interface {
	Find(service, account string) ([]byte, error)
	Put(service, account string, secret []byte) error
} = security{}

type security struct{}

// Find 读钥匙串里的条目。macOS 可能弹窗问用户，最多等 2 分钟。
func (security) Find(service, account string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("钥匙串没有把 codex 的登录信息给 morula（可能是弹窗被拒绝或没有回答）")
	}
	return bytes.TrimSpace(out), nil
}

// Put 写回钥匙串。命令行参数里不带出错信息，免得把 token 写进日志。
func (security) Put(service, account string, secret []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "/usr/bin/security", "add-generic-password", "-U", "-s", service, "-a", account, "-w", string(secret)).Run() != nil {
		return errors.New("钥匙串没有接受刷新后的登录信息")
	}
	return nil
}

// read 读登录信息。auto 时钥匙串没有就读 auth.json。
func read(home string) ([]byte, store, error) {
	s, fallback, err := storeOf(home)
	if err != nil {
		return nil, s, err
	}
	if s.keychain {
		b, err := keychain.Find(keychainService, keychainAccount(home))
		if err == nil {
			return b, s, nil
		}
		if !fallback || !errors.Is(err, os.ErrNotExist) {
			if errors.Is(err, os.ErrNotExist) {
				return nil, s, ErrSignedOut
			}
			return nil, s, err
		}
		s.keychain = false
	}
	b, err := os.ReadFile(s.file())
	if errors.Is(err, os.ErrNotExist) {
		return nil, s, ErrSignedOut
	}
	return b, s, err
}

// write 写回原处。auth.json 先写同目录的临时文件再改名，不在原文件上截断重写。
func (s store) write(b []byte) error {
	if s.keychain {
		var v any
		json.Unmarshal(b, &v)
		one, _ := json.Marshal(v) // codex 在钥匙串里存的是一行 JSON
		return keychain.Put(keychainService, keychainAccount(s.home), one)
	}
	p := s.file()
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(p); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".auth.json.morula-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, mode)
	}
	if werr == nil {
		werr = os.Rename(tmp, p)
	}
	if werr != nil {
		os.Remove(tmp)
	}
	return werr
}

type login struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

// claims 读 JWT 的载荷，不验签名（只用来看过期时间和账号 id）。
func claims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func (l login) accountID() string {
	if l.Tokens.AccountID != "" {
		return l.Tokens.AccountID
	}
	if a, ok := claims(l.Tokens.IDToken)["https://api.openai.com/auth"].(map[string]any); ok {
		s, _ := a["chatgpt_account_id"].(string)
		return s
	}
	return ""
}

// fresh 判断 access token 离过期还有没有 5 分钟。没写过期时间的算没过期。
func fresh(tok string) bool {
	exp, _ := claims(tok)["exp"].(float64)
	return exp == 0 || time.Until(time.Unix(int64(exp), 0)) > 5*time.Minute
}

func parse(b []byte) (login, bool) {
	var l login
	if json.Unmarshal(b, &l) != nil || l.AuthMode == "apikey" || l.Tokens.AccessToken == "" {
		return login{}, false
	}
	return l, true
}

// SignedIn 判断 codex 有没有登录 ChatGPT 账号。用 API key 登录的不算。
// 登录信息在钥匙串里时不读（读会弹窗），按 config.toml 的设置算已登录。
func SignedIn() bool {
	home := Home()
	s, fallback, err := storeOf(home)
	if err != nil {
		return false
	}
	if s.keychain {
		if !fallback {
			return true
		}
		if _, err := os.Stat(s.file()); err != nil {
			return true // auto 且没有 auth.json：登录信息在钥匙串里
		}
	}
	b, err := os.ReadFile(s.file())
	if err != nil {
		return false
	}
	_, ok := parse(b)
	return ok
}

var mu sync.Mutex

// Token 返回能用的 access token 和账号 id。快过期时刷新，新 token 写回 codex 的登录信息里。
// 每次都重新读登录信息：codex 自己也会刷新，用旧的 refresh token 会被拒。
func Token(ctx context.Context) (tok, account string, err error) {
	mu.Lock()
	defer mu.Unlock()
	b, s, err := read(Home())
	if err != nil {
		return "", "", err
	}
	l, ok := parse(b)
	if !ok {
		return "", "", ErrSignedOut
	}
	if fresh(l.Tokens.AccessToken) {
		return l.Tokens.AccessToken, l.accountID(), nil
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return "", "", ErrSignedOut
	}
	tok, err = refresh(ctx, raw)
	if err != nil {
		return "", "", err
	}
	// 其他字段保持 codex 写的样子
	// 写不回去时这次的 access token 照样能用，只是 codex 下次刷新会被拒，要重新登录
	out, _ := json.MarshalIndent(raw, "", "  ")
	if werr := s.write(out); werr != nil {
		log.Println("chatgpt: 刷新后的登录信息没写回去：", werr)
	}
	return tok, l.accountID(), nil
}

// refresh 用 refresh token 换新 token，就地改 raw 里的 tokens 和 last_refresh。
// 请求发出后不跟着调用方取消：OpenAI 回答时就把 refresh token 换掉了，丢掉回答会拿着作废的 token。
func refresh(ctx context.Context, raw map[string]any) (string, error) {
	toks, _ := raw["tokens"].(map[string]any)
	rt, _ := toks["refresh_token"].(string)
	if rt == "" {
		return "", ErrSignedOut
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token",
		"refresh_token": rt, "scope": "openid profile email"})
	req, err := http.NewRequestWithContext(ctx, "POST", tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("刷新 ChatGPT 登录失败：%w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var nt struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if res.StatusCode != 200 || json.Unmarshal(b, &nt) != nil || nt.AccessToken == "" {
		for _, m := range []string{"token_revoked", "invalid_grant", "refresh_token_reused", "token_expired"} {
			if bytes.Contains(b, []byte(m)) {
				return "", ErrSignedOut
			}
		}
		return "", fmt.Errorf("刷新 ChatGPT 登录失败：OpenAI 回复 %d", res.StatusCode)
	}
	toks["access_token"] = nt.AccessToken
	if nt.IDToken != "" {
		toks["id_token"] = nt.IDToken
	}
	if nt.RefreshToken != "" {
		toks["refresh_token"] = nt.RefreshToken
	}
	raw["tokens"] = toks
	raw["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	return nt.AccessToken, nil
}
