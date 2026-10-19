//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jchv/go-webview2"
)

// desktop (Windows) — собственное окно WebView2. Чистый Go (без CGO):
// lib вшит через go-winloader (WebView2Loader.dll), нужен только рантайм
// WebView2 (встроен в Windows 10 1803+). Нет рантайма → браузер.
func desktop(url string, debug bool, logf func(string, ...interface{})) {
	dataPath := ""
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		dataPath = filepath.Join(dir, "WEDRA", "webview2")
	}

	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:    debug,
		DataPath: dataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  "Nomos",
			Width:  1280,
			Height: 860,
			Center: true,
		},
	})
	if wv == nil {
		// в лог-файл — без кода входа; ссылка с кодом — только в консоль и браузер
		logf("WebView2 не запустился (нет рантайма?) — открываю системный браузер (%s)", strings.SplitN(url, "?", 2)[0])
		fmt.Println("  ссылка с кодом входа (только для вас):", url)
		logf("рантайм: https://developer.microsoft.com/microsoft-edge/webview2/")
		openBrowser(url)
		waitSignal()
		return
	}
	wv.Navigate(url)
	wv.Run() // блокирует до закрытия окна
	wv.Destroy()
}
