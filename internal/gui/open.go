package gui

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// 打开链接：回复里的 Markdown 链接点了以后由 morula 用 macOS 的 open 打开。窗口是 WKWebView，
// 页面自己跳转会离开 morula。链接是模型写的，所以只开网页和本机已有的文件；
// 能运行的文件（程序、脚本、安装包）不直接打开，只在访达里选中。

// lineSuffix 是模型常在路径后面写的行号：「a.html:173」「a.go:12:5」「a.md#L40」。
var lineSuffix = regexp.MustCompile(`(:\d+(:\d+)?|#L\d+(-L?\d+)?)$`)

// runnable 是打开就会运行东西的扩展名。
var runnable = map[string]bool{".app": true, ".command": true, ".sh": true, ".zsh": true, ".bash": true,
	".tool": true, ".terminal": true, ".pkg": true, ".mpkg": true, ".dmg": true, ".scpt": true,
	".applescript": true, ".workflow": true, ".action": true, ".jar": true, ".py": true, ".rb": true,
	".pl": true, ".js": true, ".webloc": true, ".inetloc": true, ".fileloc": true, ".prefPane": true,
	".kext": true, ".mobileconfig": true}

// openArgs 把链接换成 open 的参数。网页原样打开；本机路径去掉行号后要存在，能运行的加 -R 只选中。
func openArgs(link string) ([]string, error) {
	link = strings.TrimSpace(link)
	if u, err := url.Parse(link); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return []string{link}, nil
	}
	p := link
	if strings.HasPrefix(p, "file://") {
		u, err := url.Parse(p)
		if err != nil {
			return nil, err
		}
		p = u.Path
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return nil, errors.New("只能打开网页和本机的绝对路径")
	}
	st, err := os.Stat(p)
	if err != nil {
		if q := lineSuffix.ReplaceAllString(p, ""); q != p {
			p = q
			st, err = os.Stat(p)
		}
	}
	if err != nil {
		return nil, errors.New("文件不存在：" + p)
	}
	ext := strings.ToLower(filepath.Ext(p))
	for k := range runnable {
		if strings.ToLower(k) == ext {
			return []string{"-R", p}, nil
		}
	}
	if !st.IsDir() && st.Mode()&0o111 != 0 {
		return []string{"-R", p}, nil
	}
	return []string{p}, nil
}

func openRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		// 只收 JSON：别的网页跨源发不了这种请求（要先预检，morula 不答预检）。
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			fail(w, http.StatusUnsupportedMediaType, "要 JSON")
			return
		}
		var req struct {
			Link string `json:"link"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		args, err := openArgs(req.Link)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := exec.Command("open", args...).Run(); err != nil {
			fail(w, http.StatusInternalServerError, "打不开："+err.Error())
			return
		}
		reply(w, map[string]bool{"ok": true})
	})
}
