package management

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ImportVertexCredential handles import vertex credential.
func (h *Handler) ImportVertexCredential(c *gin.Context) {
	// Validate request inputs before mutating persisted state.
	fileHeader, errFormFile := c.FormFile("file")
	if errFormFile != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
		return
	}

	file, errOpen := fileHeader.Open()
	if errOpen != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to read file: %v", errOpen)})
		return
	}
	data, errRead := io.ReadAll(file)
	if errClose := file.Close(); errClose != nil && errRead == nil {
		errRead = errClose
	}
	if errRead != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to read file: %v", errRead)})
		return
	}

	var serviceAccount map[string]any
	if errUnmarshal := json.Unmarshal(data, &serviceAccount); errUnmarshal != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json", "message": errUnmarshal.Error()})
		return
	}
	normalizedSA, errNormalize := normalizeVertexServiceAccountMap(serviceAccount)
	if errNormalize != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid service account", "message": errNormalize.Error()})
		return
	}
	serviceAccount = normalizedSA

	projectID := strings.TrimSpace(stringFromAny(serviceAccount["project_id"]))
	if projectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_id missing"})
		return
	}
	email := strings.TrimSpace(stringFromAny(serviceAccount["client_email"]))
	location := strings.TrimSpace(c.PostForm("location"))
	if location == "" {
		location = strings.TrimSpace(c.Query("location"))
	}
	if location == "" {
		location = "global"
	}

	label := labelForVertex(projectID, email)
	payload := map[string]any{
		"service_account": serviceAccount,
		"project_id":      projectID,
		"email":           email,
		"location":        location,
		"type":            "vertex",
		"label":           label,
	}
	rawPayload, errMarshal := json.MarshalIndent(payload, "", "  ")
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save_failed", "message": errMarshal.Error()})
		return
	}
	rawPayload = append(rawPayload, '\n')

	fileName := fmt.Sprintf("vertex-%s.json", sanitizeVertexFilePart(projectID))
	name, errStore := h.storeOAuthPayload(c, rawPayload, fileName)
	if errStore != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save_failed", "message": errStore.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"auth-file":  name,
		"project_id": projectID,
		"email":      email,
		"location":   location,
	})
}

// normalizeVertexServiceAccountMap normalizes a vertex service account map.
func normalizeVertexServiceAccountMap(serviceAccount map[string]any) (map[string]any, error) {
	if serviceAccount == nil {
		return nil, fmt.Errorf("service account payload is empty")
	}
	privateKey, _ := serviceAccount["private_key"].(string)
	if strings.TrimSpace(privateKey) == "" {
		return nil, fmt.Errorf("service account missing private_key")
	}
	normalized, errNormalize := sanitizeVertexPrivateKey(privateKey)
	if errNormalize != nil {
		return nil, errNormalize
	}
	clone := make(map[string]any, len(serviceAccount))
	for key, value := range serviceAccount {
		clone[key] = value
	}
	clone["private_key"] = normalized
	return clone, nil
}

// sanitizeVertexPrivateKey sanitizes a vertex private key.
func sanitizeVertexPrivateKey(raw string) (string, error) {
	// Validate request inputs before mutating persisted state.
	privateKey := strings.ReplaceAll(raw, "\r\n", "\n")
	privateKey = strings.ReplaceAll(privateKey, "\r", "\n")
	privateKey = stripANSIEscape(privateKey)
	privateKey = strings.ToValidUTF8(privateKey, "")
	privateKey = strings.TrimSpace(privateKey)

	normalized := privateKey
	if block, _ := pem.Decode([]byte(privateKey)); block == nil {
		reconstructed, errRebuild := rebuildVertexPEM(privateKey)
		if errRebuild != nil {
			return "", fmt.Errorf("private_key is not valid pem: %w", errRebuild)
		}
		normalized = reconstructed
	}

	block, _ := pem.Decode([]byte(normalized))
	if block == nil {
		return "", fmt.Errorf("private_key pem decode failed")
	}
	rsaBlock, errRSA := ensureVertexRSAPrivateKey(block)
	if errRSA != nil {
		return "", errRSA
	}
	return string(pem.EncodeToMemory(rsaBlock)), nil
}

