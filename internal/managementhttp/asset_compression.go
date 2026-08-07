package managementhttp

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// compressibleAssetExtensions lists the control panel asset types where gzip
// pays for itself. Fonts and images arrive already compressed, so gzipping them
// only burns CPU and can make the payload larger.
var compressibleAssetExtensions = map[string]struct{}{
	".css":  {},
	".html": {},
	".js":   {},
	".json": {},
	".map":  {},
	".svg":  {},
	".txt":  {},
}

// minCompressibleAssetSize skips payloads too small for the gzip container to
// earn back its own overhead.
const minCompressibleAssetSize = 1024

// maxCompressedAssetEntries bounds the cache. The shipped panel has roughly a
// dozen compressible assets; the cache only grows past that when a disk
// override (MANAGEMENT_STATIC_PATH) is edited repeatedly during development.
const maxCompressedAssetEntries = 64

var (
	compressedAssetMu    sync.RWMutex
	compressedAssetCache = make(map[string]([]byte))
)

// clientAcceptsGzip reports whether the caller opted into gzip. It honours an
// explicit "gzip;q=0", which is how a client asks for the identity encoding
// while still listing gzip.
func clientAcceptsGzip(r *http.Request) bool {
	if r == nil {
		return false
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "gzip") {
			continue
		}
		for _, param := range fields[1:] {
			param = strings.TrimSpace(param)
			if !strings.HasPrefix(strings.ToLower(param), "q=") {
				continue
			}
			if q, err := strconv.ParseFloat(param[2:], 64); err == nil && q == 0 {
				return false
			}
		}
		return true
	}
	return false
}

// shouldCompressAsset decides whether this request and this asset are worth
// compressing, before anything is read off disk.
func shouldCompressAsset(r *http.Request, name string, info fs.FileInfo) bool {
	if info == nil || info.Size() < minCompressibleAssetSize {
		return false
	}
	if _, ok := compressibleAssetExtensions[strings.ToLower(path.Ext(name))]; !ok {
		return false
	}
	return clientAcceptsGzip(r)
}

// compressedAssetKey identifies one compressed representation. Panel assets
// carry a content hash in their name, but the HTML entries do not and a disk
// override can change underneath us, so size and mtime are part of the key.
func compressedAssetKey(name string, info fs.FileInfo) string {
	return name + "|" + strconv.FormatInt(info.Size(), 10) + "|" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
}

// gzipAsset returns the gzipped representation of an asset, compressing it on
// first use and serving every later request from memory. The asset set is fixed
// and immutable in a running server, so paying for the best compression ratio
// once is cheaper than re-deflating on every request.
//
// Reading the file consumes it, so the bytes that were read are handed back
// alongside the result: when compression is skipped or fails, the caller must
// serve those bytes rather than re-reading an exhausted reader. A nil raw slice
// together with ok=false means the asset could not be read at all.
func gzipAsset(name string, info fs.FileInfo, file fs.File) (compressed []byte, raw []byte, ok bool) {
	key := compressedAssetKey(name, info)

	compressedAssetMu.RLock()
	cached, hit := compressedAssetCache[key]
	compressedAssetMu.RUnlock()
	if hit {
		return cached, nil, true
	}

	raw, errRead := io.ReadAll(file)
	if errRead != nil {
		return nil, nil, false
	}

	var buf bytes.Buffer
	writer, errWriter := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if errWriter != nil {
		return nil, raw, false
	}
	if _, errWrite := writer.Write(raw); errWrite != nil {
		return nil, raw, false
	}
	if errClose := writer.Close(); errClose != nil {
		return nil, raw, false
	}
	if buf.Len() >= len(raw) {
		return nil, raw, false
	}

	compressed = buf.Bytes()
	compressedAssetMu.Lock()
	if len(compressedAssetCache) >= maxCompressedAssetEntries {
		compressedAssetCache = make(map[string]([]byte), maxCompressedAssetEntries)
	}
	compressedAssetCache[key] = compressed
	compressedAssetMu.Unlock()

	return compressed, raw, true
}

// compressedAssetReader adapts the cached bytes to what http.ServeContent
// wants, so range requests and conditional revalidation keep working against
// the encoded representation.
func compressedAssetReader(compressed []byte) io.ReadSeeker {
	return bytes.NewReader(compressed)
}
