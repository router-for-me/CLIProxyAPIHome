package cluster

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const certificateKeyBits = 2048
const enrollmentSecretBytes = 32

type certificateRequestResponse struct {
	OK          bool   `json:"ok"`
	Certificate string `json:"certificate"`
	CA          string `json:"ca"`
}

type homeJWTClaims struct {
	CertificateID    string `json:"certificate_id"`
	ClusterID        string `json:"cluster_id"`
	CAFingerprint    string `json:"ca_fingerprint"`
	EnrollmentSecret string `json:"enrollment_secret"`
	IP               string `json:"ip"`
	Port             int    `json:"port"`
	IssuedAt         int64  `json:"iat"`
}

// EnsureClusterCertificates makes sure the cluster CA and this node server certificate exist.
func (r *Repository) EnsureClusterCertificates(ctx context.Context, ip string) (*tls.Config, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil, fmt.Errorf("cluster certificate ip is required")
	}
	ctx = contextOrBackground(ctx)

	ca, errCA := r.ensureCARecord(ctx, db)
	if errCA != nil {
		return nil, errCA
	}
	server, errServer := r.ensureServerCertificateRecord(ctx, db, ca, ip)
	if errServer != nil {
		return nil, errServer
	}
	tlsConfig, errTLS := tlsConfigFromCertificateRecords(ca, server)
	if errTLS != nil {
		return nil, errTLS
	}
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		return r.verifyPeerCertificateFingerprint(context.Background(), state)
	}
	return tlsConfig, nil
}

// CreatePendingClientCertificate creates an empty client certificate slot.
func (r *Repository) CreatePendingClientCertificate(ctx context.Context) (string, string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return "", "", errDB
	}
	ctx = contextOrBackground(ctx)
	ca, errCA := r.ensureCARecord(ctx, db)
	if errCA != nil {
		return "", "", errCA
	}
	caFingerprint, errFingerprint := certificateFingerprintPEM([]byte(ca.CertificatePEM))
	if errFingerprint != nil {
		return "", "", errFingerprint
	}
	secret, errSecret := randomEnrollmentSecret()
	if errSecret != nil {
		return "", "", errSecret
	}
	id, errID := randomUUID()
	if errID != nil {
		return "", "", errID
	}
	record := &CertificateRecord{
		ID:                   id,
		ClusterID:            ca.ID,
		CAFingerprint:        caFingerprint,
		EnrollmentSecretHash: hashEnrollmentSecret(secret),
	}
	if errCreate := db.WithContext(ctx).Create(record).Error; errCreate != nil {
		return "", "", errCreate
	}
	return id, secret, nil
}

// ClusterCAKeyPair returns the cluster root CA certificate and private key.
func (r *Repository) ClusterCAKeyPair(ctx context.Context) (*x509.Certificate, *rsa.PrivateKey, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, errDB
	}
	ctx = contextOrBackground(ctx)
	ca, errCA := r.ensureCARecord(ctx, db)
	if errCA != nil {
		return nil, nil, errCA
	}
	if ca == nil {
		return nil, nil, fmt.Errorf("cluster certificate ca is missing")
	}
	cert, errCert := parseCertificatePEM([]byte(ca.CertificatePEM))
	if errCert != nil {
		return nil, nil, errCert
	}
	key, errKey := parseRSAPrivateKeyPEM([]byte(ca.PrivateKeyPEM))
	if errKey != nil {
		return nil, nil, errKey
	}
	return cert, key, nil
}

// CurrentMasterNode returns the current master node if one is recorded.
func (r *Repository) CurrentMasterNode(ctx context.Context) (*ClusterNodeRecord, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, errDB
	}
	record := &ClusterNodeRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).
		Where("is_master = ?", true).
		Order("last_seen_at DESC, started_at ASC, ip ASC, port ASC").
		First(record).Error
	if errFirst == nil {
		return record, nil
	}
	if errFirst == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return nil, errFirst
}

