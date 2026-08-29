package tools

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPageCursorRejectsTampering(t *testing.T) {
	token := makePageKeyCursor("grep", "query", "generation", "last")
	if token == "" {
		t.Fatal("empty cursor")
	}
	tampered := token[:len(token)-1] + "A"
	if tampered == token {
		tampered = token[:len(token)-1] + "B"
	}
	if _, err := openPageKeyCursor(tampered, "grep", "query"); err == nil {
		t.Fatal("tampered cursor accepted")
	}
}

func TestPageCursorBindsQueryAndGeneration(t *testing.T) {
	token := makePageKeyCursor("glob", "query", "generation", "last")
	if _, err := openPageKeyCursor(token, "glob", "other"); err == nil || !strings.Contains(err.Error(), "request") {
		t.Fatalf("query error = %v", err)
	}
	// The generation is checked after the scan, not before it, which is
	// why these are two calls and not one: openPageKeyCursor hands back
	// the boundary the scan needs, and finishPageKeyCursor rejects the
	// cursor once the scan has said what generation it saw.
	c, err := openPageKeyCursor(token, "glob", "query")
	if err != nil {
		t.Fatalf("open = %v", err)
	}
	if err := finishPageKeyCursor(c, "other"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("generation error = %v", err)
	}
}

func TestPageScanIsBoundedAndPaginatesWideResults(t *testing.T) {
	const total = 100_000
	first := newPageScan[int]("", 17)
	for i := total - 1; i >= 0; i-- {
		key := fmt.Sprintf("%06d", i)
		first.Add(key, i)
	}
	values, last, truncated, gotTotal, generation := first.Finish()
	if len(values) != 17 || !truncated || gotTotal != total || values[0] != 0 || values[16] != 16 {
		t.Fatalf("first page = len %d truncated %v total %d range %v..%v", len(values), truncated, gotTotal, values[0], values[len(values)-1])
	}

	second := newPageScan[int](last, 17)
	for i := 0; i < total; i++ {
		second.Add(fmt.Sprintf("%06d", i), i)
	}
	values, _, truncated, gotTotal, secondGeneration := second.Finish()
	if len(values) != 17 || !truncated || gotTotal != total || values[0] != 17 || values[16] != 33 {
		t.Fatalf("second page = len %d truncated %v total %d range %v..%v", len(values), truncated, gotTotal, values[0], values[len(values)-1])
	}
	if generation != secondGeneration {
		t.Fatalf("generation depends on scan order: %q != %q", generation, secondGeneration)
	}
}

func TestReadCursorDetectsSameSizeRewrite(t *testing.T) {
	path := t.TempDir() + "/file.txt"
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := makePageCursor("read", path, 1)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := parsePageCursor(token, "read", path); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("same-size rewrite error = %v", err)
	}
}

func TestReadCursorDetectsSuffixRewriteBeyond64KiB(t *testing.T) {
	path := t.TempDir() + "/large.txt"
	content := append(make([]byte, 70*1024), []byte("first suffix\n")...)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := makePageCursor("read", path, 1)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	copy(content[len(content)-len("first suffix\n"):], "other suffix\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := parsePageCursor(token, "read", path); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("suffix rewrite error = %v", err)
	}
}

func TestVisitFileMatchesStreamsBeforeReadingWholeFile(t *testing.T) {
	path := t.TempDir() + "/huge.txt"
	var content strings.Builder
	for range 100_000 {
		content.WriteString("needle\n")
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	seen := 0
	err := visitFileMatches(ctx, path, regexp.MustCompile("needle"), func(lineMatch) {
		seen++
		if seen == 10 {
			cancel()
		}
	})
	if err == nil || seen != 10 {
		t.Fatalf("stream stopped with seen=%d err=%v", seen, err)
	}
}

func TestGrepContextsOpenEachFileOnce(t *testing.T) {
	path := t.TempDir() + "/file.txt"
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opens := 0
	contexts, err := loadGrepContexts(t.Context(), []grepMatch{{path: path, lineNum: 2}, {path: path, lineNum: 4}}, 1, 1, func(path string) (*os.File, error) {
		opens++
		return os.Open(path)
	})
	if err != nil || opens != 1 || len(contexts[path]) != 2 {
		t.Fatalf("opens=%d contexts=%v err=%v", opens, contexts, err)
	}
}

func TestReadTextFileCountDoesNotCountEmptySplitTail(t *testing.T) {
	path := t.TempDir() + "/file.txt"
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, more, consumed, err := readTextFileCount(path, 0, 2, 0)
	if err != nil || content != "one\ntwo" || !more || consumed != 2 {
		t.Fatalf("content=%q more=%v consumed=%d err=%v", content, more, consumed, err)
	}
}

func TestGrepContextFormat(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/file.txt"
	if err := os.WriteFile(path, []byte("before\nneedle\nafter\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := renderGrepMatchesWithContext(t.Context(), []grepMatch{{path: path, lineNum: 2, charNum: 1, lineText: "needle"}}, false, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- Line 1: before", "Line 2, Char 1: needle", "+ Line 3: after"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}
