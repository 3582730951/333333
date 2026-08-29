package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/supervisor"
)

// Proxy 是本地 MITM 代理核心
type Proxy struct {
	listenAddr string
	poolURL    string
	clientID   string
	cache      *IdentityCache
	caMgr      *CAManager
	poolClient *http.Client
	mu         sync.RWMutex
}

// NewProxy 创建代理实例
func NewProxy(cfg Config) (*Proxy, error) {
	caMgr, err := NewCAManager(cfg.MITM.CACert, cfg.MITM.CAKey)
	if err != nil {
		return nil, fmt.Errorf("init MITM CA: %w", err)
	}
	poolClient := newGatewayPoolClient()
	return &Proxy{
		listenAddr: cfg.ListenAddr,
		poolURL:    cfg.PoolServerURL,
		clientID:   cfg.ClientInstanceID,
		cache:      NewIdentityCache(cfg.PoolServerURL, cfg.DownstreamKey, cfg.IdentityTTL, poolClient),
		caMgr:      caMgr,
		poolClient: poolClient,
	}, nil
}

func newGatewayPoolClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // pool_server 可能用自签名证书
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 64,
			WriteBufferSize:     64 << 10,
			ReadBufferSize:      64 << 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// ListenAndServe 启动代理服务器
func (p *Proxy) ListenAndServe() error {
	ln, err := net.Listen("tcp", p.listenAddr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	defer ln.Close()

	log.Printf("Claude Gateway listening on %s", p.listenAddr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept error: %v", err)
			continue
		}
		go func() {
			defer supervisor.Recover("gateway-connection")
			p.handleConnection(conn)
		}()
	}
}

// handleConnection 处理单个连接
func (p *Proxy) handleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	httpReq, err := http.ReadRequest(bufio.NewReader(clientConn))
	if err != nil {
		return
	}

	// CONNECT 方法 = HTTPS MITM
	if httpReq.Method == "CONNECT" {
		target := httpReq.Host
		if target == "" && httpReq.URL != nil {
			target = httpReq.URL.Host
		}
		p.handleConnect(clientConn, target)
		return
	}

	p.handleHTTP(clientConn, httpReq)
}

// handleConnect 处理 HTTPS CONNECT 隧道
func (p *Proxy) handleConnect(clientConn net.Conn, target string) {
	// 解析目标主机
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
	}

	decision := classifyGatewayTarget(host, p.poolURL, p.gatewayPolicy())
	switch decision.Action {
	case gatewayTargetBlock:
		logBlockedGatewayTarget(host, decision.Reason)
		writeGatewayError(clientConn, http.StatusForbidden, "Blocked by Claude Gateway strict policy")
		return
	case gatewayTargetForward:
		p.forwardConnect(clientConn, target)
		return
	case gatewayTargetIntercept:
		// Continue into MITM handling below.
	default:
		writeGatewayError(clientConn, http.StatusForbidden, "Blocked by Claude Gateway strict policy")
		return
	}

	// 响应 200 Connection Established
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// 生成临时证书
	cert, err := p.caMgr.GenerateCert(host)
	if err != nil {
		log.Printf("cert generation failed: %v", err)
		return
	}

	// TLS 握手
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
	}
	tlsConn := tls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		log.Printf("TLS handshake failed: %v", err)
		return
	}
	defer tlsConn.Close()

	// 读取解密后的 HTTP 请求
	httpReq, err := http.ReadRequest(bufio.NewReader(tlsConn))
	if err != nil {
		log.Printf("read HTTP request failed: %v", err)
		return
	}

	// 改写请求
	if err := p.rewriteRequest(httpReq); err != nil {
		log.Printf("rewrite failed: %v", err)
		writeGatewayError(tlsConn, http.StatusInternalServerError, "Rewrite failed")
		return
	}

	// 转发到 pool_server
	p.forwardToPool(tlsConn, httpReq)
}

// forwardConnect 直接转发 CONNECT（不拦截）
func (p *Proxy) forwardConnect(clientConn net.Conn, target string) {
	targetConn, err := net.Dial("tcp", target)
	if err != nil {
		writeGatewayError(clientConn, http.StatusBadGateway, "Upstream unavailable")
		return
	}
	defer targetConn.Close()

	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// 双向转发
	go func() {
		defer supervisor.Recover("gateway-copy-upstream")
		_, _ = io.Copy(targetConn, clientConn)
	}()
	io.Copy(clientConn, targetConn)
}

