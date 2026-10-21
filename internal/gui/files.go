package gui

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"morula/internal/chat"
	"morula/internal/journal"
	"morula/internal/store"
)

// 附件：用户拖进来或粘贴的文件先上传，复制进数据目录的 files/<日期>/，发消息时只带 id。
// 浏览器拿不到本机原路径，所以一律复制；单个文件最大 maxFile。

const maxFile = 100 << 20

// 交给 codex 当图片看的格式；其他格式的图片按普通文件处理。
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

func filesDir() (string, error) {
	d, err := store.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "files"), nil
}

// cleanName 只留文件名本身，去掉路径分隔符和控制字符，最长 80 个字。
func cleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == ':' {
			return '_'
		}
		return r
	}, name)
	if r := []rune(name); len(r) > 80 {
		ext := filepath.Ext(name)
		name = string(r[:80-len([]rune(ext))]) + ext
	}
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// saveFile 把上传的内容存下来，返回附件。超过 maxFile 时报错并删掉写了一半的文件。
func saveFile(name string, body io.Reader) (chat.File, error) {
	root, err := filesDir()
	if err != nil {
		return chat.File{}, err
	}
	day := journal.Today()
	if err := os.MkdirAll(filepath.Join(root, day), 0o700); err != nil {
		return chat.File{}, err
	}
	b := make([]byte, 4)
	rand.Read(b)
	name = cleanName(name)
	id := day + "/" + hex.EncodeToString(b) + "-" + name
	path := filepath.Join(root, filepath.FromSlash(id))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return chat.File{}, err
	}
	n, err := io.Copy(f, io.LimitReader(body, maxFile+1))
	f.Close()
	if err == nil && n > maxFile {
		err = fmt.Errorf("文件超过 %d MB", maxFile>>20)
	}
	if err == nil && n == 0 {
		// 窗口里粘贴或拖进来的文件，WebKit 有时只给文件名不给内容
		err = errors.New("读到的内容是空的，没有存下。从访达把这个文件拖进来再试")
	}
	if err != nil {
		os.Remove(path)
		return chat.File{}, err
	}
	return fileInfo(root, id)
}

// copyFile 把本机路径上的文件复制进附件目录。窗口里拖进来的文件走这里：原生层给的是路径。
func copyFile(path string) (chat.File, error) {
	st, err := os.Stat(path)
	if err != nil {
		return chat.File{}, err
	}
	if st.IsDir() {
		return chat.File{}, errors.New("是文件夹：只能加文件，把文件夹里的文件拖进来")
	}
	f, err := os.Open(path)
	if err != nil {
		return chat.File{}, err
	}
	defer f.Close()
	return saveFile(filepath.Base(path), f)
}

// fileInfo 按 id 找到附件目录里的文件。id 不在附件目录里、或文件不存在时报错。
func fileInfo(root, id string) (chat.File, error) {
	rel := filepath.FromSlash(id)
	if !filepath.IsLocal(rel) {
		return chat.File{}, errors.New("附件不存在：" + id)
	}
	path := filepath.Join(root, rel)
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return chat.File{}, errors.New("附件不存在：" + id)
	}
	name := filepath.Base(rel)
	if i := strings.IndexByte(name, '-'); i >= 0 {
		name = name[i+1:]
	}
	return chat.File{ID: id, Name: name, Path: path, Size: st.Size(), Image: imageTypes[sniff(path)]}, nil
}

// sniff 按文件内容判断类型，不看扩展名。
func sniff(path string) string {
	head := make([]byte, 512)
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	n, _ := f.Read(head)
	return http.DetectContentType(head[:n])
}

// attachments 把发消息时带的 id 换成附件。
func attachments(ids []string) ([]chat.File, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	root, err := filesDir()
	if err != nil {
		return nil, err
	}
	out := make([]chat.File, 0, len(ids))
	for _, id := range ids {
		f, err := fileInfo(root, id)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func fileRoutes(mux *http.ServeMux) {
	// 上传：请求体是文件内容，文件名放在 ?name= 里。
	mux.HandleFunc("POST /api/files", func(w http.ResponseWriter, r *http.Request) {
		f, err := saveFile(r.URL.Query().Get("name"), r.Body)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		reply(w, f)
	})
	// 取附件：对话里的缩略图用它。只读附件目录里的文件。
	mux.HandleFunc("GET /api/files/{day}/{name}", func(w http.ResponseWriter, r *http.Request) {
		root, err := filesDir()
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		f, err := fileInfo(root, r.PathValue("day")+"/"+r.PathValue("name"))
		if err != nil {
			fail(w, http.StatusNotFound, err.Error())
			return
		}
		// 类型按内容定，不按扩展名：改名成 .html 的文件不能当页面在 morula 里打开
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if f.Image {
			w.Header().Set("Content-Type", sniff(f.Path))
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "attachment")
		}
		http.ServeFile(w, r, f.Path)
	})
}
