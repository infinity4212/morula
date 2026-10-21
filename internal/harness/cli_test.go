package harness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 假程序：bin/<名字> 链到 real 下的脚本，脚本打印 out。
func fakeBin(t *testing.T, dir, name, real, out string) {
	t.Helper()
	full := filepath.Join(dir, real)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("#!/bin/sh\n"+out+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(full, filepath.Join(dir, "bin", name)); err != nil {
		t.Fatal(err)
	}
}

func TestCLIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/@openai/codex/latest":
			w.Write([]byte(`{"version":"0.163.0"}`))
		case "/opencode-ai/latest":
			w.Write([]byte(`{"version":"1.19.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	npmRegistry, brewAPI = srv.URL, srv.URL
	latestCache.m = map[string]latestHit{}

	dir := t.TempDir()
	fakeBin(t, dir, "codex", "lib/node_modules/@openai/codex/bin/codex.js", "echo codex-cli 0.162.1")
	fakeBin(t, dir, "opencode", ".opencode/bin/opencode", "echo 1.19.0")
	fakeBin(t, dir, "hermes", ".local/bin/hermes", "echo 'venv/bin/hermes: No such file or directory' >&2; exit 127")
	t.Setenv("PATH", filepath.Join(dir, "bin")+":/usr/bin:/bin")

	got := CLIs(context.Background(), []string{"codex", "opencode", "hermes", "pi"})
	if c := got["codex"]; c.Version != "0.162.1" || c.Via != "npm" || c.Command != "npm install -g @openai/codex@latest" || c.Latest != "0.163.0" || !c.Update {
		t.Errorf("codex = %+v", c)
	}
	if c := got["opencode"]; c.Via != "self" || c.Command != "opencode upgrade" || c.Update {
		t.Errorf("opencode = %+v，同版本不该提示更新", c)
	}
	if c := got["hermes"]; c.Version != "" || c.Broken == "" || len(c.Install) == 0 || c.Update {
		t.Errorf("hermes = %+v，跑不起来时要给出输出和安装命令", c)
	}
	if c := got["pi"]; c.Path != "" || len(c.Install) != 1 || c.Install[0].Command != "npm install -g @earendil-works/pi-coding-agent" {
		t.Errorf("pi = %+v，没装时只给 npm 安装命令", c)
	}
}

func TestSetup(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "codex", "lib/codex.js", "echo codex-cli 0.162.1")
	fakeBin(t, dir, "hermes", ".local/bin/hermes", "echo 'venv/bin/hermes: No such file or directory' >&2; exit 127")
	t.Setenv("PATH", filepath.Join(dir, "bin")+":/usr/bin:/bin")
	ForgetSetup()
	if ok, broken, _ := Setup("codex"); !ok || broken != "" {
		t.Errorf("codex：ok=%v broken=%q，能跑 --version 就算装好", ok, broken)
	}
	if ok, broken, cmds := Setup("hermes"); ok || !strings.Contains(broken, "No such file") || len(cmds) == 0 {
		t.Errorf("hermes：ok=%v broken=%q，启动脚本跑不起来要算没装好、给安装命令", ok, broken)
	}
	if ok, broken, cmds := Setup("pi"); ok || broken != "" || len(cmds) == 0 {
		t.Errorf("pi：ok=%v broken=%q，不在 PATH 里就是没装", ok, broken)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"0.163.0", "0.162.1", true}, {"1.19.0", "1.19.0", false}, {"1.3.0", "1.10.0", false}, {"2.0.0-beta.1", "1.9.9", true}} {
		if got := newer(c.a, c.b); got != c.want {
			t.Errorf("newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

// InstallCLI 在跑命令之前拒收的几种情况。这里不真的装：每种情况都在 exec 之前返回。
func TestInstallRefuse(t *testing.T) {
	ctx := context.Background()
	t.Setenv("PATH", t.TempDir())
	if _, err := InstallCLI(ctx, "nope", "npm"); err == nil || !strings.Contains(err.Error(), "不知道") {
		t.Fatalf("没有这个 harness：%v", err)
	}
	if _, err := InstallCLI(ctx, "pi", "brew"); err == nil || !strings.Contains(err.Error(), "没有 brew") {
		t.Fatalf("没有这种装法：%v", err)
	}
	if _, err := InstallCLI(ctx, "pi", "npm"); err == nil || !strings.Contains(err.Error(), "要先装 Node.js") {
		t.Fatalf("没有 npm：%v", err)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "npm"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin)
	installing.Store("pi", true)
	defer installing.Delete("pi")
	if _, err := InstallCLI(ctx, "pi", "npm"); err == nil || !strings.Contains(err.Error(), "正在安装") {
		t.Fatalf("同时装两次：%v", err)
	}
}