// CreateHomeJWT creates a signed Home JWT with the client certificate id and target node.
func (r *Repository) CreateHomeJWT(ctx context.Context, certificateID string, ip string, port int, enrollmentSecret string) (string, error) {
	db, errDB := r.database()
	if errDB != nil {
		return "", errDB
	}
	certificateID = strings.TrimSpace(certificateID)
	ip = strings.TrimSpace(ip)
	if certificateID == "" {
		return "", fmt.Errorf("certificate id is required")
	}
	enrollmentSecret = strings.TrimSpace(enrollmentSecret)
	if enrollmentSecret == "" {
		return "", fmt.Errorf("enrollment secret is required")
	}
	if ip == "" || port <= 0 {
		return "", fmt.Errorf("home jwt target address is invalid")
	}
	ca, errCA := r.ensureCARecord(contextOrBackground(ctx), db)
	if errCA != nil {
		return "", errCA
	}
	key, errKey := parseRSAPrivateKeyPEM([]byte(ca.PrivateKeyPEM))
	if errKey != nil {
		return "", errKey
	}
	caFingerprint, errFingerprint := certificateFingerprintPEM([]byte(ca.CertificatePEM))
	if errFingerprint != nil {
		return "", errFingerprint
	}
	claims := homeJWTClaims{
		CertificateID:    certificateID,
		ClusterID:        ca.ID,
		CAFingerprint:    caFingerprint,
		EnrollmentSecret: enrollmentSecret,
		IP:               ip,
		Port:             port,
		IssuedAt:         time.Now().UTC().Unix(),
	}
	return signHomeJWT(key, claims)
}

// SignClientCertificateRequest signs a client CSR exactly once and returns the client certificate plus CA.
func (r *Repository) SignClientCertificateRequest(ctx context.Context, certificateID string, enrollmentSecret string, csrPEM []byte) ([]byte, []byte, error) {
	db, errDB := r.database()
	if errDB != nil {
		return nil, nil, errDB
	}
	certificateID = strings.TrimSpace(certificateID)
	if certificateID == "" {
		return nil, nil, fmt.Errorf("certificate id is required")
	}
	enrollmentSecret = strings.TrimSpace(enrollmentSecret)
	if enrollmentSecret == "" {
		return nil, nil, fmt.Errorf("enrollment secret is required")
	}
	csr, errCSR := parseCertificateRequestPEM(csrPEM)
	if errCSR != nil {
		return nil, nil, errCSR
	}
	if errCheck := csr.CheckSignature(); errCheck != nil {
		return nil, nil, fmt.Errorf("certificate request signature is invalid: %w", errCheck)
	}

	var certPEM []byte
	var caPEM []byte
	ctx = contextOrBackground(ctx)
	errTransaction := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ca, errCA := firstCARecord(ctx, tx)
		if errCA != nil {
			return errCA
		}
		if ca == nil {
			return fmt.Errorf("cluster certificate ca is missing")
		}
		caCert, errCACert := parseCertificatePEM([]byte(ca.CertificatePEM))
		if errCACert != nil {
			return errCACert
		}
		caKey, errCAKey := parseRSAPrivateKeyPEM([]byte(ca.PrivateKeyPEM))
		if errCAKey != nil {
			return errCAKey
		}

		record := &CertificateRecord{}
		errFirst := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", certificateID).First(record).Error
		if errFirst != nil {
			return errFirst
		}
		if strings.TrimSpace(record.CertificatePEM) != "" {
			return fmt.Errorf("certificate id has already been issued")
		}
		if record.IsCA || record.IsServer {
			return fmt.Errorf("certificate id is not a pending client certificate")
		}
		if !verifyEnrollmentSecret(record.EnrollmentSecretHash, enrollmentSecret) {
			return fmt.Errorf("enrollment secret is invalid")
		}

		now := time.Now().UTC()
		serial, errSerial := randomSerialNumber()
		if errSerial != nil {
			return errSerial
		}
		template := &x509.Certificate{
			SerialNumber: serial,
			Subject: pkix.Name{
				CommonName: certificateID,
			},
			NotBefore:             now,
			NotAfter:              now.AddDate(100, 0, 0),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			BasicConstraintsValid: true,
		}
		der, errCreate := x509.CreateCertificate(rand.Reader, template, caCert, csr.PublicKey, caKey)
		if errCreate != nil {
			return errCreate
		}
		certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		caPEM = []byte(ca.CertificatePEM)
		record.CertificatePEM = string(certPEM)
		record.CertificateFingerprint = certificateFingerprintDER(der)
		record.CSRPEM = string(csrPEM)
		record.PrivateKeyPEM = ""
		record.EnrollmentSecretHash = ""
		record.IsClient = true
		record.IsCA = false
		record.IsServer = false
		record.SerialNumber = serial.String()
		record.NotBefore = template.NotBefore
		record.NotAfter = template.NotAfter
		return tx.Save(record).Error
	})
	if errTransaction != nil {
		return nil, nil, errTransaction
	}
	return certPEM, caPEM, nil
}

