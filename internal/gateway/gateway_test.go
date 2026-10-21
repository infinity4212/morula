package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"morula/internal/plugin"
)

// API key 来源不经插件运行器：网关换掉模型名、带上用户的 key，直接发给它的接口。
func TestDirectAPI(t *testing.T) {
	t.Setenv("MORULA_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir()) // 不读用户的 codex 登录信息
	plugin.Forget()
	var got struct {
		path, auth, model string
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Model string }
		json.NewDecoder(r.Body).Decode(&b)
		got.path, got.auth, got.model = r.URL.Path, r.Header.Get("Authorization"), b.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer up.Close()
	if _, err := plugin.AddAPIKey(context.Background(), plugin.APIKey{ID: "up", BaseURL: up.URL + "/v1", Key: "sk-user", Models: []string{"m1"}}); err != nil {
		t.Fatal(err)
	}
	g, err := Start()
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	req, _ := http.NewRequest("POST", g.BaseURL()+"/chat/completions", strings.NewReader(`{"model":"up/m1","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+g.Key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), `"hi"`) {
		t.Fatalf("状态 %d：%s", res.StatusCode, body)
	}
	if got.path != "/v1/chat/completions" || got.auth != "Bearer sk-user" || got.model != "m1" {
		t.Errorf("转发不对：%+v", got)
	}
}
