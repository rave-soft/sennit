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
	"strings"
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
	minimum, maximum              *int
	minLength, minItems, maxItems *int
	enum                          []string
	pattern                       string
}

// ToolSchemaConstraint describes constraints omitted by Fantasy's reflection
// schema generator. It lets coordinator-built tools use the same strict schema
// path handling as package-built tools.
type ToolSchemaConstraint struct {
	Minimum, Maximum              *int
	MinLength, MinItems, MaxItems *int
	Enum                          []string
	Pattern                       string
}

func intSchemaBounds(maximum int) toolParameterSchema {
	minimum := 0
	return toolParameterSchema{minimum: &minimum, maximum: &maximum}
}

func intSchemaMinimum(minimum int) toolParameterSchema {
	return toolParameterSchema{minimum: &minimum}
}

// withToolParameterSchema applies explicit JSON-schema constraints omitted by
// Fantasy's reflection generator. Paths use dot notation and may descend via
// "items" and "properties" (for example, "questions.items.type"). A stale
// path is a programming error: silently accepting it would advertise a schema
// contract the tool does not actually have.
// WithToolSchemaConstraints applies explicit constraints to a coordinator-built
// or package-built tool. It is intentionally strict about paths.
func WithToolSchemaConstraints(tool fantasy.AgentTool, constraints map[string]ToolSchemaConstraint) fantasy.AgentTool {
	schemas := make(map[string]toolParameterSchema, len(constraints))
	for path, constraint := range constraints {
		schemas[path] = toolParameterSchema{minimum: constraint.Minimum, maximum: constraint.Maximum, minLength: constraint.MinLength, minItems: constraint.MinItems, maxItems: constraint.MaxItems, enum: constraint.Enum, pattern: constraint.Pattern}
	}
	return withToolParameterSchema(tool, schemas)
}

func withToolParameterSchema(tool fantasy.AgentTool, schemas map[string]toolParameterSchema) fantasy.AgentTool {
	info := tool.Info()
	for path, schema := range schemas {
		parameter := schemaParameter(info.Parameters, path)
		if schema.minimum != nil {
			parameter["minimum"] = *schema.minimum
		}
		if schema.maximum != nil {
			parameter["maximum"] = *schema.maximum
		}
		if schema.enum != nil {
			values := make([]any, len(schema.enum))
			for i, v := range schema.enum {
				values[i] = v
			}
			parameter["enum"] = values
		}
		for key, value := range map[string]*int{"minLength": schema.minLength, "minItems": schema.minItems, "maxItems": schema.maxItems} {
			if value != nil {
				parameter[key] = *value
			}
		}
		if schema.pattern != "" {
			parameter["pattern"] = schema.pattern
		}
	}
	return toolInfoOverride{AgentTool: tool, info: info}
}

// withToolRootSchema appends constraints that compose at the input object's
// root, where relationships between multiple parameters must be expressed.
func withToolRootSchema(tool fantasy.AgentTool, constraints ...map[string]any) fantasy.AgentTool {
	info := tool.Info()
	allOf, _ := info.InputSchema["allOf"].([]any)
	for _, constraint := range constraints {
		allOf = append(allOf, constraint)
	}
	info.InputSchema["allOf"] = allOf
	return toolInfoOverride{AgentTool: tool, info: info}
}

func schemaParameter(parameters map[string]any, path string) map[string]any {
	parts := strings.Split(path, ".")
	current, ok := parameters[parts[0]]
	if !ok {
		panic(fmt.Sprintf("tool schema path %q: missing root parameter", path))
	}
	for _, part := range parts[1:] {
		object, ok := current.(map[string]any)
		if !ok {
			panic(fmt.Sprintf("tool schema path %q: expected object before %q", path, part))
		}
		if part == "items" || part == "properties" {
			current = object[part]
		} else {
			properties, ok := object["properties"].(map[string]any)
			if !ok {
				panic(fmt.Sprintf("tool schema path %q: missing properties", path))
			}
			current = properties[part]
		}
		if current == nil {
			panic(fmt.Sprintf("tool schema path %q: missing %q", path, part))
		}
	}
	parameter, ok := current.(map[string]any)
	if !ok {
		panic(fmt.Sprintf("tool schema path %q: expected parameter object", path))
	}
	return parameter
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
	Index   int    `json:"x,omitempty"`
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

func validatePageKeyCursor(token, kind, query, generation string) error {
	c, err := openPageKeyCursor(token, kind, query)
	if err != nil {
		return err
	}
	return finishPageKeyCursor(c, generation)
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
