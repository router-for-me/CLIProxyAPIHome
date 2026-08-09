package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

const cpaNodeNameMaxRequestBodySize int64 = 4 << 10

const cpaNodeJSONWhitespace = " \t\r\n"

type cpaNodeNameRequest struct {
	NodeName *string
	Present  bool
}

func decodeCPANodeNameRequest(c *gin.Context) (cpaNodeNameRequest, error) {
	request := cpaNodeNameRequest{}
	if c == nil || c.Request == nil {
		return request, fmt.Errorf("request body is unavailable")
	}
	if c.Request.Body == nil {
		return request, nil
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, cpaNodeNameMaxRequestBodySize)
	raw, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		return request, errRead
	}
	if !utf8.Valid(raw) {
		return request, fmt.Errorf("request body must be valid UTF-8")
	}
	trimmed := trimCPANodeJSONWhitespace(raw)
	if len(trimmed) == 0 {
		return request, nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return request, nil
	}
	if trimmed[0] != '{' {
		return request, fmt.Errorf("request body must be a JSON object")
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	var fields map[string]json.RawMessage
	if errDecode := decoder.Decode(&fields); errDecode != nil {
		return request, errDecode
	}
	offset := decoder.InputOffset()
	if offset < 0 || offset > int64(len(raw)) || len(trimCPANodeJSONWhitespace(raw[offset:])) != 0 {
		return request, fmt.Errorf("request body must contain one JSON value")
	}
	if fields == nil {
		return request, fmt.Errorf("request body must be a JSON object")
	}
	for field := range fields {
		if field != "node_name" {
			return request, fmt.Errorf("unknown field %q", field)
		}
	}
	rawNodeName, present := fields["node_name"]
	if !present {
		return request, nil
	}
	request.Present = true
	if bytes.Equal(trimCPANodeJSONWhitespace(rawNodeName), []byte("null")) {
		return request, nil
	}
	if errUnicode := validateCPANodeNameJSONUnicodeEscapes(rawNodeName); errUnicode != nil {
		return request, errUnicode
	}
	var nodeName string
	if errDecode := json.Unmarshal(rawNodeName, &nodeName); errDecode != nil {
		return request, fmt.Errorf("node_name must be a string or null: %w", errDecode)
	}
	request.NodeName = &nodeName
	return request, nil
}

func trimCPANodeJSONWhitespace(value []byte) []byte {
	return bytes.Trim(value, cpaNodeJSONWhitespace)
}

func validateCPANodeNameJSONUnicodeEscapes(value []byte) error {
	value = trimCPANodeJSONWhitespace(value)
	if len(value) < 2 || value[0] != '"' {
		return nil
	}
	for index := 1; index < len(value); {
		if value[index] == '"' {
			return nil
		}
		if value[index] != '\\' {
			_, size := utf8.DecodeRune(value[index:])
			index += size
			continue
		}
		if index+2 > len(value) {
			return fmt.Errorf("node_name contains an incomplete JSON escape")
		}
		if value[index+1] != 'u' {
			index += 2
			continue
		}
		codePoint, okCodePoint := decodeCPANodeNameJSONUnicodeEscape(value, index)
		if !okCodePoint {
			return fmt.Errorf("node_name contains an invalid Unicode escape")
		}
		index += 6
		switch {
		case codePoint >= 0xd800 && codePoint <= 0xdbff:
			lowSurrogate, okLowSurrogate := decodeCPANodeNameJSONUnicodeEscape(value, index)
			if !okLowSurrogate || lowSurrogate < 0xdc00 || lowSurrogate > 0xdfff {
				return fmt.Errorf("node_name contains an unpaired high surrogate escape")
			}
			index += 6
		case codePoint >= 0xdc00 && codePoint <= 0xdfff:
			return fmt.Errorf("node_name contains an unpaired low surrogate escape")
		}
	}
	return fmt.Errorf("node_name must be a JSON string")
}

func decodeCPANodeNameJSONUnicodeEscape(value []byte, offset int) (uint16, bool) {
	if offset < 0 || offset+6 > len(value) || value[offset] != '\\' || value[offset+1] != 'u' {
		return 0, false
	}
	var codePoint uint16
	for _, digit := range value[offset+2 : offset+6] {
		codePoint <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			codePoint |= uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			codePoint |= uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			codePoint |= uint16(digit-'A') + 10
		default:
			return 0, false
		}
	}
	return codePoint, true
}

func requestedCPANodeName(request cpaNodeNameRequest, required bool) (string, error) {
	if required && !request.Present {
		return "", fmt.Errorf("node_name is required")
	}
	if request.NodeName == nil {
		return "", nil
	}
	return cluster.NormalizeCPANodeName(*request.NodeName)
}

func respondCPANodeNameRequestError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		status = http.StatusRequestEntityTooLarge
	}
	respondError(c, status, "invalid_request", err)
}

// UpdateCPANodeName sets or clears the operator-managed name for a CPA node.
func (h *Handler) UpdateCPANodeName(c *gin.Context) {
	if c == nil {
		return
	}
	if h == nil || h.repo == nil {
		respondError(c, http.StatusServiceUnavailable, "cluster_unavailable", nil)
		return
	}
	request, errDecode := decodeCPANodeNameRequest(c)
	if errDecode != nil {
		respondCPANodeNameRequestError(c, errDecode)
		return
	}
	nodeName, errNodeName := requestedCPANodeName(request, true)
	if errNodeName != nil {
		if errors.Is(errNodeName, cluster.ErrInvalidCPANodeName) {
			respondError(c, http.StatusUnprocessableEntity, "cpa_node_name_invalid", errNodeName)
		} else {
			respondError(c, http.StatusBadRequest, "invalid_request", errNodeName)
		}
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	storedName, errUpdate := h.repo.UpdateCPANodeName(ctx, c.Param("node_id"), nodeName)
	if errUpdate != nil {
		switch {
		case errors.Is(errUpdate, cluster.ErrCPANodeNotFound):
			respondError(c, http.StatusNotFound, "cpa_node_not_found", errUpdate)
		case errors.Is(errUpdate, cluster.ErrInvalidCPANodeName):
			respondError(c, http.StatusUnprocessableEntity, "cpa_node_name_invalid", errUpdate)
		default:
			respondError(c, http.StatusInternalServerError, "cpa_node_name_update_failed", errUpdate)
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"node_id":   strings.TrimSpace(c.Param("node_id")),
		"node_name": emptyStringAsNil(storedName),
	})
}