// SignClientCertificateRequestJSON signs a CSR and returns the RESP JSON payload.
func (r *Repository) SignClientCertificateRequestJSON(ctx context.Context, certificateID string, enrollmentSecret string, csrPEM []byte) ([]byte, error) {
	certPEM, caPEM, errSign := r.SignClientCertificateRequest(ctx, certificateID, enrollmentSecret, csrPEM)
	if errSign != nil {
		return nil, errSign
	}
	payload := certificateRequestResponse{
		OK:          true,
		Certificate: string(certPEM),
		CA:          string(caPEM),
	}
	return json.Marshal(payload)
}

func (r *Repository) verifyPeerCertificateFingerprint(ctx context.Context, state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return nil
	}
	if len(state.VerifiedChains) == 0 {
		return fmt.Errorf("cluster certificate peer chain is not verified")
	}
	ok, errAllowed := r.peerCertificateFingerprintAllowed(ctx, state.PeerCertificates[0])
	if errAllowed != nil {
		return errAllowed
	}
	if !ok {
		return fmt.Errorf("cluster certificate peer fingerprint is revoked or unknown")
	}
	return nil
}

func (r *Repository) peerCertificateFingerprintAllowed(ctx context.Context, cert *x509.Certificate) (bool, error) {
	if cert == nil {
		return false, nil
	}
	fingerprint := certificateFingerprint(cert)
	if fingerprint == "" {
		return false, nil
	}
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	var count int64
	errCount := db.WithContext(WithDatabaseQueryName(contextOrBackground(ctx), "cluster.certificate.peer_allowed")).
		Model(&CertificateRecord{}).
		Where("certificate_fingerprint = ? AND certificate_pem <> ? AND (is_client = ? OR is_server = ?)", fingerprint, "", true, true).
		Count(&count).Error
	if errCount != nil {
		return false, errCount
	}
	return count > 0, nil
}

func (r *Repository) ClientCertificateFingerprintActive(ctx context.Context, certificateID string, fingerprint string) (bool, error) {
	certificateID = strings.TrimSpace(certificateID)
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	if certificateID == "" || fingerprint == "" {
		return false, nil
	}
	db, errDB := r.database()
	if errDB != nil {
		return false, errDB
	}
	now := time.Now().UTC()
	var count int64
	if errCount := db.WithContext(contextOrBackground(ctx)).Model(&CertificateRecord{}).
		Where("id = ? AND certificate_fingerprint = ? AND is_client = ? AND certificate_pem <> ? AND not_before <= ? AND not_after > ?", certificateID, fingerprint, true, "", now, now).
		Count(&count).Error; errCount != nil {
		return false, errCount
	}
	return count > 0, nil
}

func (r *Repository) ensureCARecord(ctx context.Context, db *gorm.DB) (*CertificateRecord, error) {
	ca, errCA := firstCARecord(ctx, db)
	if errCA != nil {
		return nil, errCA
	}
	if ca != nil {
		return ca, nil
	}
	now := time.Now().UTC()
	certPEM, keyPEM, serial, errCreate := createCACertificate(now)
	if errCreate != nil {
		return nil, errCreate
	}
	fingerprint, errFingerprint := certificateFingerprintPEM(certPEM)
	if errFingerprint != nil {
		return nil, errFingerprint
	}
	id, errID := randomUUID()
	if errID != nil {
		return nil, errID
	}
	record := &CertificateRecord{
		ID:                     id,
		CertificatePEM:         string(certPEM),
		CertificateFingerprint: fingerprint,
		PrivateKeyPEM:          string(keyPEM),
		IsCA:                   true,
		SerialNumber:           serial,
		NotBefore:              now,
		NotAfter:               now.AddDate(100, 0, 0),
	}
	if errCreateRecord := db.WithContext(ctx).Create(record).Error; errCreateRecord != nil {
		return nil, errCreateRecord
	}
	return record, nil
}

