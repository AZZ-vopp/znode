package node

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateSelfSignedCertificateWritesMatchingRSAFiles(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "nested", "node.cer")
	keyPath := filepath.Join(dir, "nested", "node.key")
	if err := generateSelfSslCertificate("node.example.com", certPath, keyPath); err != nil {
		t.Fatalf("generate certificate: %v", err)
	}

	certData, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read certificate: %v", err)
	}
	certBlock, _ := pem.Decode(certData)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" {
		t.Fatalf("unexpected certificate PEM block")
	}
	certificate, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	if err := certificate.VerifyHostname("node.example.com"); err != nil {
		t.Fatalf("verify certificate hostname: %v", err)
	}

	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read private key: %v", err)
	}
	keyBlock, _ := pem.Decode(keyData)
	if keyBlock == nil || keyBlock.Type != "RSA PRIVATE KEY" {
		t.Fatalf("unexpected private key PEM type: %v", keyBlock)
	}
	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("parse private key: %v", err)
	}
}
