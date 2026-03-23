package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

func ensureTLSFiles(certFile, keyFile string, hosts []string) error {
	if fileExists(certFile) && fileExists(keyFile) {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(certFile), 0755); err != nil {
		return fmt.Errorf("创建证书目录失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0755); err != nil {
		return fmt.Errorf("创建私钥目录失败: %w", err)
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("生成RSA私钥失败: %w", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return fmt.Errorf("生成证书序列号失败: %w", err)
	}

	hostname, _ := os.Hostname()
	dnsNames, ipAddresses := splitTLSHosts(hosts)
	if len(dnsNames) == 0 {
		dnsNames = []string{"localhost"}
	}
	if len(ipAddresses) == 0 {
		ipAddresses = []net.IP{
			net.ParseIP("127.0.0.1"),
			net.ParseIP("::1"),
		}
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"nofx"},
			CommonName:   "nofx-self-signed",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}
	if hostname != "" && hostname != "localhost" && !containsString(template.DNSNames, hostname) {
		template.DNSNames = append(template.DNSNames, hostname)
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("生成自签名证书失败: %w", err)
	}

	certOut, err := os.OpenFile(certFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("创建证书文件失败: %w", err)
	}
	defer certOut.Close()

	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("写入证书PEM失败: %w", err)
	}

	keyOut, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("创建私钥文件失败: %w", err)
	}
	defer keyOut.Close()

	privBytes := x509.MarshalPKCS1PrivateKey(priv)
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: privBytes}); err != nil {
		return fmt.Errorf("写入私钥PEM失败: %w", err)
	}

	return nil
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func splitTLSHosts(hosts []string) ([]string, []net.IP) {
	dnsNames := make([]string, 0, len(hosts))
	ipAddresses := make([]net.IP, 0, len(hosts))

	for _, host := range hosts {
		host = filepath.Clean(host)
		if host == "." || host == "" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			ipAddresses = append(ipAddresses, ip)
			continue
		}
		dnsNames = append(dnsNames, host)
	}

	return uniqueStrings(dnsNames), uniqueIPs(ipAddresses)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func uniqueIPs(values []net.IP) []net.IP {
	seen := make(map[string]bool, len(values))
	result := make([]net.IP, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		key := value.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, value)
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