func (r *Repository) ensureServerCertificateRecord(ctx context.Context, db *gorm.DB, ca *CertificateRecord, ip string) (*CertificateRecord, error) {
	record := &CertificateRecord{}
	errFirst := db.WithContext(ctx).Where("is_server = ? AND ip = ?", true, ip).First(record).Error
	if errFirst == nil {
		if certificateRecordSupportsNodeMTLS(record) {
			return record, nil
		}
		return r.writeServerCertificateRecord(ctx, db, ca, ip, record)
	}
	if errFirst != nil && errFirst != gorm.ErrRecordNotFound {
		return nil, errFirst
	}
	return r.writeServerCertificateRecord(ctx, db, ca, ip, nil)
}

func (r *Repository) writeServerCertificateRecord(ctx context.Context, db *gorm.DB, ca *CertificateRecord, ip string, record *CertificateRecord) (*CertificateRecord, error) {
	caCert, errCACert := parseCertificatePEM([]byte(ca.CertificatePEM))
	if errCACert != nil {
		return nil, errCACert
	}
	caKey, errCAKey := parseRSAPrivateKeyPEM([]byte(ca.PrivateKeyPEM))
	if errCAKey != nil {
		return nil, errCAKey
	}
	now := time.Now().UTC()
	certPEM, keyPEM, serial, errCreate := createServerCertificate(now, caCert, caKey, ip)
	if errCreate != nil {
		return nil, errCreate
	}
	fingerprint, errFingerprint := certificateFingerprintPEM(certPEM)
	if errFingerprint != nil {
		return nil, errFingerprint
	}
	notAfter := now.AddDate(100, 0, 0)
	if record != nil && strings.TrimSpace(record.ID) != "" {
		updates := map[string]any{
			"certificate_pem":         string(certPEM),
			"certificate_fingerprint": fingerprint,
			"private_key_pem":         string(keyPEM),
			"serial_number":           serial,
			"not_before":              now,
			"not_after":               notAfter,
		}
		if errUpdate := db.WithContext(ctx).Model(record).Updates(updates).Error; errUpdate != nil {
			return nil, errUpdate
		}
		record.CertificatePEM = string(certPEM)
		record.CertificateFingerprint = fingerprint
		record.PrivateKeyPEM = string(keyPEM)
		record.SerialNumber = serial
		record.NotBefore = now
		record.NotAfter = notAfter
		return record, nil
	}

	id, errID := randomUUID()
	if errID != nil {
		return nil, errID
	}
	record = &CertificateRecord{
		ID:                     id,
		CertificatePEM:         string(certPEM),
		CertificateFingerprint: fingerprint,
		PrivateKeyPEM:          string(keyPEM),
		IP:                     ip,
		IsServer:               true,
		SerialNumber:           serial,
		NotBefore:              now,
		NotAfter:               notAfter,
	}
	if errCreateRecord := db.WithContext(ctx).Create(record).Error; errCreateRecord != nil {
		return nil, errCreateRecord
	}
	return record, nil
}

func certificateRecordSupportsNodeMTLS(record *CertificateRecord) bool {
	if record == nil {
		return false
	}
	cert, errCert := parseCertificatePEM([]byte(record.CertificatePEM))
	if errCert != nil {
		return false
	}
	return hasExtKeyUsage(cert, x509.ExtKeyUsageServerAuth) && hasExtKeyUsage(cert, x509.ExtKeyUsageClientAuth)
}

func hasExtKeyUsage(cert *x509.Certificate, usage x509.ExtKeyUsage) bool {
	if cert == nil {
		return false
	}
	for _, existingUsage := range cert.ExtKeyUsage {
		if existingUsage == usage {
			return true
		}
	}
	return false
}

func firstCARecord(ctx context.Context, db *gorm.DB) (*CertificateRecord, error) {
	record := &CertificateRecord{}
	errFirst := db.WithContext(contextOrBackground(ctx)).Where("is_ca = ?", true).Order("created_at ASC").First(record).Error
	if errFirst == nil {
		return record, nil
	}
	if errFirst == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return nil, errFirst
}

