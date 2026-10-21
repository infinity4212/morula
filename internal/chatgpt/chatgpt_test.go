package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jwt 造一个假 token，只有载荷有用。
func jwt(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

// fakeHome 在临时目录里放一份假的 codex 登录信息，CODEX_HOME 指过去。不碰用户的 ~/.codex。
func fakeHome(t *testing.T, access string, extra string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	auth := fmt.Sprintf(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,%s"tokens":{"id_token":%q,"access_token":%q,"refresh_token":"rt-old","account_id":"acct-1"},"last_refresh":"2026-01-01T00:00:00Z"}`,
		extra, jwt(map[string]any{"email": "someone@example.com"}), access)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestTokenFresh(t *testing.T) {
	tok := jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	fakeHome(t, tok, "")
	tokenURL = "http://127.0.0.1:1/never" // 没过期时不该刷新
	got, acct, err := Token(context.Background())
	if err != nil || got != tok || acct != "acct-1" {
		t.Fatalf("Token = %q %q %v", got, acct, err)
	}
	if !SignedIn() {
		t.Fatal("SignedIn 应为真")
	}
}

func TestTokenRefreshWritesBack(t *testing.T) {
	old := jwt(map[string]any{"exp": time.Now().Add(time.Minute).Unix()}) // 不到 5 分钟，要刷新
	home := fakeHome(t, old, `"custom_field":"keep",`)
	newTok := jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var b map[string]string
		json.NewDecoder(r.Body).Decode(&b)
		if b["grant_type"] != "refresh_token" || b["refresh_token"] != "rt-old" || b["client_id"] != clientID {
			http.Error(w, "bad", 400)
			return
		}
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt-new","id_token":"id-new"}`, newTok)
	}))
	defer srv.Close()
	tokenURL = srv.URL
	got, _, err := Token(context.Background())
	if err != nil || got != newTok {
		t.Fatalf("Token = %q %v", got, err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "auth.json"))
	var a map[string]any
	json.Unmarshal(b, &a)
	toks := a["tokens"].(map[string]any)
	if toks["refresh_token"] != "rt-new" || toks["access_token"] != newTok || toks["account_id"] != "acct-1" || a["custom_field"] != "keep" {
		t.Fatalf("写回的内容不对：%s", b)
	}
	if fi, _ := os.Stat(filepath.Join(home, "auth.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限变成了 %v", fi.Mode().Perm())
	}
	// 第二次读到写回的新 token，不再刷新
	if _, _, err := Token(context.Background()); err != nil || calls != 1 {
		t.Fatalf("第二次 Token：err=%v，刷新了 %d 次", err, calls)
	}
	left, _ := filepath.Glob(filepath.Join(home, ".auth.json.morula-*"))
	if len(left) > 0 {
		t.Fatalf("临时文件没删：%v", left)
	}
}

func TestTokenRevoked(t *testing.T) {
	fakeHome(t, jwt(map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"error":{"code":"refresh_token_reused"}}`)
	}))
	defer srv.Close()
	tokenURL = srv.URL
	if _, _, err := Token(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("err = %v，应为 ErrSignedOut", err)
	}
}

func TestAPIKeyLoginNotSignedIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`), 0o600)
	if SignedIn() {
		t.Fatal("API key 登录不算 ChatGPT 订阅")
	}
	if _, _, err := Token(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("err = %v", err)
	}
}

type fakeKeychain struct {
	items map[string][]byte
}

func (k *fakeKeychain) Find(s, a string) ([]byte, error) {
	if b, ok := k.items[s+"/"+a]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func (k *fakeKeychain) Put(s, a string, b []byte) error {
	k.items[s+"/"+a] = b
	return nil
}

func TestKeyringStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"x\"\ncli_auth_credentials_store = \"keyring\" # 钥匙串\n[features]\n"), 0o644)
	kc := &fakeKeychain{items: map[string][]byte{}}
	saved := keychain
	keychain = kc
	defer func() { keychain = saved }()
	tok := jwt(map[string]any{"exp": time.Now().Add(time.Minute).Unix()})
	kc.items[keychainService+"/"+keychainAccount(home)] = []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"` + tok + `","refresh_token":"rt-old","account_id":"a"}}`)
	newTok := jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt-new"}`, newTok)
	}))
	defer srv.Close()
	tokenURL = srv.URL
	if got, _, err := Token(context.Background()); err != nil || got != newTok {
		t.Fatalf("Token = %q %v", got, err)
	}
	b := kc.items[keychainService+"/"+keychainAccount(home)]
	if !strings.Contains(string(b), "rt-new") || strings.Contains(string(b), "\n") {
		t.Fatalf("钥匙串写回的内容不对：%s", b)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err == nil {
		t.Fatal("keyring 模式不该写 auth.json")
	}
}

