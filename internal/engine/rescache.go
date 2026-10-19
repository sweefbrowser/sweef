package engine

// rescache.go — the code cache and URL helpers of the cache layer. The HTTP cache itself is in httpcache_*.go.
//
//   - codeCache: compiled QuickJS bytecode keyed by the SHA-256 of the
//     script source, so a framework bundle is parsed once per process, the
//     way V8's code cache works. Execution still happens on every page.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"modernc.org/quickjs"
)

// versionedURL reports whether the URL carries a version or content hash
// (app.3f9a1c7e.js, logo-9b2e4d1a.png, lib.js?v=1.2.3): its content changes
// only when the URL does.
func versionedURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	name := u.Path
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	for _, seg := range strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '_' || r == '-' }) {
		hex, digit, letter := true, false, false
		for _, r := range seg {
			switch {
			case r >= '0' && r <= '9':
				digit = true
			case r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F':
				letter = true
			default:
				hex = false
				if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
					letter = true
				}
			}
		}
		if len(seg) >= 8 && hex && digit {
			return true
		}
		if len(seg) >= 16 && digit && letter {
			return true
		}
	}
	q := u.Query()
	for _, k := range []string{"v", "ver", "version", "rev", "hash", "h", "cb", "build"} {
		if q.Get(k) != "" {
			return true
		}
	}
	return false
}

// codeCache keeps compiled bytecode in memory and, when a cache directory is
// available, on disk, so a freshly started daemon does not re-parse the
// framework bundles it has seen before (like V8's on-disk code cache).
// Keys include the QuickJS version, so an engine upgrade never loads
// bytecode in an old format.
type codeCache struct {
	mu    sync.Mutex
	m     map[[32]byte][]byte
	bytes int
	max   int
	dir   string
	ext   string
}

func newCodeCache(dir string) *codeCache {
	return newCodeCacheAt(dir, "code-"+sanitizeVersion(quickjs.Version()), ".qjsbc")
}

// newCodeCacheAt is a code cache in dir/sub whose files end in ext (the V8 runtime keeps its own, apart from
// QuickJS's: the two formats must never meet).
func newCodeCacheAt(dir, sub, ext string) *codeCache {
	c := &codeCache{m: map[[32]byte][]byte{}, max: 32 << 20, ext: ext}
	if dir != "" {
		d := filepath.Join(dir, sub)
		if os.MkdirAll(d, 0o700) == nil {
			c.dir = d
		}
	}
	return c
}

func sanitizeVersion(v string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, v)
}

func (c *codeCache) path(key [32]byte) string {
	return filepath.Join(c.dir, hex.EncodeToString(key[:])+c.ext)
}

func (c *codeCache) get(key [32]byte) []byte {
	c.mu.Lock()
	bc := c.m[key]
	dir := c.dir
	c.mu.Unlock()
	if bc != nil || dir == "" {
		return bc
	}
	b, err := os.ReadFile(c.path(key))
	if err != nil || len(b) == 0 {
		return nil
	}
	c.mu.Lock()
	c.m[key] = b
	c.bytes += len(b)
	c.mu.Unlock()
	return b
}

func (c *codeCache) put(key [32]byte, bc []byte) {
	c.mu.Lock()
	if c.bytes+len(bc) > c.max {
		c.m = map[[32]byte][]byte{}
		c.bytes = 0
	}
	c.m[key] = bc
	c.bytes += len(bc)
	dir := c.dir
	c.mu.Unlock()
	if dir != "" {
		go func() {
			tmp := c.path(key) + ".tmp"
			if os.WriteFile(tmp, bc, 0o600) == nil {
				_ = os.Rename(tmp, c.path(key))
			}
		}()
	}
}

// forget drops an entry that failed to load (corrupt or stale file).
func (c *codeCache) forget(key [32]byte) {
	c.mu.Lock()
	delete(c.m, key)
	dir := c.dir
	c.mu.Unlock()
	if dir != "" {
		_ = os.Remove(c.path(key))
	}
}

func codeKey(src string) [32]byte { return sha256.Sum256([]byte(src)) }

// defaultCacheDir is $SWEEF_CACHE_DIR, else the user cache dir + /sweef.
func defaultCacheDir() string {
	if d := os.Getenv("SWEEF_CACHE_DIR"); d != "" {
		if d == "off" {
			return ""
		}
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "sweef")
	}
	return ""
}
