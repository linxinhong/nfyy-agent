package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/linxinhong/xingding-agent-runtime/pkg/mcp"
)

// 测试中使用的证书主体名，便于断言服务器端实际观察到的客户端身份。
const (
	mtlsServerCN  = "xingding-mtls-server"
	mtlsClientACN = "xingding-mtls-client-a"
	mtlsClientBCN = "xingding-mtls-client-b"
)

// testPKI 是一套完全用标准库在内存中生成的测试 PKI：一个自签名 CA、
// 一个服务器叶子证书，以及两个可互换的客户端叶子证书。
type testPKI struct {
	caPool      *x509.CertPool
	serverCert  tls.Certificate
	clientCertA tls.Certificate
	clientCertB tls.Certificate
}

// newTestPKI 生成测试 CA 与叶子证书。证书有效期覆盖当前测试运行窗口，
// 全程不依赖任何第三方库或磁盘文件。
func newTestPKI(t *testing.T) *testPKI {
	t.Helper()

	// 自签名 CA：用于签发并校验服务器 / 客户端证书。
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "xingding-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return &testPKI{
		caPool:      pool,
		serverCert:  newTestLeafCert(t, caCert, caKey, 2, mtlsServerCN, true),
		clientCertA: newTestLeafCert(t, caCert, caKey, 3, mtlsClientACN, false),
		clientCertB: newTestLeafCert(t, caCert, caKey, 4, mtlsClientBCN, false),
	}
}

// newTestLeafCert 用 CA 签发一张叶子证书；isServer 决定其用途（服务端
// 或客户端认证）以及是否携带 127.0.0.1/localhost 的 SAN。
func newTestLeafCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, serial int64, cn string, isServer bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key %q: %v", cn, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		tmpl.DNSNames = []string{"localhost"}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert %q: %v", cn, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// mtlsPeerLog 记录服务器在每个请求上观察到的客户端证书主体名，用自身
// 互斥锁保证与请求处理 goroutine 的并发安全。
type mtlsPeerLog struct {
	mu       sync.Mutex
	subjects []string
}

func (l *mtlsPeerLog) record(cn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.subjects = append(l.subjects, cn)
}

func (l *mtlsPeerLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.subjects...)
}

// newMTLSServer 启动一个要求并校验客户端证书的 httptest 服务器，并返回
// 记录客户端身份的日志（按到达顺序）。
func newMTLSServer(t *testing.T, pki *testPKI) (*httptest.Server, *mtlsPeerLog) {
	t.Helper()
	peerLog := &mtlsPeerLog{}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			peerLog.record(r.TLS.PeerCertificates[0].Subject.CommonName)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		method, _ := req["method"].(string)

		// 通知走 202 空响应；initialize / ping 返回合法 JSON-RPC result。
		if method != "initialize" && method != "ping" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]any{
				"protocolVersion": mcp.ProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mtls", "version": "1.0"},
			},
		})
	}))

	// 必须在 StartTLS 之前设置 ClientAuth/ClientCAs；StartTLS 会 Clone 已有
	// 配置并补齐默认证书。这里显式提供由测试 CA 签发的服务器证书，客户端
	// 只信任该 CA，从而验证双向握手。
	srv.TLS = &tls.Config{
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pki.caPool,
		Certificates: []tls.Certificate{pki.serverCert},
	}
	srv.StartTLS()
	return srv, peerLog
}

// mtlsHTTPClient 构造一个信任测试 CA、并携带给定客户端证书的 *http.Client，
// 用于通过 HTTPTransportOptions.HTTPClient 注入 transport。
func mtlsHTTPClient(pki *testPKI, cert tls.Certificate) *http.Client {
	tlsCfg := &tls.Config{
		RootCAs:      pki.caPool,
		Certificates: []tls.Certificate{cert},
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}
}

// TestHTTPTransportMutualTLSHandshake 验证注入的 HTTPClient 携带客户端证书
// 完成双向 mTLS 握手，并走通一次真实的 JSON-RPC Initialize 往返。
func TestHTTPTransportMutualTLSHandshake(t *testing.T) {
	pki := newTestPKI(t)
	srv, peerLog := newMTLSServer(t, pki)
	defer srv.Close()

	transport, err := NewHTTPClientTransportWithOptions(srv.URL, HTTPTransportOptions{
		HTTPClient: mtlsHTTPClient(pki, pki.clientCertA),
	})
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	client := NewClient(transport)
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// Initialize 会先后发出 initialize 请求与 notifications/initialized 通知，
	// 两次请求都应通过注入的 client 走同一 mTLS 连接并携带客户端证书。
	observed := peerLog.snapshot()
	if len(observed) < 2 {
		t.Fatalf("server observed %d peer certs, want >= 2 (initialize + initialized notification)", len(observed))
	}
	for i, cn := range observed {
		if cn != mtlsClientACN {
			t.Fatalf("request %d peer CN = %q, want %q", i, cn, mtlsClientACN)
		}
	}
}

// TestHTTPTransportMutualTLSRejectsMissingClientCert 验证同一 mTLS 服务器会
// 拒绝未携带客户端证书的连接：握手失败，Call 必须返回非 nil 错误。
func TestHTTPTransportMutualTLSRejectsMissingClientCert(t *testing.T) {
	pki := newTestPKI(t)
	srv, _ := newMTLSServer(t, pki)
	defer srv.Close()

	// 只信任 CA、不提供任何客户端证书。
	noCert := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pki.caPool},
	}}
	transport, err := NewHTTPClientTransportWithOptions(srv.URL, HTTPTransportOptions{HTTPClient: noCert})
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	if _, err := transport.Call(context.Background(), mcp.Request{JSONRPC: "2.0", ID: "1", Method: "ping"}); err == nil {
		t.Fatalf("expected TLS handshake failure without client certificate, got nil error")
	}
}

// TestHTTPTransportMutualTLSCertificateRotation 验证证书可轮换：同一 mTLS
// 服务器先后接受两张不同的客户端证书完成握手，且服务器观察到不同身份，
// 证明 transport 每次握手走的是注入的 client 而非固定连接。
func TestHTTPTransportMutualTLSCertificateRotation(t *testing.T) {
	pki := newTestPKI(t)
	srv, peerLog := newMTLSServer(t, pki)
	defer srv.Close()

	// 第一次握手使用 client-a 证书。
	transportA, err := NewHTTPClientTransportWithOptions(srv.URL, HTTPTransportOptions{
		HTTPClient: mtlsHTTPClient(pki, pki.clientCertA),
	})
	if err != nil {
		t.Fatalf("new transport A: %v", err)
	}
	if err := NewClient(transportA).Initialize(context.Background()); err != nil {
		t.Fatalf("initialize with client-a: %v", err)
	}

	// 换用 client-b 证书再次握手。
	transportB, err := NewHTTPClientTransportWithOptions(srv.URL, HTTPTransportOptions{
		HTTPClient: mtlsHTTPClient(pki, pki.clientCertB),
	})
	if err != nil {
		t.Fatalf("new transport B: %v", err)
	}
	if err := NewClient(transportB).Initialize(context.Background()); err != nil {
		t.Fatalf("initialize with client-b: %v", err)
	}

	// 服务器应先后观察到两张不同客户端证书。
	hasA, hasB := false, false
	for _, cn := range peerLog.snapshot() {
		switch cn {
		case mtlsClientACN:
			hasA = true
		case mtlsClientBCN:
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Fatalf("server observed client CNs = %v, want both %q and %q", peerLog.snapshot(), mtlsClientACN, mtlsClientBCN)
	}
}
