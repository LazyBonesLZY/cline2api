package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

var execCommand = exec.Command

// 全局出站 transport：经 cline_proxy.go 的钩子支持应用内出口代理池
// （Cline 对话/认证/模型同步与复用此 transport 的自定义 Provider 共同生效）。
var httpTransport = &http.Transport{
	Proxy:               clineOutboundProxy,
	DialContext:         clineDialContext,
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 10,
	IdleConnTimeout:     90 * time.Second,
	DisableCompression:  false,
}

var httpClient = &http.Client{
	Transport: httpTransport,
	// 不设 Client.Timeout：它会把读完整个 body 也算进去，长流式输出会被 5 分钟掐断。
	// 时限按请求种类加在 context 上，见 doHTTP。
}

const (
	upstreamHeaderTimeout = 60 * time.Second
	upstreamTotalTimeout  = 5 * time.Minute
	authRequestTimeout    = 60 * time.Second
)

// doHTTP 发出请求。stream 为真时只限制响应头等待，body 读到调用方关闭为止；
// 非流式则用 total 覆盖响应头和 body。
func doHTTP(client *http.Client, req *http.Request, stream bool, total, headerWait time.Duration) (*http.Response, error) {
	if client == nil {
		client = httpClient
	}
	if stream {
		if headerWait <= 0 {
			headerWait = upstreamHeaderTimeout
		}
		ctx, cancel := context.WithCancel(req.Context())
		timer := time.AfterFunc(headerWait, cancel)
		resp, err := client.Do(req.WithContext(ctx))
		if !timer.Stop() {
			cancel()
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			if err == nil {
				err = context.DeadlineExceeded
			}
			return nil, err
		}
		if err != nil {
			cancel()
			return nil, err
		}
		return withCancelOnClose(resp, cancel), nil
	}
	if total <= 0 {
		total = upstreamTotalTimeout
	}
	ctx, cancel := context.WithTimeout(req.Context(), total)
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	return withCancelOnClose(resp, cancel), nil
}

func doUpstream(req *http.Request, stream bool) (*http.Response, error) {
	return doHTTP(httpClient, req, stream, upstreamTotalTimeout, upstreamHeaderTimeout)
}

func httpPostForm(rawURL string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doHTTP(httpClient, req, false, authRequestTimeout, authRequestTimeout)
}

func httpPostJSON(rawURL string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", rawURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return doHTTP(httpClient, req, false, authRequestTimeout, authRequestTimeout)
}

func readBody(resp *http.Response) string {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("<read error: %v>", err)
	}
	return string(data)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Start()
}
