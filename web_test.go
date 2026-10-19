package roundtable

import (
	"io/fs"
	"os"
	"testing"
)

// The page loads web/js/main.js, which imports the other modules, so every file
// under web/js must be in the binary.
func TestWebFSEmbedsModules(t *testing.T) {
	entries, err := os.ReadDir("web/js")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("web/js is empty")
	}
	for _, e := range entries {
		if _, err := fs.Stat(WebFS, "web/js/"+e.Name()); err != nil {
			t.Fatalf("%s is not embedded: %v", e.Name(), err)
		}
	}
}
