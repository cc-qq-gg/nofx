package utils

import (
	"net/http"
	"net/url"
	"os"
	"time"
)

// CreateHTTPClient 创建支持代理的HTTP客户端
func CreateHTTPClient(timeout int) *http.Client {
	client := &http.Client{Timeout: time.Duration(timeout) * time.Second}

	// 检查环境变量中的代理设置
	if proxyURL := getProxyURL(); proxyURL != nil {
		client.Transport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
	}

	return client
}

// CreateHTTPClientWithTransport 创建支持代理的HTTP客户端，支持自定义Transport
func CreateHTTPClientWithTransport(timeout int, transport *http.Transport) *http.Client {
	client := &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: transport,
	}

	// 如果Transport没有设置代理，则自动设置
	if transport != nil && transport.Proxy == nil {
		if proxyURL := getProxyURL(); proxyURL != nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	return client
}

// getProxyURL 从环境变量获取代理URL
func getProxyURL() *url.URL {
	// 优先检查 HTTPS_PROXY
	if proxy := os.Getenv("HTTPS_PROXY"); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			return u
		}
	}

	// 其次检查 HTTP_PROXY
	if proxy := os.Getenv("HTTP_PROXY"); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			return u
		}
	}

	// 检查小写版本
	if proxy := os.Getenv("https_proxy"); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			return u
		}
	}

	if proxy := os.Getenv("http_proxy"); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			return u
		}
	}

	return nil
}