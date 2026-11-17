package app

import (
	"net/url"
	"strconv"
	"strings"
)

type bookLink struct {
	Name, URL string
	Search    bool
}

// externalBookLinks builds ordinary outbound links without contacting a catalog.
// All hosts are fixed; imported identifiers are escaped rather than trusted URLs.
func externalBookLinks(b Book, hc *hcBookLink) []bookLink {
	query := strings.TrimSpace(b.Title + " " + b.Author)
	isbn := cleanISBN(b.ISBN)
	lookup := query
	if isbn != "" {
		lookup = isbn
	}

	library := bookLink{"Open Library", "https://openlibrary.org/search?q=" + url.QueryEscape(query), true}
	goodreads := bookLink{"Goodreads", "https://www.goodreads.com/search?q=" + url.QueryEscape(lookup), true}
	hardcover := bookLink{"Hardcover", "https://hardcover.app/search?query=" + url.QueryEscape(query), true}
	google := bookLink{"Google Books", "https://books.google.com/books?q=" + url.QueryEscape(lookup), true}
	if hc != nil && strings.TrimSpace(hc.Slug) != "" {
		hardcover.URL, hardcover.Search = "https://hardcover.app/books/"+url.PathEscape(hc.Slug), false
	}
	if isbn != "" {
		library.URL, library.Search = "https://openlibrary.org/isbn/"+url.PathEscape(isbn), false
	}
	if id, err := strconv.ParseInt(b.GoodreadsID, 10, 64); err == nil && id > 0 {
		goodreads.URL, goodreads.Search = "https://www.goodreads.com/book/show/"+strconv.FormatInt(id, 10), false
	}
	worldcatQuery := query
	if isbn != "" {
		worldcatQuery = "bn:" + isbn
	}
	worldcat := bookLink{"WorldCat", "https://search.worldcat.org/search?q=" + url.QueryEscape(worldcatQuery), true}
	return []bookLink{library, goodreads, hardcover, google, worldcat}
}
