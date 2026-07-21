package userapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	defaultSessionTTL = 24 * time.Hour
	randomTokenBytes  = 32
	maxPasswordBytes  = 72
	userJWTAlgorithm  = "RS256"
	userJWTType       = "JWT"
	userJWTClaimType  = "user"
)

var errPasswordTooLong = errors.New("password exceeds the 72-byte bcrypt limit")

type userJWTHeader struct {
	Algorithm string `json:"alg"`
	Type      string `json:"typ"`
}

type userJWTClaims struct {
	Subject        string `json:"sub,omitempty"`
	UserID         uint   `json:"user_id"`
	SessionVersion uint64 `json:"session_version,omitempty"`
	IssuedAt       int64  `json:"iat"`
	ExpiresAt      int64  `json:"exp"`
	TokenType      string `json:"typ"`
}

func (h *Handler) createBearerToken(ctx context.Context, userID uint, sessionVersion uint64, ttl time.Duration) (string, time.Time, error) {
	if userID == 0 {
		return "", time.Time{}, fmt.Errorf("user id is required")
	}
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	key, errKey := h.userJWTSigningKey(ctx)
	if errKey != nil {
		return "", time.Time{}, errKey
	}
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	claims := userJWTClaims{
		Subject:        strconv.FormatUint(uint64(userID), 10),
		UserID:         userID,
		SessionVersion: sessionVersion,
		IssuedAt:       now.Unix(),
		ExpiresAt:      expiresAt.Unix(),
		TokenType:      userJWTClaimType,
	}
	token, errToken := signUserJWT(key, claims)
	if errToken != nil {
		return "", time.Time{}, errToken
	}
	return token, expiresAt, nil
}

func (h *Handler) bearerTokenClaims(ctx context.Context, token string) (userJWTClaims, error) {
	key, errKey := h.userJWTPublicKey(ctx)
	if errKey != nil {
		return userJWTClaims{}, errKey
	}
	return verifyUserJWT(key, token)
}

func (h *Handler) userJWTSigningKey(ctx context.Context) (*rsa.PrivateKey, error) {
	if h == nil || h.repo == nil {
		return nil, fmt.Errorf("user api repository is required")
	}
	_, key, errKey := h.repo.ClusterCAKeyPair(ctx)
	if errKey != nil {
		return nil, errKey
	}
	if key == nil {
		return nil, fmt.Errorf("cluster ca private key is required")
	}
	return key, nil
}

func (h *Handler) userJWTPublicKey(ctx context.Context) (*rsa.PublicKey, error) {
	if h == nil || h.repo == nil {
		return nil, fmt.Errorf("user api repository is required")
	}
	cert, _, errKey := h.repo.ClusterCAKeyPair(ctx)
	if errKey != nil {
		return nil, errKey
	}
	if cert == nil {
		return nil, fmt.Errorf("cluster ca certificate is required")
	}
	key, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || key == nil {
		return nil, fmt.Errorf("cluster ca public key is not rsa")
	}
	return key, nil
}

func signUserJWT(key *rsa.PrivateKey, claims userJWTClaims) (string, error) {
	if key == nil {
		return "", fmt.Errorf("jwt signing key is required")
	}
	header := userJWTHeader{Algorithm: userJWTAlgorithm, Type: userJWTType}
	headerData, errMarshalHeader := json.Marshal(header)
	if errMarshalHeader != nil {
		return "", errMarshalHeader
	}
	claimsData, errMarshalClaims := json.Marshal(claims)
	if errMarshalClaims != nil {
		return "", errMarshalClaims
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerData) + "." + base64.RawURLEncoding.EncodeToString(claimsData)
	sum := sha256.Sum256([]byte(signingInput))
	signature, errSign := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if errSign != nil {
		return "", errSign
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func verifyUserJWT(key *rsa.PublicKey, token string) (userJWTClaims, error) {
	if key == nil {
		return userJWTClaims{}, fmt.Errorf("jwt public key is required")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return userJWTClaims{}, fmt.Errorf("jwt token is required")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return userJWTClaims{}, fmt.Errorf("invalid jwt token")
	}
	headerData, errHeader := base64.RawURLEncoding.DecodeString(parts[0])
	if errHeader != nil {
		return userJWTClaims{}, fmt.Errorf("invalid jwt header")
	}
	var header userJWTHeader
	if errUnmarshalHeader := json.Unmarshal(headerData, &header); errUnmarshalHeader != nil {
		return userJWTClaims{}, fmt.Errorf("invalid jwt header")
	}
	if header.Algorithm != userJWTAlgorithm || header.Type != userJWTType {
		return userJWTClaims{}, fmt.Errorf("invalid jwt header")
	}
	signature, errDecodeSignature := base64.RawURLEncoding.DecodeString(parts[2])
	if errDecodeSignature != nil {
		return userJWTClaims{}, fmt.Errorf("invalid jwt signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if errVerifySignature := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], signature); errVerifySignature != nil {
		return userJWTClaims{}, fmt.Errorf("invalid jwt signature")
	}
	claimsData, errClaims := base64.RawURLEncoding.DecodeString(parts[1])
	if errClaims != nil {
		return userJWTClaims{}, fmt.Errorf("invalid jwt claims")
	}
	var claims userJWTClaims
	if errUnmarshalClaims := json.Unmarshal(claimsData, &claims); errUnmarshalClaims != nil {
		return userJWTClaims{}, fmt.Errorf("invalid jwt claims")
	}
	if claims.TokenType != userJWTClaimType {
		return userJWTClaims{}, fmt.Errorf("invalid jwt claims")
	}
	now := time.Now().UTC()
	if claims.ExpiresAt <= 0 || !time.Unix(claims.ExpiresAt, 0).After(now) {
		return userJWTClaims{}, fmt.Errorf("jwt token is expired")
	}
	if claims.IssuedAt > now.Add(time.Minute).Unix() {
		return userJWTClaims{}, fmt.Errorf("invalid jwt claims")
	}
	userID := claims.UserID
	if userID == 0 && strings.TrimSpace(claims.Subject) != "" {
		parsed, errParse := strconv.ParseUint(strings.TrimSpace(claims.Subject), 10, 64)
		if errParse != nil {
			return userJWTClaims{}, fmt.Errorf("invalid jwt subject")
		}
		userID = uint(parsed)
	}
	if userID == 0 {
		return userJWTClaims{}, fmt.Errorf("user id is required")
	}
	claims.UserID = userID
	return claims, nil
}

func randomToken(size int) (string, error) {
	if size <= 0 {
		size = randomTokenBytes
	}
	buf := make([]byte, size)
	if _, errRead := rand.Read(buf); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashPassword(password string) (string, error) {
	if password == "" {
		return "", fmt.Errorf("password is required")
	}
	if len([]byte(password)) > maxPasswordBytes {
		return "", errPasswordTooLong
	}
	hashed, errHash := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if errHash != nil {
		return "", errHash
	}
	return string(hashed), nil
}

func passwordMatches(stored string, password string) bool {
	if stored == "" || password == "" {
		return false
	}
	if isBcryptHash(stored) {
		return bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)) == nil
	}
	if len(stored) != len(password) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(stored), []byte(password)) == 1
}

func isBcryptHash(value string) bool {
	return strings.HasPrefix(value, "$2a$") ||
		strings.HasPrefix(value, "$2b$") ||
		strings.HasPrefix(value, "$2x$") ||
		strings.HasPrefix(value, "$2y$")
}