// ensureVertexRSAPrivateKey ensures a vertex rsa private key.
func ensureVertexRSAPrivateKey(block *pem.Block) (*pem.Block, error) {
	// Validate request inputs before mutating persisted state.
	if block == nil {
		return nil, fmt.Errorf("pem block is nil")
	}
	if block.Type == "RSA PRIVATE KEY" {
		if _, errParse := x509.ParsePKCS1PrivateKey(block.Bytes); errParse != nil {
			return nil, fmt.Errorf("private_key invalid rsa: %w", errParse)
		}
		return block, nil
	}
	if block.Type == "PRIVATE KEY" {
		key, errParse := x509.ParsePKCS8PrivateKey(block.Bytes)
		if errParse != nil {
			return nil, fmt.Errorf("private_key invalid pkcs8: %w", errParse)
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private_key is not an RSA key")
		}
		return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}, nil
	}
	if rsaKey, errParse := x509.ParsePKCS1PrivateKey(block.Bytes); errParse == nil {
		return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}, nil
	}
	if key, errParse := x509.ParsePKCS8PrivateKey(block.Bytes); errParse == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}, nil
		}
	}
	return nil, fmt.Errorf("private_key uses unsupported format")
}

// rebuildVertexPEM rebuilds a vertex pem.
func rebuildVertexPEM(raw string) (string, error) {
	// Validate request inputs before mutating persisted state.
	kind := "PRIVATE KEY"
	if strings.Contains(raw, "RSA PRIVATE KEY") {
		kind = "RSA PRIVATE KEY"
	}
	header := "-----BEGIN " + kind + "-----"
	footer := "-----END " + kind + "-----"
	start := strings.Index(raw, header)
	end := strings.Index(raw, footer)
	if start < 0 || end <= start {
		return "", fmt.Errorf("missing pem markers")
	}
	body := raw[start+len(header) : end]
	payload := filterVertexBase64(body)
	if payload == "" {
		return "", fmt.Errorf("private_key base64 payload empty")
	}
	der, errDecode := base64.StdEncoding.DecodeString(payload)
	if errDecode != nil {
		return "", fmt.Errorf("private_key base64 decode failed: %w", errDecode)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der})), nil
}

// filterVertexBase64 handles a filter vertex base 64.
func filterVertexBase64(value string) string {
	var builder strings.Builder
	for _, char := range value {
		switch {
		case char >= 'A' && char <= 'Z':
			builder.WriteRune(char)
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '+' || char == '/' || char == '=':
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

// stripANSIEscape handles a strip ansi escape.
func stripANSIEscape(value string) string {
	var builder strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != 0x1b || i+1 >= len(value) || value[i+1] != '[' {
			builder.WriteByte(value[i])
			continue
		}
		i += 2
		for i < len(value) {
			ch := value[i]
			if ch >= 0x40 && ch <= 0x7e {
				break
			}
			i++
		}
	}
	return builder.String()
}

// sanitizeVertexFilePart sanitizes a vertex file part.
func sanitizeVertexFilePart(value string) string {
	out := strings.TrimSpace(value)
	replacers := []string{"/", "_", "\\", "_", ":", "_", " ", "-"}
	for i := 0; i < len(replacers); i += 2 {
		out = strings.ReplaceAll(out, replacers[i], replacers[i+1])
	}
	if out == "" {
		return "vertex"
	}
	return out
}

// labelForVertex returns a label for vertex.
func labelForVertex(projectID, email string) string {
	projectID = strings.TrimSpace(projectID)
	email = strings.TrimSpace(email)
	if projectID != "" && email != "" {
		return fmt.Sprintf("%s (%s)", projectID, email)
	}
	if projectID != "" {
		return projectID
	}
	if email != "" {
		return email
	}
	return "vertex"
}
