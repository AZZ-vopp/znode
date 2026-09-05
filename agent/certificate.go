package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	panel "github.com/AZZ-vopp/znode/api/v2board"
	commonfile "github.com/AZZ-vopp/znode/common/file"
)

const maxCertificateFileSize = 4 << 20

var certificateRoots = []string{"/etc/znode", "/etc/letsencrypt", "/etc/ssl"}

type certificateReporter interface {
	ReportCertificate(context.Context, string, panel.CertificateReport) error
}

type certificateVaultClient interface {
	BackupCertificateVault(context.Context, panel.AgentCertificateVaultRequest, string, string) (*panel.CertificateVaultMaterial, error)
	RestoreCertificateVault(context.Context, panel.AgentCertificateVaultRequest) (*panel.CertificateVaultMaterial, error)
}

// reconcileCertificateVault is deliberately before runtime construction. It
// only uploads an existing pair when the panel has no vault, and only restores
// when neither target exists; a live certificate is never overwritten.
func reconcileCertificateVault(ctx context.Context, client certificateVaultClient, requests []panel.AgentCertificateVaultRequest) (bool, error) {
	if client == nil {
		return false, nil
	}
	changed := false
	for _, request := range requests {
		certPath, keyPath, err := vaultPaths(request)
		if err != nil {
			return false, err
		}
		if err := recoverCertificateVaultTransaction(request.NodeID, certPath, keyPath); err != nil {
			return false, err
		}
		readCertPath, certReadErr := vaultReadPath(certPath, false)
		readKeyPath, keyReadErr := vaultReadPath(keyPath, true)
		certPresent := certReadErr == nil
		keyPresent := keyReadErr == nil
		if certPresent != keyPresent {
			return false, fmt.Errorf("certificate vault found incomplete local pair for node %d", request.NodeID)
		}
		switch request.Action {
		case "backup":
			if !certPresent || !keyPresent {
				// Fresh Auto TLS nodes have no material yet. Do not prevent the
				// runtime from booting and generating its first certificate.
				if os.IsNotExist(certReadErr) && os.IsNotExist(keyReadErr) {
					continue
				}
				if certReadErr != nil || keyReadErr != nil {
					return false, fmt.Errorf("certificate vault cannot read local pair for node %d", request.NodeID)
				}
				continue
			}
			cert, err := commonfile.ReadRegularFileLimited(readCertPath, 128<<10)
			if err != nil {
				return false, err
			}
			key, err := commonfile.ReadRegularFileLimited(readKeyPath, 128<<10)
			if err != nil {
				return false, err
			}
			if err := validateCertificatePair(cert, key); err != nil {
				return false, err
			}
			if _, err = client.BackupCertificateVault(ctx, request, string(cert), string(key)); err != nil {
				return false, err
			}
		case "sync":
			if certPresent {
				cert, err := commonfile.ReadRegularFileLimited(readCertPath, 128<<10)
				if err != nil {
					return false, err
				}
				key, err := commonfile.ReadRegularFileLimited(readKeyPath, 128<<10)
				if err != nil {
					return false, err
				}
				if err := validateCertificatePair(cert, key); err != nil {
					return false, err
				}
				if localSHA, err := certificateLeafSHA256(cert); err == nil && request.ExpectedSHA256 != "" && strings.EqualFold(localSHA, request.ExpectedSHA256) {
					continue
				}
				if _, err := client.BackupCertificateVault(ctx, request, string(cert), string(key)); err != nil {
					return false, err
				}
				continue
			}
			material, err := client.RestoreCertificateVault(ctx, request)
			if err != nil {
				return false, err
			}
			if err := validateCertificatePair([]byte(material.CertificatePEM), []byte(material.PrivateKeyPEM)); err != nil {
				return false, err
			}
			if request.ExpectedSHA256 != "" {
				actual, err := certificateLeafSHA256([]byte(material.CertificatePEM))
				if err != nil || !strings.EqualFold(actual, request.ExpectedSHA256) {
					return false, fmt.Errorf("restored certificate fingerprint does not match vault")
				}
			}
			if err := atomicCertificatePair(request.NodeID, certPath, keyPath, []byte(material.CertificatePEM), []byte(material.PrivateKeyPEM)); err != nil {
				return false, err
			}
			changed = true
		case "restore":
			if certPresent || keyPresent {
				continue
			}
			material, err := client.RestoreCertificateVault(ctx, request)
			if err != nil {
				return false, err
			}
			if err := validateCertificatePair([]byte(material.CertificatePEM), []byte(material.PrivateKeyPEM)); err != nil {
				return false, fmt.Errorf("validate restored certificate: %w", err)
			}
			if err := atomicCertificatePair(request.NodeID, certPath, keyPath, []byte(material.CertificatePEM), []byte(material.PrivateKeyPEM)); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

func vaultPaths(request panel.AgentCertificateVaultRequest) (string, string, error) {
	if request.CertFile == request.KeyFile {
		return "", "", fmt.Errorf("certificate and key paths must differ")
	}
	cert, err := allowedVaultPath(request.CertFile, false)
	if err != nil {
		return "", "", err
	}
	key, err := allowedVaultPath(request.KeyFile, true)
	if err != nil {
		return "", "", err
	}
	return cert, key, nil
}

func allowedVaultPath(path string, key bool) (string, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("certificate path must be absolute")
	}
	ext := strings.ToLower(filepath.Ext(path))
	if (!key && ext != ".cer" && ext != ".crt" && ext != ".pem") || (key && ext != ".key" && ext != ".pem") {
		return "", fmt.Errorf("certificate path has unsupported extension")
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		path = filepath.Join(parent, filepath.Base(path))
	}
	for _, root := range certificateRoots {
		root = filepath.Clean(root)
		if resolvedRoot, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
			root = resolvedRoot
		}
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			current := root
			for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
				if part == "." || part == "" {
					continue
				}
				current = filepath.Join(current, part)
				if info, statErr := os.Lstat(current); statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return "", fmt.Errorf("certificate path contains unsafe directory")
				}
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("certificate path is outside allowed roots")
}

func regularFileExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// vaultReadPath permits Certbot's cert.pem -> archive/... symlinks, but only
// after the final target remains inside an allowlisted certificate root.
func vaultReadPath(path string, key bool) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if _, err := allowedVaultPath(resolved, key); err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 128<<10 {
		return "", fmt.Errorf("certificate material is not a bounded regular file")
	}
	return resolved, nil
}

