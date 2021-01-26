package frontendlogging

import (
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	sourcemap "github.com/go-sourcemap/sourcemap"

	"github.com/getsentry/sentry-go"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/setting"
)

type sourceMapLocation struct {
	dir      string
	path     string
	pluginID string
}

type sourceMap struct {
	consumer *sourcemap.Consumer
	pluginID string
}

type ReadSourceMapFn func(dir string, path string) ([]byte, error)

func ReadSourceMapFromFs(dir string, path string) ([]byte, error) {
	file, err := http.Dir(dir).Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := file.Close(); err != nil {
			logger.Error("Failed to close source map file.", "err", err)
		}
	}()
	return ioutil.ReadAll(file)
}

type SourceMapStore struct {
	cache         map[string]*sourceMap
	cfg           *setting.Cfg
	readSourceMap ReadSourceMapFn
	sync.Mutex
}

func NewSourceMapStore(cfg *setting.Cfg, readSourceMap ReadSourceMapFn) *SourceMapStore {
	return &SourceMapStore{
		cache:         make(map[string]*sourceMap),
		cfg:           cfg,
		readSourceMap: readSourceMap,
	}
}

func (store *SourceMapStore) guessSourceMapLocation(sourceURL string) (*sourceMapLocation, error) {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(u.Path, "/public/build/") {
		return &sourceMapLocation{
			dir:      store.cfg.StaticRootPath,
			path:     filepath.Join("build", u.Path[len("/public/build/"):]) + ".map",
			pluginID: "",
		}, nil
	} else if strings.HasPrefix(u.Path, "/public/plugins/") {
		for _, route := range plugins.StaticRoutes {
			pluginPrefix := filepath.Join("/public/plugins/", route.PluginId)
			if strings.HasPrefix(u.Path, pluginPrefix) {
				return &sourceMapLocation{
					dir:      route.Directory,
					path:     u.Path[len(pluginPrefix):] + ".map",
					pluginID: route.PluginId,
				}, nil
			}
		}
	}
	return nil, nil
}

func (store *SourceMapStore) getSourceMap(sourceURL string) (*sourceMap, error) {
	store.Lock()
	defer store.Unlock()

	if smap, ok := store.cache[sourceURL]; ok {
		return smap, nil
	}
	sourceMapLocation, err := store.guessSourceMapLocation(sourceURL)
	if err != nil {
		return nil, err
	}
	if sourceMapLocation == nil {
		// Cache nil value for sourceURL, since we want to flag that we couldn't guess the map location and not try again
		store.cache[sourceURL] = nil
		return nil, nil
	}
	path := sourceMapLocation.path
	if strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = filepath.Clean(path)
	b, err := store.readSourceMap(sourceMapLocation.dir, path)
	if err != nil {
		if os.IsNotExist(err) {
			// Cache nil value for sourceURL, since we want to flag that it wasn't found in the filesystem and not try again
			store.cache[sourceURL] = nil
			return nil, nil
		}
		return nil, err
	}

	consumer, err := sourcemap.Parse(sourceURL+".map", b)
	if err != nil {
		return nil, err
	}
	smap := &sourceMap{
		consumer: consumer,
		pluginID: sourceMapLocation.pluginID,
	}
	store.cache[sourceURL] = smap
	return smap, nil
}

func (store *SourceMapStore) resolveSourceLocation(frame sentry.Frame) (*sentry.Frame, error) {
	smap, err := store.getSourceMap(frame.Filename)
	if err != nil {
		return nil, err
	}
	if smap == nil {
		return nil, nil
	}
	file, function, line, col, ok := smap.consumer.Source(frame.Lineno, frame.Colno)
	if !ok {
		return nil, nil
	}
	if len(function) == 0 {
		function = "?"
	}
	module := "core"
	if len(smap.pluginID) > 0 {
		module = smap.pluginID
	}
	return &sentry.Frame{
		Filename: file,
		Lineno:   line,
		Colno:    col,
		Function: function,
		Module:   module,
	}, nil
}
