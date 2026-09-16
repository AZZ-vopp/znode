package agent

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
)

func TestAllowedCertificatePathRejectsTraversalAndFilesOutsideRoots(t *testing.T) {
	root := t.TempDir()
	oldRoots := certificateRoots
	certificateRoots = []string{root}
	defer func() { certificateRoots = oldRoots }()

	valid := filepath.Join(root, "node.crt")
	if err := os.WriteFile(valid, []byte("certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantResolved, err := filepath.EvalSymlinks(valid)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, err := allowedCertificatePath(valid); err != nil || resolved != wantResolved {
		t.Fatalf("valid certificate path rejected: resolved=%q err=%v", resolved, err)
	}

	outside := filepath.Join(t.TempDir(), "secret.pem")
	if err := os.WriteFile(outside, []byte("private material"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := allowedCertificatePath(outside); err == nil {
		t.Fatal("certificate reader accepted a file outside the allowlisted roots")
	}
	if _, err := allowedCertificatePath(filepath.Join(root, "node.key")); err == nil {
		t.Fatal("certificate reader accepted a private-key extension")
	}
}

type vaultTestClient struct {
	material *panel.CertificateVaultMaterial
}

func (c vaultTestClient) BackupCertificateVault(context.Context, panel.AgentCertificateVaultRequest, string, string) (*panel.CertificateVaultMaterial, error) {
	return &panel.CertificateVaultMaterial{}, nil
}
func (c vaultTestClient) RestoreCertificateVault(context.Context, panel.AgentCertificateVaultRequest) (*panel.CertificateVaultMaterial, error) {
	return c.material, nil
}

func TestCertificateVaultRestoreWritesAtomicPairWithSafePermissions(t *testing.T) {
	root := t.TempDir()
	oldRoots := certificateRoots
	certificateRoots = []string{root}
	defer func() { certificateRoots = oldRoots }()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	req := panel.AgentCertificateVaultRequest{ID: "0123456789abcdef0123456789abcdef", NodeID: 1, Action: "restore", CertFile: filepath.Join(root, "node.cer"), KeyFile: filepath.Join(root, "node.key")}
	if _, err := reconcileCertificateVault(context.Background(), vaultTestClient{material: &panel.CertificateVaultMaterial{CertificatePEM: string(cert), PrivateKeyPEM: string(keyPEM)}}, []panel.AgentCertificateVaultRequest{req}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Stat(req.CertFile); got.Mode().Perm() != 0o644 {
		t.Fatalf("cert mode = %o", got.Mode().Perm())
	}
	if got, _ := os.Stat(req.KeyFile); got.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o", got.Mode().Perm())
	}
	if err := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "link.cer")); err != nil {
		t.Fatal(err)
	}
	if err := atomicCertificatePair(1, filepath.Join(root, "link.cer"), filepath.Join(root, "node2.key"), cert, keyPEM); err == nil {
		t.Fatal("restore followed a symlink target")
	}
}

func TestVaultReadPathAllowsCertbotStyleSymlinkOnlyInsideRoots(t *testing.T) {
	root := t.TempDir()
	oldRoots := certificateRoots
	certificateRoots = []string{root}
	defer func() { certificateRoots = oldRoots }()
	archive := filepath.Join(root, "archive")
	if err := os.Mkdir(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(archive, "cert1.pem")
	if err := os.WriteFile(target, []byte("certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "cert.pem")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(target)
	if resolved, err := vaultReadPath(link, false); err != nil || resolved != want {
		t.Fatalf("certbot symlink = %q, %v", resolved, err)
	}
	outside := filepath.Join(t.TempDir(), "secret.pem")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := vaultReadPath(filepath.Join(root, "outside.pem"), false); err == nil {
		t.Fatal("symlink escaped certificate root")
	}
}

func TestVaultJournalRecoversOnlyMatchingHalfPair(t *testing.T) {
	root := t.TempDir()
	cert := filepath.Join(root, "node.cer")
	key := filepath.Join(root, "node.key")
	data := []byte("transaction-cert")
	if err := os.WriteFile(cert, data, 0o644); err != nil {
		t.Fatal(err)
	}
	journal := certificateVaultJournal{NodeID: 9, CertPath: cert, KeyPath: key, CertSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), KeySHA256: "missing"}
	encoded, _ := json.Marshal(journal)
	if err := os.WriteFile(certificateVaultJournalPath(cert, 9), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverCertificateVaultTransaction(9, cert, key); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cert); !os.IsNotExist(err) {
		t.Fatal("matching half pair was not removed")
	}
	if _, err := os.Stat(certificateVaultJournalPath(cert, 9)); !os.IsNotExist(err) {
		t.Fatal("journal was not cleared")
	}
}

func TestAllowedCertificatePathBoundsFileSize(t *testing.T) {
	root := t.TempDir()
	oldRoots := certificateRoots
	certificateRoots = []string{root}
	defer func() { certificateRoots = oldRoots }()

	empty := filepath.Join(root, "empty.pem")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := allowedCertificatePath(empty); err == nil {
		t.Fatal("certificate reader accepted an empty file")
	}
}
