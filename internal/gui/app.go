// Package gui 是 morula 的桌面窗口：Wails v3 承载一个 WebKit 窗口，
// 页面和 JSON 接口都由同一个 http.Handler 提供。
package gui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed assets
var assets embed.FS

var app *application.App

// Handler 提供嵌入的页面和 /api/* 接口。
func Handler(version string) (http.Handler, error) {
	if err := setup(); err != nil {
		return nil, err
	}
	sub, _ := fs.Sub(assets, "assets")
	mux := http.NewServeMux()
	routes(mux, version)
	// BundledAssetFileServer 另外提供 /wails/runtime.js：窗口拖动、双击缩放靠它处理 --wails-draggable
	mux.Handle("/", application.BundledAssetFileServer(sub))
	return mux, nil
}

// Serve 在浏览器里提供同一套界面，用于调试和截图（无窗口，不能自更新退出）。
func Serve(version, addr string) error {
	h, err := Handler(version)
	if err != nil {
		return err
	}
	log.Printf("morula web: http://%s", addr)
	return http.ListenAndServe(addr, h)
}

// Run 打开主窗口，直到用户退出。
func Run(version string) error {
	h, err := Handler(version)
	if err != nil {
		return err
	}
	defer eng.StopAll()
	defer sess.Close()
	app = application.New(application.Options{
		Name:        "morula",
		Description: "一个人的 agent 团队",
		Assets:      application.AssetOptions{Handler: h},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
		ErrorHandler: func(err error) { log.Println("morula:", err) },
	})
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "morula",
		URL:              "/",
		Width:            1180,
		Height:           760,
		MinWidth:         860,
		MinHeight:        540,
		BackgroundColour: application.NewRGB(0x1a, 0x1a, 0x1e),
		Mac: application.MacWindow{
			// 红绿灯在窗口左上角 x 9–69、y 9–23（AppKit 实测）。侧栏宽 78px，红绿灯左右各留 9px，见 app.css 的 --rail。
			// HiddenInset 的红绿灯在 x 19–79，侧栏要 98px 才能居中，太宽，不用。
			TitleBar: application.MacTitleBarHidden,
		},
		// 从访达拖进窗口的文件，WebKit 交给页面的有时是空内容（实测 16 KB 的 html 存成 0 字节）。
		// 开原生拖放拿到路径，由这里复制，再交给页面的 morulaDropped。
		EnableFileDrop: true,
	})
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		var out []map[string]any
		for _, p := range e.Context().DroppedFiles() {
			f, err := copyFile(p)
			if err != nil {
				out = append(out, map[string]any{"name": cleanName(p), "err": err.Error()})
				continue
			}
			out = append(out, map[string]any{"id": f.ID, "name": f.Name, "image": f.Image})
		}
		b, _ := json.Marshal(out)
		win.ExecJS("window.morulaDropped(" + string(b) + ")")
	})
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) { startNotify(win) })
	return app.Run()
}
