package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CAManager 管理自签名 CA 和动态证书生成
type CAManager struct {
	caCertPath string
	caKeyPath  string
	caCert     *x509.Certificate
	caKey      *rsa.PrivateKey
	certCache  sync.Map // host -> *tls.Certificate
}

// NewCAManager 创建 CA 管理器
func NewCAManager(certPath, keyPath string) (*CAManager, error) {
	mgr := &CAManager{
		caCertPath: certPath,
		caKeyPath:  keyPath,
	}
	// 加载或生成 CA
	if err := mgr.loadOrGenerateCA(); err != nil {
		return nil, fmt.Errorf("CA init failed: %w", err)
	}
	return mgr, nil
}

// loadOrGenerateCA 加载或生成 CA 证书
func (m *CAManager) loadOrGenerateCA() error {
	// 尝试加载现有 CA
	if m.loadCA() == nil {
		return nil
	}

	// 生成新 CA
	return m.generateCA()
}

// loadCA 加载现有 CA
func (m *CAManager) loadCA() error {
	certPEM, err := os.ReadFile(m.caCertPath)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(m.caKeyPath)
	if err != nil {
		return err
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return fmt.Errorf("invalid cert PEM")
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return fmt.Errorf("invalid key PEM")
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return err
	}

	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return err
	}

	m.caCert = cert
	m.caKey = key
	return nil
}

// generateCA 生成新的 CA 证书
func (m *CAManager) generateCA() error {
	// 生成私钥
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	// 生成证书
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Claude Gateway CA"},
			CommonName:   "Claude Gateway Root CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour), // 10 years
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return err
	}

	// 保存到文件
	caDir := filepath.Dir(m.caCertPath)
	if err := os.MkdirAll(caDir, gatewayPrivateDirMode); err != nil {
		return err
	}
	if err := chmodGatewayPrivateDir(caDir); err != nil {
		return err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(m.caCertPath, certPEM, gatewayPublicCertMode); err != nil {
		return err
	}
	if err := os.Chmod(m.caCertPath, gatewayPublicCertMode); err != nil {
		return err
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(m.caKeyPath, keyPEM, gatewayPrivateFileMode); err != nil {
		return err
	}
	if err := os.Chmod(m.caKeyPath, gatewayPrivateFileMode); err != nil {
		return err
	}

	m.caCert = cert
	m.caKey = key
	return nil
}

// GenerateCert 为指定主机生成证书
func (m *CAManager) GenerateCert(host string) (*tls.Certificate, error) {
	// 检查缓存
	if cached, ok := m.certCache.Load(host); ok {
		return cached.(*tls.Certificate), nil
	}

	// 生成新证书
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	serialNumber, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Claude Gateway"},
			CommonName:   host,
		},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(365 * 24 * time.Hour), // 1 year
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{host},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, err
	}

	cert := &tls.Certificate{
		Certificate: [][]byte{certDER, m.caCert.Raw},
		PrivateKey:  key,
	}

	// 缓存
	m.certCache.Store(host, cert)

	return cert, nil
}