func validateCertificatePair(certPEM, keyPEM []byte) error {
	var leaf *x509.Certificate
	rest := certPEM
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = next
		if block.Type == "CERTIFICATE" {
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				return err
			}
			if !cert.IsCA && leaf == nil {
				leaf = cert
			}
		}
	}
	if leaf == nil {
		return fmt.Errorf("no non-CA certificate")
	}
	if now := time.Now(); leaf.NotAfter.Before(now.Add(5*time.Minute)) || leaf.NotBefore.After(now.Add(5*time.Minute)) {
		return fmt.Errorf("certificate is expired or not yet valid")
	}
	block, rest := pem.Decode(keyPEM)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return fmt.Errorf("expected exactly one private key")
	}
	var private any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		private, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		private, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		private, err = x509.ParseECPrivateKey(block.Bytes)
	default:
		return fmt.Errorf("unsupported private key")
	}
	if err != nil {
		return err
	}
	pub, err := x509.MarshalPKIXPublicKey(publicFromPrivate(private))
	if err != nil {
		return err
	}
	if string(pub) != string(leaf.RawSubjectPublicKeyInfo) {
		return fmt.Errorf("certificate and private key do not match")
	}
	return nil
}

func certificateLeafSHA256(certPEM []byte) (string, error) {
	for rest := certPEM; ; {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = next
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", err
		}
		if !cert.IsCA {
			digest := sha256.Sum256(cert.Raw)
			return hex.EncodeToString(digest[:]), nil
		}
	}
	return "", fmt.Errorf("no leaf certificate")
}

