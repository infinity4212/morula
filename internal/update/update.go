// Package update 让已安装的 morula.app 从本地通道自更新。
//
// 通道是一个目录：latest.json 描述最新版本，旁边放着对应的 zip。
// `make release` 往里发布；客户端读 latest.json，版本更新就
// 校验 sha256 → 解压到 .app 旁边 → 改名替换 → 等本进程退出后重新打开。
// 不校验签名团队（我们只做 ad-hoc 签名），也不申请管理员权限。
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultFeed 由 Makefile 通过 -ldflags 写入，指向本机的发布目录。
var DefaultFeed = ""

// Feed 是 latest.json 的路径；MORULA_UPDATE_FEED 可以临时改掉它。
func Feed() string {
	if f := os.Getenv("MORULA_UPDATE_FEED"); f != "" {
		return f
	}
	return DefaultFeed
}

type Release struct {
	Version string `json:"version"`
	Notes   string `json:"notes"`
	Zip     string `json:"zip"` // 相对 latest.json 所在目录
	SHA256  string `json:"sha256"`
}

// Latest 读取通道里的最新版本。
func Latest() (*Release, error) {
	feed := Feed()
	if feed == "" {
		return nil, errors.New("没有配置更新通道")
	}
	b, err := os.ReadFile(feed)
	if err != nil {
		return nil, err
	}
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("latest.json 格式不对：%w", err)
	}
	return &r, nil
}

// Newer 判断版本 a 是否比 b 新，只比较 x.y.z 三段数字。
func Newer(a, b string) bool {
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Bundle 是当前进程所在的 .app；不是从 .app 启动（如 go run）时返回 ""。
func Bundle() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return ""
	}
	// …/morula.app/Contents/MacOS/morula
	macos := filepath.Dir(exe)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return ""
	}
	return app
}

// Apply 下载、校验、替换 bundle，并安排本进程退出后重新打开。
// 调用方随后应退出应用。
func Apply(rel *Release, bundle string) error {
	if bundle == "" {
		return errors.New("当前不是从 morula.app 运行，不能自更新")
	}
	src := filepath.Join(filepath.Dir(Feed()), rel.Zip)
	stage := filepath.Join(filepath.Dir(bundle), ".morula-update")
	os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return fmt.Errorf("不能在 %s 旁边建临时目录：%w", bundle, err)
	}
	zip := filepath.Join(stage, "morula.zip")
	if err := copyChecked(src, zip, rel.SHA256); err != nil {
		os.RemoveAll(stage)
		return err
	}
	out := filepath.Join(stage, "app")
	if b, err := exec.Command("ditto", "-x", "-k", zip, out).CombinedOutput(); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("解压失败：%v：%s", err, b)
	}
	staged := filepath.Join(out, "morula.app")
	if b, err := exec.Command("codesign", "--verify", "--deep", "--strict", staged).CombinedOutput(); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("新版本签名校验失败：%s", strings.TrimSpace(string(b)))
	}

	old := filepath.Join(stage, "old.app")
	if err := os.Rename(bundle, old); err != nil {
		os.RemoveAll(stage)
		return fmt.Errorf("替换失败（%s 可能不可写）：%w", filepath.Dir(bundle), err)
	}
	if err := os.Rename(staged, bundle); err != nil {
		os.Rename(old, bundle) // 放回旧版
		os.RemoveAll(stage)
		return fmt.Errorf("替换失败，已放回旧版：%w", err)
	}
	return relaunch(bundle, stage)
}

func copyChecked(src, dst, want string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("读不到更新包：%w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("更新包 sha256 不符：期望 %s，实际 %s", want, got)
	}
	return nil
}

// relaunch 等本进程退出后打开新版；新版 10 秒内没起来就换回旧版再打开。
func relaunch(bundle, stage string) error {
	old := filepath.Join(stage, "old.app")
	script := fmt.Sprintf(`
while kill -0 %d 2>/dev/null; do sleep 0.2; done
open %q
for i in $(seq 1 50); do
  if pgrep -f %q >/dev/null; then rm -rf %q; exit 0; fi
  sleep 0.2
done
rm -rf %q && mv %q %q && open %q
rm -rf %q
`, os.Getpid(), bundle,
		filepath.Join(bundle, "Contents/MacOS/morula"), stage,
		bundle, old, bundle, bundle,
		stage)
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	return cmd.Start()
}
