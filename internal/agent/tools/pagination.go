package tools

import (
	"container/heap"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"charm.land/fantasy"
)

const maxPageResults = 1000

type toolInfoOverride struct {
	fantasy.AgentTool
	info fantasy.ToolInfo
}

func (t toolInfoOverride) Info() fantasy.ToolInfo { return t.info }

type toolParameterSchema struct {
	minimum *int
	maximum *int
}

func intSchemaBounds(minimum, maximum int) toolParameterSchema {
	return toolParameterSchema{minimum: &minimum, maximum: &maximum}
}

func intSchemaMinimum(minimum int) toolParameterSchema {
	return toolParameterSchema{minimum: &minimum}
}

// withToolParameterSchema fills constraints that Fantasy's reflection schema
// generator does not derive from numeric struct tags.
func withToolParameterSchema(tool fantasy.AgentTool, schemas map[string]toolParameterSchema) fantasy.AgentTool {
	info := tool.Info()
	for name, schema := range schemas {
		parameter, ok := info.Parameters[name].(map[string]any)
		if !ok {
			continue
		}
		if schema.minimum != nil {
			parameter["minimum"] = *schema.minimum
		}
		if schema.maximum != nil {
			parameter["maximum"] = *schema.maximum
		}
	}
	return toolInfoOverride{AgentTool: tool, info: info}
}

type pageCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"k"`
	Query   string `json:"q"`
	Gen     string `json:"g"`
	Last    string `json:"l"`
	Path    string `json:"p,omitempty"`
	Offset  int    `json:"o,omitempty"`
	FileID  string `json:"i,omitempty"`
}

var (
	cursorSecret     []byte
	cursorSecretOnce sync.Once
)

func pageCursorSecret() []byte {
	cursorSecretOnce.Do(func() {
		cursorSecret = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, cursorSecret); err != nil {
			panic(fmt.Sprintf("generating cursor secret: %v", err))
		}
	})
	return cursorSecret
}

func fingerprintPage(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func encodePageCursor(c pageCursor) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, pageCursorSecret())
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(append(payload, mac.Sum(nil)...)), nil
}

func decodePageCursor(token string) (pageCursor, error) {
	var c pageCursor
	encoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(encoded) <= sha256.Size {
		return c, fmt.Errorf("invalid cursor")
	}
	payload, signature := encoded[:len(encoded)-sha256.Size], encoded[len(encoded)-sha256.Size:]
	mac := hmac.New(sha256.New, pageCursorSecret())
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(payload, &c) != nil {
		return c, fmt.Errorf("invalid cursor")
	}
	return c, nil
}

func makePageKeyCursor(kind, query, generation, last string) string {
	token, _ := encodePageCursor(pageCursor{Version: 2, Kind: kind, Query: query, Gen: generation, Last: last})
	return token
}

// openPageKeyCursor validates the signed request binding before a scan and
// returns its keyset boundary. finishPageKeyCursor validates the generation
// computed by that scan. Splitting the checks lets scanners discard candidates
// before the boundary while retaining only O(page size) results.
func openPageKeyCursor(token, kind, query string) (pageCursor, error) {
	if token == "" {
		return pageCursor{}, nil
	}
	c, err := decodePageCursor(token)
	if err != nil || c.Version != 2 || c.Kind != kind {
		return pageCursor{}, fmt.Errorf("invalid cursor")
	}
	if c.Query != query {
		return pageCursor{}, fmt.Errorf("cursor does not match this request")
	}
	return c, nil
}

func finishPageKeyCursor(c pageCursor, generation string) error {
	if c.Version != 0 && c.Gen != generation {
		return fmt.Errorf("stale cursor")
	}
	return nil
}

func parsePageKeyCursor(token, kind, query, generation string) (string, error) {
	c, err := openPageKeyCursor(token, kind, query)
	if err != nil {
		return "", err
	}
	if err := finishPageKeyCursor(c, generation); err != nil {
		return "", err
	}
	return c.Last, nil
}

type pageItem[T any] struct {
	key   string
	value T
}

type maxPageHeap[T any] []pageItem[T]

func (h maxPageHeap[T]) Len() int           { return len(h) }
func (h maxPageHeap[T]) Less(i, j int) bool { return h[i].key > h[j].key }
func (h maxPageHeap[T]) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxPageHeap[T]) Push(v any)        { *h = append(*h, v.(pageItem[T])) }
func (h *maxPageHeap[T]) Pop() any {
	old := *h
	v := old[len(old)-1]
	*h = old[:len(old)-1]
	return v
}

// pageScan computes an order-independent generation fingerprint and exact
// total while retaining only the first limit+1 values after a keyset boundary.
type pageScan[T any] struct {
	mu         sync.Mutex
	last       string
	limit      int
	total      int
	generation [4]uint64
	items      maxPageHeap[T]
}

func newPageScan[T any](last string, limit int) *pageScan[T] {
	return &pageScan[T]{last: last, limit: limit}
}

func (s *pageScan[T]) Add(key string, value T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := sha256.Sum256([]byte(key))
	for i := range s.generation {
		s.generation[i] += binary.LittleEndian.Uint64(digest[i*8 : (i+1)*8])
	}
	s.total++
	if key <= s.last {
		return
	}
	heap.Push(&s.items, pageItem[T]{key: key, value: value})
	if s.items.Len() > s.limit+1 {
		heap.Pop(&s.items)
	}
}

func (s *pageScan[T]) Finish() (values []T, last string, truncated bool, total int, generation string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]pageItem[T], s.items.Len())
	for i := len(items) - 1; i >= 0; i-- {
		items[i] = heap.Pop(&s.items).(pageItem[T])
	}
	truncated = len(items) > s.limit
	if truncated {
		items = items[:s.limit]
	}
	values = make([]T, len(items))
	for i := range items {
		values[i] = items[i].value
	}
	if len(items) > 0 {
		last = items[len(items)-1].key
	}
	var raw [32]byte
	for i, v := range s.generation {
		binary.LittleEndian.PutUint64(raw[i*8:(i+1)*8], v)
	}
	generation = fmt.Sprintf("%d:%x", s.total, raw)
	return values, last, truncated, s.total, generation
}

func canonicalPath(path string) string { return filepath.Clean(path) }

func makePageCursor(kind, path string, offset int) (string, error) {
	id, err := readFileIdentity(path)
	if err != nil {
		return "", err
	}
	return encodePageCursor(pageCursor{Version: 1, Kind: kind, Path: canonicalPath(path), Offset: offset, FileID: id})
}

func parsePageCursor(token, kind, path string) (int, error) {
	c, err := decodePageCursor(token)
	if err != nil || c.Version != 1 || c.Kind != kind || c.Path != canonicalPath(path) || c.Offset < 0 {
		return 0, fmt.Errorf("invalid cursor")
	}
	id, err := readFileIdentity(path)
	if err != nil || !hmac.Equal([]byte(id), []byte(c.FileID)) {
		return 0, fmt.Errorf("stale cursor")
	}
	return c.Offset, nil
}

// readFileIdentity combines stable filesystem identity where available with
// file metadata and a full streaming content hash.
func readFileIdentity(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%d\x00%s\x00", canonicalPath(path), info.Size(), info.ModTime().UnixNano(), stableFileID(info))
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// stableFileID extracts device/inode without platform-specific build files.
// Platforms whose FileInfo does not expose them still retain the metadata and
// content identity above.
func stableFileID(info os.FileInfo) string {
	value := reflect.ValueOf(info.Sys())
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return ""
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return ""
	}
	dev, ino := value.FieldByName("Dev"), value.FieldByName("Ino")
	if !dev.IsValid() || !ino.IsValid() || !dev.CanUint() || !ino.CanUint() {
		return ""
	}
	return fmt.Sprintf("%d:%d", dev.Uint(), ino.Uint())
}
