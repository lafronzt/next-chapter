package app

import (
	"fmt"
	"strings"
	"testing"
)

func TestCoverIsDeterministicAndVaried(t *testing.T) {
	t.Parallel()
	if coverClass("Piranesi", "Susanna Clarke") != coverClass("  piranesi ", "SUSANNA CLARKE") {
		t.Fatal("not stable across case and spacing")
	}
	seen := map[string]bool{}
	colors := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := coverClass(fmt.Sprintf("Book number %d", i), fmt.Sprintf("Author %d", i%17))
		if again := coverClass(fmt.Sprintf("Book number %d", i), fmt.Sprintf("Author %d", i%17)); again != c {
			t.Fatal("not deterministic")
		}
		seen[c] = true
		colors[strings.Fields(c)[0]] = true
	}
	if len(seen) < 10 || len(colors) < 10 {
		t.Fatalf("%d tiles, %d colors across 200 books", len(seen), len(colors))
	}
	// Similar titles differ, not just by id.
	if coverClass("The Sun", "A") == coverClass("The Sun", "B") && coverClass("The Sun", "A") == coverClass("The Sun", "C") {
		t.Fatal("author is ignored")
	}
	for _, c := range []string{coverClass("", ""), coverClass("星の王子さま", "サン＝テグジュペリ"), coverClass("Ж", "")} {
		var color, pattern int
		if _, err := fmt.Sscanf(c, "cover-%d pat-%d", &color, &pattern); err != nil || color >= coverColors || pattern >= coverPatterns {
			t.Fatalf("%q", c)
		}
	}
}
