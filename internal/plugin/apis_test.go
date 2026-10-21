package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAPIKeys(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir()) // 不读用户的 codex 登录信息
	Forget()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer sk-test" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer up.Close()
	ctx := context.Background()

	if _, err := AddAPIKey(ctx, APIKey{BaseURL: "ftp://x", Key: "k"}); err == nil {
		t.Error("ftp 地址应被拒绝")
	}
	if _, err := AddAPIKey(ctx, APIKey{ID: "up", BaseURL: up.URL + "/v1", Key: "wrong"}); err == nil {
		t.Error("读不到模型列表时应报错")
	}
	a, err := AddAPIKey(ctx, APIKey{ID: "up", BaseURL: up.URL + "/v1/", Key: " sk-test "})
	if err != nil {
		t.Fatal(err)
	}
	if a.BaseURL != up.URL+"/v1" || len(a.Models) != 2 || a.Key != "sk-test" {
		t.Errorf("存的不对：%+v", a)
	}
	st, err := os.Stat(filepath.Join(os.Getenv("MORULA_HOME"), "api-keys.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("api-keys.json 权限应为 0600：%v %v", st.Mode(), err)
	}
	ref, ok := Find(ctx, "up/m2")
	if !ok || !ref.Provider.Builtin || ref.Provider.API != a.BaseURL {
		t.Errorf("Find 找不到 API key 来源的模型：%+v %v", ref, ok)
	}
	// 手填模型时不调接口
	if b, err := AddAPIKey(ctx, APIKey{ID: "hand", BaseURL: "https://example.invalid/v1", Key: "k", Models: []string{" x ", "x", "y"}}); err != nil || len(b.Models) != 2 {
		t.Errorf("手填模型：%+v %v", b, err)
	}
	if err := RemoveAPIKey("up"); err != nil {
		t.Fatal(err)
	}
	if _, ok := Find(ctx, "up/m2"); ok {
		t.Error("删掉后还能找到")
	}
	if err := RemoveAPIKey("up"); err == nil {
		t.Error("删不存在的来源应报错")
	}
}
