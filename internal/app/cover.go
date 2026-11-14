package app

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// Cover tiles are drawn in CSS: a color from coverColors and one of
// coverPatterns, chosen from a hash of the title and author so the same book
// always looks the same and look-alike titles differ. Nothing is fetched.
const (
	coverColors   = 12
	coverPatterns = 4
)

// coverClass returns the CSS classes for a book's tile, such as "cover-7 pat-2".
func coverClass(title, author string) string {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(strings.TrimSpace(title))))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(author))))
	sum := h.Sum32()
	return fmt.Sprintf("cover-%d pat-%d", sum%coverColors, (sum/coverColors)%coverPatterns)
}
