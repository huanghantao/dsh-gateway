package v1

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"reflect"
	"sync"
	"time"

	"github.com/huanghantao/dsh-gateway/internal/atomicfile"
	"github.com/huanghantao/dsh-gateway/internal/harness"
	"github.com/huanghantao/dsh-gateway/internal/logx"
)

// configCache holds the most recently observed configuration options.
//
// ACP only reveals the model catalog when a session is created or resumed; there
// is no standalone "list models" call. Caching the last observation lets the
// model picker work before the operator has opened a conversation, which is
// exactly when they need it.
//
// In memory that only covers a running gateway, and a restart emptied it: until
// some session attached again, `GET /models` answered with an empty catalog, so
// every picker in the app had nothing to offer but "Leave unchanged" and no
// default it could resolve. The last observation is therefore also written next
// to the rest of the gateway's state and read back at startup.
//
// A remembered catalog can be stale — the operator may have removed a provider
// while the gateway was down. That is not a correctness problem: it is a list of
// values to offer, the harness rejects a value it no longer knows with a message
// the app shows, and the next session that attaches replaces the file.
type configCache struct {
	mu      sync.RWMutex
	options []harness.ConfigOption

	// path is where the catalog is kept between runs. Empty means memory only,
	// which is what a test with no state directory gets.
	path   string
	logger *logx.Logger
}

// open reads a remembered catalog, if there is one.
//
// Every failure here is a warning and an empty cache: a catalog is a
// convenience, and a gateway that refuses to start because a cache file is
// unreadable would be trading a working agent for a picker.
func (c *configCache) open(path string, logger *logx.Logger) {
	c.path = path
	c.logger = logger

	data, err := os.ReadFile(path) //nolint:gosec // a cache file under the operator's state directory
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			logger.Warn("could not read the remembered model catalog", "path", path, "error", err.Error())
		}
		return
	}

	var doc catalogDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		logger.Warn("ignoring an unreadable model catalog", "path", path, "error", err.Error())
		return
	}
	if doc.Version != catalogVersion {
		logger.Warn("ignoring a model catalog written in another format",
			"path", path, "version", doc.Version, "want", catalogVersion)
		return
	}
	c.options = doc.options()
}

func (c *configCache) set(options []harness.ConfigOption) {
	if len(options) == 0 {
		return
	}

	c.mu.Lock()
	unchanged := reflect.DeepEqual(c.options, options)
	if !unchanged {
		c.options = options
	}
	path, logger := c.path, c.logger
	c.mu.Unlock()

	if unchanged || path == "" {
		return
	}
	c.persist(path, logger, options)
}

// persist writes the observation. The lock is not held: this is disk I/O, and a
// picker waiting on it would be waiting on a file it does not read.
func (c *configCache) persist(path string, logger *logx.Logger, options []harness.ConfigOption) {
	doc := catalogDocument{Version: catalogVersion, SavedAt: time.Now().UTC(), Catalog: catalogOf(options)}
	data, err := json.Marshal(doc)
	if err != nil {
		logger.Warn("could not encode the model catalog", "error", err.Error())
		return
	}
	if err := atomicfile.WriteFile(path, data, 0o600); err != nil {
		logger.Warn("could not remember the model catalog", "path", path, "error", err.Error())
	}
}

func (c *configCache) get() []harness.ConfigOption {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.options
}

// catalogVersion is the shape of the file. A gateway that reads a version it
// does not know ignores it rather than guessing: nothing here is worth a
// migration, because the next attached session rewrites it.
const catalogVersion = 1

// catalogDocument is the file on disk.
//
// It is a separate type from harness.ConfigOption on purpose: the file is this
// package's business, and a field added to the harness's option type should not
// silently appear in a file that older builds have to keep reading.
type catalogDocument struct {
	Version int             `json:"version"`
	SavedAt time.Time       `json:"savedAt"`
	Catalog []catalogOption `json:"catalog"`
}

type catalogOption struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Current string               `json:"current"`
	Options []catalogOptionValue `json:"options"`
}

type catalogOptionValue struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Group       string `json:"group"`
}

func catalogOf(options []harness.ConfigOption) []catalogOption {
	out := make([]catalogOption, 0, len(options))
	for _, o := range options {
		values := make([]catalogOptionValue, 0, len(o.Options))
		for _, v := range o.Options {
			values = append(values, catalogOptionValue{
				ID:          v.ID,
				Name:        v.Name,
				Description: v.Description,
				Group:       v.Group,
			})
		}
		out = append(out, catalogOption{ID: o.ID, Name: o.Name, Current: o.Current, Options: values})
	}
	return out
}

func (d catalogDocument) options() []harness.ConfigOption {
	out := make([]harness.ConfigOption, 0, len(d.Catalog))
	for _, o := range d.Catalog {
		values := make([]harness.ConfigOptionValue, 0, len(o.Options))
		for _, v := range o.Options {
			values = append(values, harness.ConfigOptionValue{
				ID:          v.ID,
				Name:        v.Name,
				Description: v.Description,
				Group:       v.Group,
			})
		}
		out = append(out, harness.ConfigOption{ID: o.ID, Name: o.Name, Current: o.Current, Options: values})
	}
	return out
}