func tlsConfigFromCertificateRecords(ca *CertificateRecord, server *CertificateRecord) (*tls.Config, error) {
	if ca == nil || server == nil {
		return nil, fmt.Errorf("cluster certificate records are required")
	}
	certPair, errPair := tls.X509KeyPair([]byte(server.CertificatePEM), []byte(server.PrivateKeyPEM))
	if errPair != nil {
		return nil, errPair
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM([]byte(ca.CertificatePEM)) {
		return nil, fmt.Errorf("cluster ca certificate is invalid")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certPair},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    caPool,
		RootCAs:      caPool,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

func createCACertificate(now time.Time) ([]byte, []byte, string, error) {
	key, errKey := rsa.GenerateKey(rand.Reader, certificateKeyBits)
	if errKey != nil {
		return nil, nil, "", errKey
	}
	serial, errSerial := randomSerialNumber()
	if errSerial != nil {
		return nil, nil, "", errSerial
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "CLIProxyAPIHome CA",
		},
		NotBefore:             now,
		NotAfter:              now.AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, errCreate := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if errCreate != nil {
		return nil, nil, "", errCreate
	}
	return encodeCertificatePEM(der), encodeRSAPrivateKeyPEM(key), serial.String(), nil
}

func createServerCertificate(now time.Time, ca *x509.Certificate, caKey *rsa.PrivateKey, host string) ([]byte, []byte, string, error) {
	key, errKey := rsa.GenerateKey(rand.Reader, certificateKeyBits)
	if errKey != nil {
		return nil, nil, "", errKey
	}
	serial, errSerial := randomSerialNumber()
	if errSerial != nil {
		return nil, nil, "", errSerial
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		NotBefore:             now,
		NotAfter:              now.AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, errCreate := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if errCreate != nil {
		return nil, nil, "", errCreate
	}
	return encodeCertificatePEM(der), encodeRSAPrivateKeyPEM(key), serial.String(), nil
}

func randomSerialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, errSerial := rand.Int(rand.Reader, limit)
	if errSerial != nil {
		return nil, errSerial
	}
	if serial.Sign() == 0 {
		serial = big.NewInt(1)
	}
	return serial, nil
}

func randomEnrollmentSecret() (string, error) {
	raw := make([]byte, enrollmentSecretBytes)
	if _, errRead := rand.Read(raw); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashEnrollmentSecret(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}

func verifyEnrollmentSecret(storedHash string, secret string) bool {
	storedHash = strings.TrimSpace(storedHash)
	if storedHash == "" {
		return false
	}
	expected := hashEnrollmentSecret(secret)
	return subtle.ConstantTimeCompare([]byte(storedHash), []byte(expected)) == 1
}

func encodeCertificatePEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeRSAPrivateKeyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func parseCertificatePEM(raw []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("certificate pem is invalid")
	}
	return x509.ParseCertificate(block.Bytes)
}

func certificateFingerprintPEM(raw []byte) (string, error) {
	cert, errCert := parseCertificatePEM(raw)
	if errCert != nil {
		return "", errCert
	}
	return certificateFingerprint(cert), nil
}

func certificateFingerprint(cert *x509.Certificate) string {
	if cert == nil || len(cert.Raw) == 0 {
		return ""
	}
	return certificateFingerprintDER(cert.Raw)
}

func certificateFingerprintDER(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func parseRSAPrivateKeyPEM(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("private key pem is invalid")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, errParse := x509.ParsePKCS8PrivateKey(block.Bytes)
		if errParse != nil {
			return nil, errParse
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not rsa")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("private key pem type %q is unsupported", block.Type)
	}
}

func parseCertificateRequestPEM(raw []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("certificate request pem is invalid")
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

func signHomeJWT(key *rsa.PrivateKey, claims homeJWTClaims) (string, error) {
	headerRaw, errHeader := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if errHeader != nil {
		return "", errHeader
	}
	payloadRaw, errPayload := json.Marshal(claims)
	if errPayload != nil {
		return "", errPayload
	}
	encoder := base64.RawURLEncoding
	signingInput := encoder.EncodeToString(headerRaw) + "." + encoder.EncodeToString(payloadRaw)
	sum := sha256.Sum256([]byte(signingInput))
	signature, errSign := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if errSign != nil {
		return "", errSign
	}
	return signingInput + "." + encoder.EncodeToString(signature), nil
}
