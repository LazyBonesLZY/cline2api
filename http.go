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
	// 拨号由 clineDialContext 负责（出口代理池 + 内置 net.Dialer 超时），
	// 不要在这里再设 DialContext：Go 结构体字面量不允许重复字段。
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	// 响应头限时：非流式请求在生成完成前不返回响应头，等价于旧的兜底超时；
	// 流式响应的响应头几乎立即到达，不限制流的后续读取时长。
	ResponseHeaderTimeout: 5 * time.Minute,
}

var httpClient = &http.Client{
	Transport: httpTransport,
	// 注意：绝不能设置 Client.Timeout —— Go 的 Client.Timeout 包含整个响应体读取时间，
	// 会把超过 5 分钟的流式响应拦腰截断（表现为「流式中途断掉」）。
	// 超时改为 transport 级（拨号/TLS/响应头限时，body 读取不限时），
	// 并按请求种类在 context 上再加一层：流式只限响应头等待，见 doHTTP。
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
