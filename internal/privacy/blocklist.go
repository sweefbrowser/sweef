// Package privacy implements Sweef's built-in tracker/ad blocking policy:
// a compact domain blocklist, first-party/third-party classification, HTTPS
// upgrade, referrer trimming, GPC/DNT headers and third-party cookie
// filtering. It is pure Go and independent of the engine; internal/app wires
// it to engine tabs.
package privacy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Compiled blocklist format ("SBL1"):
//
//	magic "SBL1" | uint32 count | uint32 blobLen | blob
//
// blob is the sorted, de-duplicated list of reversed domains
// ("com.doubleclick") joined by '\n'. Rules prefixed with '@' in the source
// are exceptions and stored with a leading '@' (sorted after the others by
// construction of the key). Loading builds an offset table (4 bytes/entry)
// and a Bloom filter (~10 bits/entry), so memory is roughly the blob plus
// 5.25 bytes per domain, and a lookup for a clean host is a few hash probes
// with no binary search.

var sblMagic = []byte("SBL1")

// Compile turns a text list into the compact binary form. Accepted lines:
// `example.com`, `||example.com^` (Adblock-style domain rule), `@@example.com`
// or `@@||example.com^` (exception), `# comment`, `! comment`, hosts-file
// lines `0.0.0.0 example.com`. Each rule covers the domain and all its
// subdomains.
func Compile(src string) []byte {
	rules := parseRules(src)
	keys := make([]string, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	blob := []byte(strings.Join(keys, "\n"))
	out := make([]byte, 0, 12+len(blob))
	out = append(out, sblMagic...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(keys)))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(blob)))
	return append(out, blob...)
}

func parseRules(src string) map[string]struct{} {
	rules := map[string]struct{}{}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '!' || line[0] == '[' {
			continue
		}
		exc := false
		if strings.HasPrefix(line, "@@") {
			exc, line = true, line[2:]
		}
		line = strings.TrimPrefix(line, "||")
		if i := strings.IndexAny(line, "^/$ \t"); i >= 0 {
			if f := strings.Fields(line); len(f) == 2 && (f[0] == "0.0.0.0" || f[0] == "127.0.0.1") {
				line = f[1] // hosts file
			} else {
				line = line[:i]
			}
		}
		line = strings.ToLower(strings.Trim(line, ".*"))
		if line == "" || !strings.Contains(line, ".") {
			continue
		}
		k := reverseDomain(line)
		if exc {
			k = "@" + k
		}
		rules[k] = struct{}{}
	}
	return rules
}

// reverseDomain turns a.b.com into com.b.a.
func reverseDomain(d string) string {
	parts := strings.Split(d, ".")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}

// Blocklist answers "is this host blocked" over the compiled form.
type Blocklist struct {
	loader func() ([]byte, error)
	once   sync.Once
	err    error

	blob   []byte
	offs   []uint32 // start offset of each entry in blob
	bloom  []uint64
	bloomN uint64
	count  int
	hasExc bool
	ready  atomic.Bool
}

// NewBlocklist creates a lazily loaded blocklist: loader runs on first
// lookup, never at construction (start-up costs nothing).
func NewBlocklist(loader func() ([]byte, error)) *Blocklist { return &Blocklist{loader: loader} }

// FromCompiled wraps already compiled data.
func FromCompiled(data []byte) *Blocklist {
	return NewBlocklist(func() ([]byte, error) { return data, nil })
}

// FromText compiles text lazily.
func FromText(src string) *Blocklist {
	return NewBlocklist(func() ([]byte, error) { return Compile(src), nil })
}

func (b *Blocklist) load() {
	b.once.Do(func() {
		if b.loader == nil {
			b.ready.Store(true)
			return
		}
		data, err := b.loader()
		if err != nil {
			b.err = err
			b.ready.Store(true)
			return
		}
		b.err = b.parse(data)
		b.ready.Store(true)
	})
}

