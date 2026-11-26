package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// catalogUpdater serializes source changes with publication, not network I/O.
// Cancellation saves work; the generation check also protects against readers
// that finish after cancellation (including local file reads).
type catalogUpdater struct {
	mu         sync.Mutex
	source     string
	generation uint64
	cancel     context.CancelFunc
	ctx        context.Context
	parentDone <-chan struct{}
	fetch      func(context.Context, string) ([]byte, error)
	publish    func([]byte) ([]string, error)
}

func (u *catalogUpdater) configure(ctx context.Context, source string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cancel != nil && u.source == source && u.parentDone == ctx.Done() && u.ctx.Err() == nil {
		return
	}
	if u.cancel != nil {
		u.cancel()
	}
	u.generation++
	u.source = source
	refreshCtx, cancel := context.WithCancel(ctx)
	u.cancel = cancel
	u.ctx = refreshCtx
	u.parentDone = ctx.Done()
	generation := u.generation
	go func() {
		u.refresh(refreshCtx, source, generation)
		ticker := time.NewTicker(modelsRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-ticker.C:
				u.refresh(refreshCtx, source, generation)
			}
		}
	}()
}

func (u *catalogUpdater) refresh(ctx context.Context, source string, generation uint64) {
	data, errFetch := u.fetch(ctx, source)
	if errFetch != nil {
		if ctx.Err() == nil {
			log.Warn("model catalog refresh failed; keeping last valid catalog")
		}
		return
	}
	u.mu.Lock()
	if generation != u.generation || ctx.Err() != nil {
		u.mu.Unlock()
		return
	}
	changed, errPublish := u.publish(data)
	u.mu.Unlock()
	if errPublish != nil {
		log.Warn("model catalog rejected; keeping last valid catalog")
		return
	}
	notifyModelRefresh(changed)
}

func readCatalogSource(ctx context.Context, source string) ([]byte, error) {
	var reader io.ReadCloser
	defer func() {
		if reader != nil {
			if errClose := reader.Close(); errClose != nil {
				log.Debug("catalog reader close failed")
			}
		}
	}()
	if filepath.IsAbs(source) {
		// Reject special files before opening: opening a FIFO can itself block.
		info, errStat := os.Stat(source)
		if errStat != nil {
			return nil, errStat
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("catalog source must be a regular file")
		}
		file, errOpen := os.Open(source)
		if errOpen != nil {
			return nil, errOpen
		}
		reader = file
		info, errStat = file.Stat()
		if errStat != nil {
			return nil, errStat
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("catalog source must be a regular file")
		}
	} else {
		reqCtx, cancel := context.WithTimeout(ctx, modelsFetchTimeout)
		defer cancel()
		req, errRequest := http.NewRequestWithContext(reqCtx, http.MethodGet, source, nil)
		if errRequest != nil {
			return nil, errRequest
		}
		resp, errDo := http.DefaultClient.Do(req)
		if errDo != nil {
			return nil, errDo
		}
		reader = resp.Body
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("catalog HTTP status %d", resp.StatusCode)
		}
	}
	data, errRead := io.ReadAll(io.LimitReader(reader, maxCodexClientModelsSize+1))
	if errRead != nil {
		return nil, errRead
	}
	if len(data) > maxCodexClientModelsSize {
		return nil, fmt.Errorf("catalog exceeds size limit")
	}
	return data, nil
}

func catalogFetcher(urls func() []string, validate func([]byte) error) func(context.Context, string) ([]byte, error) {
	return func(ctx context.Context, source string) ([]byte, error) {
		sources := urls()
		if source != "" {
			sources = []string{source}
		}
		for _, candidate := range sources {
			data, errRead := readCatalogSource(ctx, candidate)
			if errRead == nil && validate(data) == nil {
				return data, nil
			}
			if ctx.Err() != nil {
				break
			}
		}
		return nil, fmt.Errorf("no valid model catalog source")
	}
}

func validateCatalogBytes(data []byte) error {
	var parsed staticModelsJSON
	if errUnmarshal := json.Unmarshal(data, &parsed); errUnmarshal != nil {
		return errUnmarshal
	}
	return validateModelsCatalog(&parsed)
}

func publishCatalogBytes(data []byte) ([]string, error) {
	var parsed staticModelsJSON
	if errUnmarshal := json.Unmarshal(data, &parsed); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if errValidate := validateModelsCatalog(&parsed); errValidate != nil {
		return nil, errValidate
	}
	old := getModels()
	if old != nil {
		if len(parsed.Devin) == 0 {
			parsed.Devin = old.Devin
		}
		if len(parsed.Meta) == 0 {
			parsed.Meta = old.Meta
		}
		if len(parsed.Copilot) == 0 {
			parsed.Copilot = old.Copilot
		}
	}
	changed := detectChangedProviders(old, &parsed)
	modelsCatalogStore.mu.Lock()
	modelsCatalogStore.data = &parsed
	modelsCatalogStore.mu.Unlock()
	return changed, nil
}

var generalCatalogUpdater = &catalogUpdater{
	fetch:   catalogFetcher(func() []string { return modelsURLs }, validateCatalogBytes),
	publish: publishCatalogBytes,
}
var codexCatalogUpdater = &catalogUpdater{
	fetch: catalogFetcher(func() []string { return codexClientModelsURLs }, ValidateCodexClientModelsJSON),
	publish: func(data []byte) ([]string, error) {
		_, errLoad := loadCodexClientModelsFromBytes(data, "catalog")
		return nil, errLoad
	},
}
var devinCatalogUpdater = &catalogUpdater{
	fetch: catalogFetcher(func() []string { return devinModelsURLs }, validateDevinCatalogBytes),
	publish: func(data []byte) ([]string, error) {
		changed, errLoad := loadDevinModelsFromBytes(data, "catalog")
		if errLoad == nil && changed {
			return []string{"devin"}, nil
		}
		return nil, errLoad
	},
}

var catalogRuntime struct {
	sync.Mutex
	ctx context.Context
}

// StartModelCatalogUpdaters binds process-wide readers to the Home lifetime.
func StartModelCatalogUpdaters(ctx context.Context, sources CatalogSources) {
	catalogRuntime.Lock()
	defer catalogRuntime.Unlock()
	catalogRuntime.ctx = ctx
	configureCatalogs(ctx, sources)
}

// UpdateModelCatalogSources updates an already-started process policy on reload.
// Updates before startup or after shutdown do not start readers.
func UpdateModelCatalogSources(sources CatalogSources) {
	catalogRuntime.Lock()
	defer catalogRuntime.Unlock()
	if catalogRuntime.ctx != nil && catalogRuntime.ctx.Err() == nil {
		configureCatalogs(catalogRuntime.ctx, sources)
	}
}

func configureCatalogs(ctx context.Context, sources CatalogSources) {
	if errValidate := sources.Validate(); errValidate != nil {
		log.WithError(errValidate).Warn("invalid catalog sources")
		return
	}
	generalCatalogUpdater.configure(ctx, sources.Catalog)
	codexCatalogUpdater.configure(ctx, sources.CodexCatalog)
	devinCatalogUpdater.configure(ctx, sources.DevinCatalog)
}

func validateDevinCatalogBytes(data []byte) error {
	_, errValidate := ValidateDevinModelsJSON(data)
	return errValidate
}
