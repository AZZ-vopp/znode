package node

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	panel "github.com/AZZ-vopp/znode/api/v2board"
)

func useTemporaryCertificateWriteRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	root = resolvedRoot
	oldRoots := certificateWriteRoots
	certificateWriteRoots = []string{root}
	t.Cleanup(func() { certificateWriteRoots = oldRoots })
	return root
}

func certificatePairPEM(t *testing.T, domain string) ([]byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "source.cer")
	keyPath := filepath.Join(dir, "source.key")
	if err := generateSelfSslCertificate(domain, certPath, keyPath); err != nil {
		t.Fatalf("generate source pair: %v", err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return certPEM, keyPEM
}

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

func TestCertificateGenerationRejectsPanelControlledPathsOutsideZNode(t *testing.T) {
	controller := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: &panel.CertInfo{
		CertMode: "self",
		CertFile: filepath.Join(t.TempDir(), "node.cer"),
		KeyFile:  filepath.Join(t.TempDir(), "node.key"),
	}}}}
	if err := controller.requestCert(); err == nil {
		t.Fatal("root certificate generation accepted a path outside /etc/znode")
	}
	if !pathWithinCertificateRoot("/etc/znode/nodes/node.cer", []string{"/etc/znode"}) {
		t.Fatal("valid ZNode certificate path was rejected")
	}
	if pathWithinCertificateRoot("/etc/znode-escape/node.cer", []string{"/etc/znode"}) {
		t.Fatal("prefix-confusable certificate path was accepted")
	}
	if err := validateCertificateMaterialPath("/etc/znode/config.json", false, true); err == nil {
		t.Fatal("certificate generation accepted a non-certificate target")
	}
}

func TestCertificateReaderRejectsOversizedMaterial(t *testing.T) {
	root := t.TempDir()
	oldRoots := certificateReadRoots
	certificateReadRoots = []string{root}
	t.Cleanup(func() { certificateReadRoots = oldRoots })

	path := filepath.Join(root, "oversized.pem")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxCertificateMaterialBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateCertificateMaterialPath(path, false, false); err == nil {
		t.Fatal("oversized certificate material was accepted")
	}
}

func TestRemoteCertificateBootstrapsFreshWritablePair(t *testing.T) {
	root := useTemporaryCertificateWriteRoot(t)
	certPEM, keyPEM := certificatePairPEM(t, "fresh.example.com")
	certPath := filepath.Join(root, "nodes", "fresh.cer")
	keyPath := filepath.Join(root, "nodes", "fresh.key")
	controller := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: &panel.CertInfo{
		CertMode: "remote", CertFile: certPath, KeyFile: keyPath,
		TlsCert: string(certPEM), TlsKey: string(keyPEM),
	}}}}
	if err := controller.requestCert(); err != nil {
		t.Fatalf("bootstrap remote pair: %v", err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("installed pair is invalid: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRemoteCertificateRotationReplacesMatchingPairAndPreservesOldOnInvalidInput(t *testing.T) {
	root := useTemporaryCertificateWriteRoot(t)
	oldCert, oldKey := certificatePairPEM(t, "old.example.com")
	newCert, newKey := certificatePairPEM(t, "new.example.com")
	certPath := filepath.Join(root, "node.cer")
	keyPath := filepath.Join(root, "node.key")
	if err := writeRemoteCertificatePair(certPath, keyPath, oldCert, oldKey); err != nil {
		t.Fatalf("install old pair: %v", err)
	}
	controller := &Controller{info: &panel.NodeInfo{Common: &panel.CommonNode{CertInfo: &panel.CertInfo{
		CertMode: "remote", CertFile: certPath, KeyFile: keyPath,
		TlsCert: string(newCert), TlsKey: string(newKey),
	}}}}
	if err := controller.requestCert(); err != nil {
		t.Fatalf("rotate remote pair: %v", err)
	}
	gotCert, _ := os.ReadFile(certPath)
	gotKey, _ := os.ReadFile(keyPath)
	if string(gotCert) != string(newCert) || string(gotKey) != string(newKey) {
		t.Fatal("rotation did not replace both certificate files")
	}
	if err := writeRemoteCertificatePair(certPath, keyPath, oldCert, newKey); err == nil {
		t.Fatal("mismatched rotation material was accepted")
	}
	stillCert, _ := os.ReadFile(certPath)
	stillKey, _ := os.ReadFile(keyPath)
	if string(stillCert) != string(newCert) || string(stillKey) != string(newKey) {
		t.Fatal("invalid rotation changed the live certificate pair")
	}
}

func TestRemoteCertificateRejectsSymlinkTarget(t *testing.T) {
	root := useTemporaryCertificateWriteRoot(t)
	certPEM, keyPEM := certificatePairPEM(t, "node.example.com")
	target := filepath.Join(root, "outside.cer")
	if err := os.WriteFile(target, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(root, "node.cer")
	if err := os.Symlink(target, certPath); err != nil {
		t.Fatal(err)
	}
	if err := writeRemoteCertificatePair(certPath, filepath.Join(root, "node.key"), certPEM, keyPEM); err == nil {
		t.Fatal("remote certificate writer accepted a symlink target")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "do not overwrite" {
		t.Fatal("symlink target was overwritten")
	}
}