func TestEphemeralStore(t *testing.T) {
	home := fakeHome(t, "x", "")
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(`cli_auth_credentials_store = "ephemeral"`), 0o644)
	if SignedIn() {
		t.Fatal("ephemeral 读不到登录信息")
	}
}

func TestModels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"client_version":"9.9.9","models":[
		{"slug":"m-a","display_name":"A","visibility":"list","context_window":1000,"base_instructions":"base A","supported_reasoning_levels":[{"effort":"low"}]},
		{"slug":"m-b","visibility":"list","model_messages":{"instructions_template":"tmpl B"}},
		{"slug":"m-hidden","visibility":"hide"},
		{"slug":"claude/x","visibility":"list"}]}`), 0o644)
	ms := Models()
	if len(ms) != 2 || ms[0].ID != "m-a" || ms[0].Instructions != "base A" || ms[0].Context != 1000 || ms[1].Instructions != "tmpl B" || ms[1].Name != "m-b" {
		t.Fatalf("Models = %+v", ms)
	}
}

func TestBody(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[{"slug":"m","visibility":"list","base_instructions":"You are Codex.\nmore"}]}`), 0o644)
	var m map[string]any
	json.Unmarshal(Body(map[string]any{"model": "m", "instructions": "你是助理", "temperature": 0.2, "max_output_tokens": 10,
		"input": []any{map[string]any{"type": "item_reference", "id": "x"}, map[string]any{"type": "message", "role": "system", "content": "s"},
			map[string]any{"type": "message", "role": "user", "content": "hi"}},
		"reasoning": map[string]any{"effort": "ultra"}}), &m)
	in := m["input"].([]any)
	first := in[0].(map[string]any)
	if m["instructions"] != "You are Codex.\nmore" || first["role"] != "developer" || len(in) != 3 || in[1].(map[string]any)["role"] != "developer" {
		t.Fatalf("instructions 或 input 不对：%v", m)
	}
	if _, ok := m["temperature"]; ok || m["store"] != false || m["stream"] != true || m["prompt_cache_key"] == nil {
		t.Fatalf("参数不对：%v", m)
	}
	if r := m["reasoning"].(map[string]any); r["effort"] != "max" || r["summary"] != "auto" {
		t.Fatalf("reasoning 不对：%v", r)
	}
	// codex 自己的 instructions 原样保留
	json.Unmarshal(Body(map[string]any{"model": "m", "instructions": "You are Codex.\nother", "input": "hi"}), &m)
	if m["instructions"] != "You are Codex.\nother" || len(m["input"].([]any)) != 1 {
		t.Fatalf("codex 的 instructions 被换掉了：%v", m)
	}
}

func TestDoSigns(t *testing.T) {
	tok := jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	home := fakeHome(t, tok, "")
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"client_version":"1.2.3","models":[]}`), 0o644)
	var h http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		if r.URL.Path != "/backend-api/codex/responses" || r.ProtoMajor != 1 {
			http.Error(w, r.URL.Path, 404)
		}
	}))
	defer srv.Close()
	saved := Base
	Base = srv.URL + "/backend-api/codex"
	defer func() { Base = saved }()
	res, err := Do(context.Background(), []byte(`{"prompt_cache_key":"k1"}`))
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("Do: %v %v", err, res)
	}
	res.Body.Close()
	if h.Get("Authorization") != "Bearer "+tok || h.Get("chatgpt-account-id") != "acct-1" || h.Get("originator") != "codex_cli_rs" ||
		!strings.HasPrefix(h.Get("User-Agent"), "codex_cli_rs/1.2.3 ") || h.Get("session-id") != "k1" || h.Get("version") != "1.2.3" {
		t.Fatalf("请求头不对：%v", h)
	}
}