// Loaded reports whether the list has been materialised.
func (b *Blocklist) Loaded() bool { return b.ready.Load() }

func (b *Blocklist) parse(data []byte) error {
	if len(data) < 12 || !bytes.Equal(data[:4], sblMagic) {
		return errors.New("privacy: bad blocklist")
	}
	n := binary.LittleEndian.Uint32(data[4:8])
	bl := binary.LittleEndian.Uint32(data[8:12])
	if int(bl) != len(data)-12 {
		return errors.New("privacy: truncated blocklist")
	}
	blob := data[12:]
	offs := make([]uint32, 0, n)
	if n > 0 {
		offs = append(offs, 0)
		for i, c := range blob {
			if c == '\n' {
				offs = append(offs, uint32(i+1))
			}
		}
	}
	if len(offs) != int(n) {
		return errors.New("privacy: blocklist count mismatch")
	}
	bits := uint64(n)*10 + 64
	bloom := make([]uint64, (bits+63)/64)
	b.bloomN = uint64(len(bloom)) * 64
	b.blob, b.offs, b.bloom, b.count = blob, offs, bloom, int(n)
	for i := range offs {
		key := b.entry(i)
		if len(key) > 0 && key[0] == '@' {
			b.hasExc = true
			key = key[1:]
		}
		b.bloomAdd(key)
	}
	return nil
}

func (b *Blocklist) entry(i int) []byte {
	s := b.offs[i]
	e := uint32(len(b.blob))
	if i+1 < len(b.offs) {
		e = b.offs[i+1] - 1
	}
	return b.blob[s:e]
}

func hash2(s string) (uint64, uint64) {
	h := fnv.New64a()
	h.Write([]byte(s))
	a := h.Sum64()
	return a, (a >> 33) | 1 | (a << 31)
}

func hash2b(s []byte) (uint64, uint64) {
	h := fnv.New64a()
	h.Write(s)
	a := h.Sum64()
	return a, (a >> 33) | 1 | (a << 31)
}

func (b *Blocklist) bloomAdd(key []byte) {
	h1, h2 := hash2b(key)
	for k := uint64(0); k < 7; k++ {
		bit := (h1 + k*h2) % b.bloomN
		b.bloom[bit>>6] |= 1 << (bit & 63)
	}
}

func (b *Blocklist) bloomMay(key string) bool {
	h1, h2 := hash2(key)
	for k := uint64(0); k < 7; k++ {
		bit := (h1 + k*h2) % b.bloomN
		if b.bloom[bit>>6]&(1<<(bit&63)) == 0 {
			return false
		}
	}
	return true
}

// has does an exact lookup of a reversed-domain key (optionally "@"-prefixed).
func (b *Blocklist) has(key string) bool {
	i := sort.Search(len(b.offs), func(i int) bool { return string(b.entry(i)) >= key })
	return i < len(b.offs) && string(b.entry(i)) == key
}

// Len is the number of rules (after load).
func (b *Blocklist) Len() int { b.load(); return b.count }

// Err returns the load error, if any.
func (b *Blocklist) Err() error { b.load(); return b.err }

// Match reports whether host (or a parent domain) is blocked and returns the
// matching rule domain, e.g. host "x.ads.doubleclick.net" -> "doubleclick.net".
// Exceptions (@@) on the host or any parent override a block.
func (b *Blocklist) Match(host string) (string, bool) {
	b.load()
	if b == nil || b.count == 0 {
		return "", false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", false
	}
	// walk from the full host up to the second-level domain
	var hit string
	found := false
	for i := 0; i <= len(labels)-2; i++ {
		cand := strings.Join(labels[i:], ".")
		rk := reverseDomain(cand)
		if !b.bloomMay(rk) {
			continue
		}
		if b.hasExc && b.has("@"+rk) {
			return "", false
		}
		if !found && b.has(rk) {
			hit, found = cand, true
		}
	}
	return hit, found
}
