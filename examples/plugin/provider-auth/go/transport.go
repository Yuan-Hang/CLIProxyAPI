package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const directProxyURL = "direct"

type pluginHTTPClients struct {
	mu         sync.Mutex
	transports map[string]*http.Transport
	clients    map[string]*http.Client
}

var upstreamHTTPClients = &pluginHTTPClients{
	transports: make(map[string]*http.Transport),
	clients:    make(map[string]*http.Client),
}

func (c *pluginHTTPClients) reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	for _, transport := range c.transports {
		transport.CloseIdleConnections()
	}
	c.transports = make(map[string]*http.Transport)
	c.clients = make(map[string]*http.Client)
	c.mu.Unlock()
}

func (c *pluginHTTPClients) client(proxyURL string) (*http.Client, error) {
	proxyURL, proxyFunc, errProxy := normalizeProxy(proxyURL)
	if errProxy != nil {
		return nil, errProxy
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if client := c.clients[proxyURL]; client != nil {
		return client, nil
	}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	client := &http.Client{Transport: transport}
	c.transports[proxyURL] = transport
	c.clients[proxyURL] = client
	return client, nil
}

func normalizeProxy(raw string) (string, func(*http.Request) (*url.URL, error), error) {
	raw = strings.TrimSpace(raw)
	if strings.EqualFold(raw, directProxyURL) {
		return directProxyURL, nil, nil
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.Host == "" {
		return "", nil, fmt.Errorf("proxy_url must be %q or a valid proxy URL", directProxyURL)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", nil, fmt.Errorf("proxy_url scheme must be http, https, socks5, or socks5h")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	return parsed.String(), http.ProxyURL(parsed), nil
}

func authProxyURL(storageJSON []byte) (string, error) {
	record, handled, errRecord := decodeAuthRecord(storageJSON)
	if errRecord != nil {
		return "", errRecord
	}
	if !handled {
		return "", nil
	}
	return record.ProxyURL, nil
}

func doUpstreamHTTPRequest(ctx context.Context, callbackID string, storageJSON []byte, req hostHTTPRequest) (pluginapi.HTTPResponse, error) {
	proxyURL, errProxy := authProxyURL(storageJSON)
	if errProxy != nil {
		return pluginapi.HTTPResponse{}, errProxy
	}
	if proxyURL == "" {
		req.HostCallbackID = callbackID
		return doHostHTTPRequest(req)
	}
	resp, errDo := doPluginHTTPRequest(ctx, proxyURL, req)
	if errDo != nil {
		return pluginapi.HTTPResponse{}, errDo
	}
	defer resp.Body.Close()
	body, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("read upstream response: %w", errRead)
	}
	return pluginapi.HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header.Clone(),
		Body:       body,
	}, nil
}

func doPluginHTTPRequest(ctx context.Context, proxyURL string, req hostHTTPRequest) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	client, errClient := upstreamHTTPClients.client(proxyURL)
	if errClient != nil {
		return nil, errClient
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	httpReq, errRequest := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if errRequest != nil {
		return nil, fmt.Errorf("create upstream request: %w", errRequest)
	}
	httpReq.Header = req.Headers.Clone()
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return nil, fmt.Errorf("execute upstream request: %w", errDo)
	}
	return resp, nil
}