// handleHTTP 处理普通 HTTP。它只允许转发到配置的 pool host；其他目标仍按
// strict gateway policy 拦截，避免 HTTP_PROXY 被滥用成开放代理。
func (p *Proxy) handleHTTP(clientConn io.Writer, req *http.Request) {
	if !requestTargetsPool(req, p.poolURL) {
		writeGatewayError(clientConn, http.StatusForbidden, "Blocked by Claude Gateway strict policy")
		return
	}
	p.forwardToPool(clientConn, req)
}

func requestTargetsPool(req *http.Request, poolURL string) bool {
	if req == nil {
		return false
	}
	target := req.Host
	if req.URL != nil && req.URL.Host != "" {
		target = req.URL.Host
	}
	pool := strings.TrimSpace(poolURL)
	if pool == "" {
		return false
	}
	poolHost := pool
	if strings.Contains(pool, "://") {
		if u, err := http.NewRequest(http.MethodGet, pool, nil); err == nil && u.URL != nil {
			poolHost = u.URL.Host
		}
	}
	return sameGatewayHostPort(target, poolHost)
}

func sameGatewayHostPort(target, pool string) bool {
	targetHost := normalizeTargetHost(target)
	poolHost := normalizeTargetHost(pool)
	if targetHost == "" || poolHost == "" || targetHost != poolHost {
		return false
	}
	targetPort := gatewayHostPort(target)
	poolPort := gatewayHostPort(pool)
	return poolPort == "" || targetPort == poolPort
}

func gatewayHostPort(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" {
		return ""
	}
	if _, port, err := net.SplitHostPort(hostport); err == nil {
		return port
	}
	if strings.Count(hostport, ":") == 1 {
		parts := strings.SplitN(hostport, ":", 2)
		return parts[1]
	}
	return ""
}

// forwardToPool 转发到 pool_server
func (p *Proxy) forwardToPool(clientConn io.Writer, req *http.Request) {
	// 构造目标 URL
	targetURL := strings.TrimRight(p.poolURL, "/") + req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		targetURL += "?" + req.URL.RawQuery
	}

	// 创建新请求
	proxyReq, err := http.NewRequest(req.Method, targetURL, req.Body)
	if err != nil {
		log.Printf("create proxy request failed: %v", err)
		return
	}
	proxyReq.ContentLength = req.ContentLength

	// 复制请求头
	for k, v := range req.Header {
		proxyReq.Header[k] = v
	}
	// 添加网关模式标记
	proxyReq.Header.Set("X-Gateway-Mode", "local")
	// Overwrite rather than forward a caller-selected value: one gateway install
	// is one durable client namespace even when many Claude Code processes share it.
	proxyReq.Header.Set("X-Pool-Client-ID", p.clientID)

	resp, err := p.poolClient.Do(proxyReq)
	if err != nil {
		log.Printf("forward to pool failed: %v", err)
		writeGatewayError(clientConn, http.StatusBadGateway, "Pool unavailable")
		return
	}
	defer resp.Body.Close()

	// 写回响应
	w := newTLSResponseWriter(clientConn)
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func writeGatewayError(dst io.Writer, statusCode int, message string) {
	body := strings.TrimSpace(message) + "\n"
	w := newTLSResponseWriter(dst)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(statusCode)
	_, _ = io.WriteString(w, body)
}

// tlsResponseWriter 包装连接为 http.ResponseWriter
type tlsResponseWriter struct {
	conn        io.Writer
	header      http.Header
	wroteHeader bool
}

func newTLSResponseWriter(conn io.Writer) *tlsResponseWriter {
	return &tlsResponseWriter{
		conn:   conn,
		header: make(http.Header),
	}
}

func (w *tlsResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *tlsResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.conn.Write(data)
}

func (w *tlsResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	reason := http.StatusText(statusCode)
	if reason == "" {
		reason = "status"
	}
	_, _ = fmt.Fprintf(w.conn, "HTTP/1.1 %d %s\r\n", statusCode, reason)
	_ = w.Header().Write(w.conn)
	_, _ = io.WriteString(w.conn, "\r\n")
}
