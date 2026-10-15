//go:build desktop

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

const defaultDesktopPort = 3457

func main() {
	selfCheck := flag.Bool("selfcheck", false, "Start embedded proxy, wait for /health, and exit")
	port := flag.Int("port", configuredDesktopPort(), "Proxy server port")
	host := flag.String("host", configuredDesktopHost(), "Proxy server listen host (0.0.0.0 = all interfaces)")
	flag.Parse()

	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- startProxy(*host, *port)
	}()

	if err := waitForEmbeddedProxy(*port, 15*time.Second, proxyErr); err != nil {
		log.Fatal(err)
	}

	if *selfCheck {
		fmt.Printf("selfcheck ok: embedded proxy serving http://127.0.0.1:%d/health\n", *port)
		return
	}

	if !isLoopbackHost(*host) {
		log.Printf("警告: 监听 %s 非本机回环地址，桌面窗口可能无法自动连接，建议使用 127.0.0.1 或 0.0.0.0", *host)
	}

	if err := runDesktopWindow(*port); err != nil {
		log.Fatal(err)
	}
}

func configuredDesktopPort() int {
	if value, err := strconv.Atoi(os.Getenv("CLINE_PROXY_PORT")); err == nil && value > 0 && value < 65536 {
		return value
	}
	return defaultDesktopPort
}

func configuredDesktopHost() string {
	if value := os.Getenv("CLINE_PROXY_HOST"); value != "" {
		return value
	}
	if value := loadPool().ListenHost; value != "" {
		return value
	}
	return "127.0.0.1"
}

func waitForEmbeddedProxy(port int, timeout time.Duration, proxyErr <-chan error) error {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-proxyErr:
			return fmt.Errorf("代理启动失败：%w", err)
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health returned status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("代理启动超时（%s）：%v", url, lastErr)
}

func runDesktopWindow(port int) error {
	return wails.Run(&options.App{
		Title:     "Cline Go Proxy",
		Width:     1280,
		Height:    820,
		MinWidth:  960,
		MinHeight: 620,
		AssetServer: &assetserver.Options{
			Handler: desktopRedirectHandler(port),
		},
		BackgroundColour: &options.RGBA{R: 248, G: 250, B: 252, A: 255},
		OnStartup:        func(_ context.Context) {},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
			DisableWindowIcon:    false,
		},
	})
}

func desktopRedirectHandler(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Cline Go Proxy</title>
  <style>
    :root { color-scheme: light; font-family: system-ui, -apple-system, "Segoe UI", sans-serif; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #f8fafc; color: #334155; }
    main { width: min(420px, calc(100vw - 48px)); text-align: center; }
    h1 { margin: 0 0 12px; font-size: 22px; color: #0f172a; }
    p { margin: 0; line-height: 1.6; font-size: 14px; }
  </style>
</head>
<body>
  <main>
    <h1>Cline Go Proxy</h1>
    <p>正在打开管理后台...</p>
  </main>
  <script>location.replace(%q);</script>
</body>
</html>`, fmt.Sprintf("http://127.0.0.1:%d/admin/", port))
	})
}
