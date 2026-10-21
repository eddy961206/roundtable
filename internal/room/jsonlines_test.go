package room

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func checkJSONLineBoundaries(t testing.TB, data []byte) {
	t.Helper()
	var wantForward, wantReverse []string
	if len(data) > 0 {
		for _, line := range bytes.SplitAfter(data, []byte{'\n'}) {
			if len(line) > 0 {
				wantForward = append(wantForward, string(line))
			}
		}
		body := bytes.TrimSuffix(data, []byte{'\n'})
		for _, line := range bytes.Split(body, []byte{'\n'}) {
			wantReverse = append(wantReverse, string(line))
		}
		slices.Reverse(wantReverse)
	}
	var forward, reverse []string
	if err := readJSONLines(bytes.NewReader(data), "test.jsonl", func(line []byte) error {
		forward = append(forward, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := readJSONLinesReverse(bytes.NewReader(data), int64(len(data)), func(line []byte) error {
		reverse = append(reverse, string(line))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forward, wantForward) || !reflect.DeepEqual(reverse, wantReverse) {
		t.Fatalf("record boundaries changed: input=%d bytes, forward=%d/%d records, reverse=%d/%d records", len(data), len(forward), len(wantForward), len(reverse), len(wantReverse))
	}
}

func TestJSONLineBufferBoundaries(t *testing.T) {
	for _, size := range []int{0, 1, 65534, 65535, 65536, 65537, 131071, 131072, 131073, 512000} {
		for _, ending := range []string{"", "\n", "\r\n", "\n\n"} {
			data := []byte("first\n\n" + strings.Repeat("x", size) + ending + "last\n")
			checkJSONLineBoundaries(t, data)
			checkJSONLineBoundaries(t, bytes.TrimSuffix(data, []byte{'\n'}))
		}
	}
	for _, data := range []string{"", "\n", "\n\n", "\r\n", "first"} {
		checkJSONLineBoundaries(t, []byte(data))
	}
}

type jsonLineReadError struct{ err error }

func (r jsonLineReadError) Read([]byte) (int, error)          { return 0, r.err }
func (r jsonLineReadError) ReadAt([]byte, int64) (int, error) { return 0, r.err }

func TestJSONLineErrorsRemainVisible(t *testing.T) {
	want := errors.New("synthetic read failure")
	source := io.MultiReader(strings.NewReader(strings.Repeat("x", 140000)), jsonLineReadError{want})
	count := 0
	err := readJSONLines(source, "events.jsonl", func(line []byte) error {
		count++
		if len(line) != 140000 {
			t.Fatalf("partial record size=%d", len(line))
		}
		return nil
	})
	if !errors.Is(err, want) || count != 1 || !strings.Contains(err.Error(), "events.jsonl:1") {
		t.Fatalf("read error lost: calls=%d err=%v", count, err)
	}
	if err := readJSONLinesReverse(jsonLineReadError{want}, 1, func([]byte) error { return nil }); !errors.Is(err, want) {
		t.Fatalf("reverse read error lost: %v", err)
	}
	data := strings.Repeat("x", 140000) + "\nsecond\nthird\n"
	count = 0
	err = readJSONLines(strings.NewReader(data), "events.jsonl", func([]byte) error {
		count++
		if count == 2 {
			return want
		}
		return nil
	})
	if !errors.Is(err, want) || count != 2 || !strings.Contains(err.Error(), "events.jsonl:2") {
		t.Fatalf("consumer error lost: calls=%d err=%v", count, err)
	}
	count = 0
	err = readJSONLinesReverse(strings.NewReader(data), int64(len(data)), func([]byte) error {
		count++
		return want
	})
	if !errors.Is(err, want) || count != 1 {
		t.Fatalf("reverse consumer error lost: calls=%d err=%v", count, err)
	}
}

func FuzzJSONLineBoundaries(f *testing.F) {
	for _, data := range []string{"", "\n", "a\nb", "a\n\n", "a\r\nb\r\n", strings.Repeat("x", 65536), "\n" + strings.Repeat("x", 131072) + "\n"} {
		f.Add([]byte(data))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 2*1024*1024 {
			t.Skip()
		}
		checkJSONLineBoundaries(t, data)
	})
}
