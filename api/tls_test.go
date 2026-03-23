package api

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureTLSFilesGeneratesPEM(t *testing.T) {
	tempDir := t.TempDir()
	certFile := filepath.Join(tempDir, "server-cert.pem")
	keyFile := filepath.Join(tempDir, "server-key.pem")

	if err := ensureTLSFiles(certFile, keyFile, []string{"localhost", "127.0.0.1", "api.example.com", "203.0.113.10"}); err != nil {
		t.Fatalf("ensureTLSFiles error = %v", err)
	}

	certData, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read cert file error = %v", err)
	}
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read key file error = %v", err)
	}

	if len(certData) == 0 || string(certData[:27]) != "-----BEGIN CERTIFICATE-----" {
		t.Fatalf("unexpected cert pem content: %q", string(certData))
	}
	if len(keyData) == 0 || string(keyData[:31]) != "-----BEGIN RSA PRIVATE KEY-----" {
		t.Fatalf("unexpected key pem content: %q", string(keyData))
	}

	block, _ := pem.Decode(certData)
	if block == nil {
		t.Fatal("failed to decode cert pem")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert error = %v", err)
	}
	if !containsString(cert.DNSNames, "api.example.com") {
		t.Fatalf("expected SAN DNSNames to contain api.example.com, got %v", cert.DNSNames)
	}
	foundIP := false
	for _, ip := range cert.IPAddresses {
		if ip.String() == "203.0.113.10" {
			foundIP = true
			break
		}
	}
	if !foundIP {
		t.Fatalf("expected SAN IPAddresses to contain 203.0.113.10, got %v", cert.IPAddresses)
	}
}
