package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/sessionidentity"
)

const identityErrorBodyLimit = 64 * 1024
const gatewayRewriteBodyLimit = 64 << 20

// LocalEnvironment 本地真实环境
type LocalEnvironment struct {
	Username   string
	Hostname   string
	HomeDir    string
	WorkDir    string
	DNSServers []string
	EnvVars    map[string]string
}

// VirtualIdentity VPS 下发的虚拟身份
type VirtualIdentity struct {
	AccountID               string            `json:"account_id"`
	SessionID               string            `json:"session_id"`
	UserID                  string            `json:"user_id"`
	MachineID               string            `json:"machine_id"`
	OSName                  string            `json:"os_name"`
	OSVersion               string            `json:"os_version"`
	OSRelease               string            `json:"os_release"`
	Arch                    string            `json:"arch"`
	Terminal                string            `json:"terminal"`
	NodeVersion             string            `json:"node_version"`
	ClaudeCLIVersion        string            `json:"claude_cli_version"`
	StainlessPackageVersion string            `json:"stainless_package_version"`
	CodexCLIVersion         string            `json:"codex_cli_version"`
	Username                string            `json:"username"`
	Hostname                string            `json:"hostname"`
	HomeDir                 string            `json:"home_dir"`
	EnvVars                 map[string]string `json:"env_vars"`
	DNSServers              []string          `json:"dns_servers"`
	GatewayIP               string            `json:"gateway_ip"`
	LocalIP                 string            `json:"local_ip"`
	ProcessInfo             ProcessInfo       `json:"process_info"`
	GatewayPolicy           GatewayPolicy     `json:"gateway_policy"`
}

type ProcessInfo struct {
	PID       int      `json:"pid"`
	ParentPID int      `json:"parent_pid"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	CWD       string   `json:"cwd"`
}

// CachedIdentity 缓存的身份信息
type CachedIdentity struct {
	Virtual   *VirtualIdentity
	Local     *LocalEnvironment
	FetchedAt time.Time
}

// IdentityCache 身份缓存
type IdentityCache struct {
	poolURL       string
	downstreamKey string
	ttl           time.Duration
	client        *http.Client
	mu            sync.Mutex
	cache         map[string]*CachedIdentity // key: provider
	inflight      map[string]*identityFetch
}

type identityFetch struct {
	done   chan struct{}
	result *CachedIdentity
	err    error
}

// NewIdentityCache 创建身份缓存
func NewIdentityCache(poolURL, downstreamKey string, ttl time.Duration, client *http.Client) *IdentityCache {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &IdentityCache{
		poolURL:       poolURL,
		downstreamKey: downstreamKey,
		ttl:           ttl,
		client:        client,
		cache:         make(map[string]*CachedIdentity),
		inflight:      make(map[string]*identityFetch),
	}
}

// Get 获取虚拟身份（带缓存）
func (c *IdentityCache) Get(provider string) (*CachedIdentity, error) {
	// 检查缓存
	c.mu.Lock()
	if cached, ok := c.cache[provider]; ok {
		if time.Since(cached.FetchedAt) < c.ttl {
			c.mu.Unlock()
			return cached, nil
		}
	}
	if fetch := c.inflight[provider]; fetch != nil {
		c.mu.Unlock()
		<-fetch.done
		return fetch.result, fetch.err
	}
	fetch := &identityFetch{done: make(chan struct{})}
	c.inflight[provider] = fetch
	c.mu.Unlock()

	cached, err := c.fetchIdentity(provider)
	c.mu.Lock()
	if err == nil {
		c.cache[provider] = cached
	}
	fetch.result = cached
	fetch.err = err
	delete(c.inflight, provider)
	close(fetch.done)
	c.mu.Unlock()

	return cached, err
}

func (c *IdentityCache) fetchIdentity(provider string) (*CachedIdentity, error) {
	// 获取本地真实环境
	local := c.getLocalEnvironment()
	// 从 pool_server 获取虚拟身份
	virtual, err := c.fetchFromPool(provider)
	if err != nil {
		return nil, err
	}

	// 缓存
	return &CachedIdentity{
		Virtual:   virtual,
		Local:     local,
		FetchedAt: time.Now(),
	}, nil
}

// getLocalEnvironment 读取本地真实环境
func (c *IdentityCache) getLocalEnvironment() *LocalEnvironment {
	hostname, _ := os.Hostname()
	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("USERNAME") // Windows
	}
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = os.Getenv("USERPROFILE") // Windows
	}
	workDir, _ := os.Getwd()

	return &LocalEnvironment{
		Username:   username,
		Hostname:   hostname,
		HomeDir:    homeDir,
		WorkDir:    workDir,
		DNSServers: readLocalDNSServers(),
		EnvVars: map[string]string{
			"HOME":   homeDir,
			"USER":   username,
			"SHELL":  os.Getenv("SHELL"),
			"TERM":   os.Getenv("TERM"),
			"LANG":   os.Getenv("LANG"),
			"TMPDIR": os.Getenv("TMPDIR"),
		},
	}
}

func readLocalDNSServers() []string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var servers []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "nameserver" {
			servers = append(servers, fields[1])
		}
	}
	return servers
}

// fetchFromPool 从 pool_server 获取虚拟身份
func (c *IdentityCache) fetchFromPool(provider string) (*VirtualIdentity, error) {
	endpoint := strings.TrimRight(c.poolURL, "/") + "/v1/gateway/identity?provider=" + url.QueryEscape(provider)

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create identity request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.downstreamKey)
	req.Header.Set("X-Gateway-Mode", "local")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch identity failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, identityErrorBodyLimit))
		return nil, fmt.Errorf("pool returned %d: %s", resp.StatusCode, body)
	}

	var virtual VirtualIdentity
	if err := json.NewDecoder(resp.Body).Decode(&virtual); err != nil {
		return nil, fmt.Errorf("decode identity failed: %w", err)
	}

	return &virtual, nil
}

func readRewriteBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, gatewayRewriteBodyLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > gatewayRewriteBodyLimit {
		return nil, errors.New("gateway request body too large")
	}
	return body, nil
}

// rewriteRequest 改写请求体
func (p *Proxy) rewriteRequest(req *http.Request) error {
	if req.Method != "POST" || !shouldRewriteGatewayRequest(req.URL.Path) {
		return nil
	}

	// 检测 provider
	provider := "claude"
	if strings.Contains(req.Host, "chatgpt.com") || strings.Contains(req.URL.Path, "codex") {
		provider = "codex"
	}

	// 获取虚拟身份
	identity, err := p.cache.Get(provider)
	if err != nil {
		return fmt.Errorf("get identity failed: %w", err)
	}

	// 读取原始 body
	body, err := readRewriteBody(req.Body)
	if err != nil {
		return fmt.Errorf("read body failed: %w", err)
	}
	req.Body.Close()

	// 改写 body
	rewritten, poolSession, err := rewriteBodyForRequest(body, req.Header, identity)
	if err != nil {
		return fmt.Errorf("rewrite body failed: %w", err)
	}
	if poolSession != "" {
		// The value is already pseudonymized with the gateway identity seed. The
		// pool projects it again with the selected account's session seed.
		req.Header.Set(sessionidentity.PoolSessionHeader, poolSession)
	}

	// 替换 body
	req.Body = io.NopCloser(bytes.NewReader(rewritten))
	req.ContentLength = int64(len(rewritten))

	return nil
}