func publicFromPrivate(key any) any {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return &k.PublicKey
	case *ecdsa.PrivateKey:
		return &k.PublicKey
	case ed25519.PrivateKey:
		return k.Public()
	default:
		return nil
	}
}

type certificateVaultJournal struct {
	NodeID     int    `json:"node_id"`
	CertPath   string `json:"cert_path"`
	KeyPath    string `json:"key_path"`
	CertSHA256 string `json:"cert_sha256"`
	KeySHA256  string `json:"key_sha256"`
}

func certificateVaultJournalPath(certPath string, nodeID int) string {
	return filepath.Join(filepath.Dir(certPath), fmt.Sprintf(".znode-vault-%d.json", nodeID))
}

func atomicCertificatePair(nodeID int, certPath, keyPath string, cert, key []byte) error {
	certTemp, err := stageCertificateFile(certPath, cert, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(certTemp)
	keyTemp, err := stageCertificateFile(keyPath, key, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(keyTemp)
	// Restore only calls this with absent material; retaining this guard makes a
	// direct caller fail safely rather than replacing a live pair.
	if _, err := os.Lstat(certPath); err == nil {
		return fmt.Errorf("refusing to overwrite active certificate")
	}
	if _, err := os.Lstat(keyPath); err == nil {
		return fmt.Errorf("refusing to overwrite active private key")
	}
	journal := certificateVaultJournal{NodeID: nodeID, CertPath: certPath, KeyPath: keyPath, CertSHA256: fmt.Sprintf("%x", sha256.Sum256(cert)), KeySHA256: fmt.Sprintf("%x", sha256.Sum256(key))}
	journalData, _ := json.Marshal(journal)
	journalPath := certificateVaultJournalPath(certPath, nodeID)
	if err := writeJournal(journalPath, journalData); err != nil {
		return err
	}
	defer func() { _ = finalizeCertificateVaultJournal(journalPath) }()
	if err = os.Rename(certTemp, certPath); err != nil {
		return err
	}
	if err = os.Rename(keyTemp, keyPath); err != nil {
		// No prior material existed, so rollback is an unlink, leaving a retryable
		// absent pair rather than a broken half-installed certificate.
		_ = os.Remove(certPath)
		return fmt.Errorf("commit private key; certificate rollback attempted: %w", err)
	}
	return syncCertificateDirectories(certPath, keyPath)
}

func writeJournal(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func recoverCertificateVaultTransaction(nodeID int, certPath, keyPath string) error {
	path := certificateVaultJournalPath(certPath, nodeID)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var journal certificateVaultJournal
	if err := json.Unmarshal(data, &journal); err != nil || journal.NodeID != nodeID || journal.CertPath != certPath || journal.KeyPath != keyPath {
		return fmt.Errorf("invalid certificate vault journal")
	}
	certMatch := journalDigestMatches(certPath, journal.CertSHA256)
	keyMatch := journalDigestMatches(keyPath, journal.KeySHA256)
	if !certMatch && !keyMatch {
		_, certErr := os.Lstat(certPath)
		_, keyErr := os.Lstat(keyPath)
		if os.IsNotExist(certErr) && os.IsNotExist(keyErr) {
			return finalizeCertificateVaultJournal(path)
		}
		return fmt.Errorf("certificate vault journal conflicts with local material")
	}
	if certMatch != keyMatch {
		if certMatch {
			if err := os.Remove(certPath); err != nil {
				return err
			}
		}
		if keyMatch {
			if err := os.Remove(keyPath); err != nil {
				return err
			}
		}
		return finalizeCertificateVaultJournal(path)
	}
	if certMatch && keyMatch {
		return finalizeCertificateVaultJournal(path)
	}
	// Do not remove unrelated files; leave an explicit error for manual repair.
	return fmt.Errorf("certificate vault journal conflicts with local material")
}

func finalizeCertificateVaultJournal(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func journalDigestMatches(path, expected string) bool {
	data, err := os.ReadFile(path)
	return err == nil && fmt.Sprintf("%x", sha256.Sum256(data)) == expected
}

func stageCertificateFile(path string, content []byte, mode os.FileMode) (string, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("refusing non-regular certificate target")
	}
	dir := filepath.Dir(path)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("invalid certificate directory")
	}
	tmp, err := os.CreateTemp(dir, ".znode-cert-")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(content)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	return tmpName, nil
}

func syncCertificateDirectories(paths ...string) error {
	for _, path := range paths {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func reconcileCertificate(ctx context.Context, reporter certificateReporter, request *panel.AgentCertificateRequest) error {
	if reporter == nil || request == nil {
		return nil
	}
	report := panel.CertificateReport{NodeID: request.NodeID, Status: "completed"}
	certFile, data, err := readRequestedCertificate(request)
	if err != nil {
		report.Status = "failed"
		report.Message = err.Error()
		return reporter.ReportCertificate(ctx, request.ID, report)
	}
	var certs []*x509.Certificate
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			break
		}
		data = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr == nil {
			certs = append(certs, cert)
		}
	}
	if len(certs) == 0 {
		report.Status = "failed"
		report.Message = fmt.Sprintf("no PEM certificate found in %s", certFile)
		return reporter.ReportCertificate(ctx, request.ID, report)
	}
	cert := certs[0]
	for _, candidate := range certs {
		if !candidate.IsCA {
			cert = candidate
			break
		}
	}
	certHash := sha256.Sum256(cert.Raw)
	keyHash := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	report.SHA256 = hex.EncodeToString(certHash[:])
	report.PublicKeySHA256 = base64.StdEncoding.EncodeToString(keyHash[:])
	report.NotAfter = cert.NotAfter.Unix()
	report.Issuer = cert.Issuer.String()
	return reporter.ReportCertificate(ctx, request.ID, report)
}

func readRequestedCertificate(request *panel.AgentCertificateRequest) (string, []byte, error) {
	candidates := []string{request.CertFile}
	ext := filepath.Ext(request.CertFile)
	base := request.CertFile[:len(request.CertFile)-len(ext)]
	for _, candidateExt := range []string{".cer", ".crt", ".pem"} {
		candidates = append(candidates, base+candidateExt)
	}
	for _, candidateExt := range []string{"cer", "crt", "pem"} {
		matches, _ := filepath.Glob(fmt.Sprintf("/etc/znode/*%d.%s", request.NodeID, candidateExt))
		candidates = append(candidates, matches...)
	}

	seen := make(map[string]struct{}, len(candidates))
	var lastErr error
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		resolved, err := allowedCertificatePath(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := commonfile.ReadRegularFileLimited(resolved, maxCertificateFileSize)
		if err == nil {
			return resolved, data, nil
		}
		lastErr = err
	}
	return "", nil, fmt.Errorf("certificate file for node %d was not found (requested %q): %v", request.NodeID, request.CertFile, lastErr)
}

func allowedCertificatePath(candidate string) (string, error) {
	candidate = filepath.Clean(strings.TrimSpace(candidate))
	if candidate == "." || !filepath.IsAbs(candidate) {
		return "", fmt.Errorf("certificate path must be absolute")
	}
	extension := strings.ToLower(filepath.Ext(candidate))
	if extension != ".cer" && extension != ".crt" && extension != ".pem" {
		return "", fmt.Errorf("certificate path has an unsupported extension")
	}

	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	resolvedExtension := strings.ToLower(filepath.Ext(resolved))
	if resolvedExtension != ".cer" && resolvedExtension != ".crt" && resolvedExtension != ".pem" {
		return "", fmt.Errorf("resolved certificate path has an unsupported extension")
	}
	allowed := false
	for _, root := range certificateRoots {
		rootResolved, rootErr := filepath.EvalSymlinks(root)
		if rootErr != nil {
			continue
		}
		relative, relErr := filepath.Rel(rootResolved, resolved)
		if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", fmt.Errorf("certificate path is outside the allowed certificate directories")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxCertificateFileSize {
		return "", fmt.Errorf("certificate file must be a regular file between 1 byte and %d bytes", maxCertificateFileSize)
	}
	return resolved, nil
}
