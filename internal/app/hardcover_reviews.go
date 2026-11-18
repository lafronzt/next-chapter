package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
)

// Reviews are sent individually, separately from private notes and shelf sync.
// Hardcover has one review per book, while local rereads each keep their own.
type hcReviewPlan struct {
	ReadingID, BookID, UserID, HardcoverBookID int64
	Title, Entry                               string
	Remote                                     hcReview
}

type hcReview struct {
	ID       flexInt `json:"id"`
	Markdown string  `json:"review_markdown"`
	Raw      string  `json:"review_raw"`
	Updated  string  `json:"updated_at"`
	Privacy  int     `json:"privacy_setting_id"`
}

// UserBookUpdateInput.review_markdown verified in Hardcover's published schema:
// https://github.com/hardcoverapp/hardcover-docs/blob/main/schema.graphql
const hcReviewQuery = `query NextChapterReview($uid: Int!, $book: Int!) {
 user_books(where: {user_id: {_eq: $uid}, book_id: {_eq: $book}}, limit: 2) {
 id review_markdown review_raw updated_at privacy_setting_id
 }
}`
const hcReviewMutation = `mutation NextChapterReviewUpdate($id: Int!, $review: String!) {
 update_user_book(id: $id, object: {review_markdown: $review}) {
 id error user_book { id review_markdown }
 }
}`

func (c *hardcoverClient) bookReview(ctx context.Context, uid, book int64) (hcReview, error) {
	var out struct {
		Books []hcReview `json:"user_books"`
	}
	if err := c.do(ctx, hcReviewQuery, map[string]any{"uid": uid, "book": book}, &out); err != nil {
		return hcReview{}, err
	}
	if len(out.Books) != 1 || out.Books[0].ID <= 0 {
		return hcReview{}, bad("Send this book's status to Hardcover first, then share its review. Exactly one library entry is required.")
	}
	if out.Books[0].Privacy < 1 || out.Books[0].Privacy > 3 {
		return hcReview{}, bad("Hardcover returned an unknown privacy setting. Check the book on Hardcover before sharing.")
	}
	return out.Books[0], nil
}

func getReviewReading(q queryer, rid int64) (readingEntry, error) {
	entries, err := queryReadings(q, "WHERE l.id=?", rid)
	if err != nil {
		return readingEntry{}, err
	}
	if len(entries) != 1 {
		return readingEntry{}, sql.ErrNoRows
	}
	return entries[0], nil
}

func (a *App) hardcoverReviewPreview(w http.ResponseWriter, r *http.Request) error {
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	if err := current(a.store.DB, r); err != nil {
		return err
	}
	rid, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil || rid < 1 {
		return bad("Choose a valid reading.")
	}
	e, err := getReviewReading(a.store.DB, rid)
	if err != nil {
		return err
	}
	if e.Review == nil || e.ReviewFormat != "markdown" {
		return bad("Write and save a review here before sharing it.")
	}
	link, err := getBookLink(a.store.DB, e.BookID)
	if err != nil {
		return err
	}
	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	if link == nil || acct == nil {
		return bad("Link the book and test the Hardcover connection first.")
	}
	p := hcReviewPlan{ReadingID: rid, BookID: e.BookID, UserID: acct.UserID, HardcoverBookID: link.HardcoverBookID, Title: e.Title, Entry: *e.Review}
	a.hardcoverMu.Lock()
	p.Remote, err = a.hardcover.bookReview(r.Context(), p.UserID, p.HardcoverBookID)
	a.hardcoverMu.Unlock()
	if err != nil {
		return hardcoverHTTPError(err)
	}
	var planID string
	err = a.store.Write(func(tx *sql.Tx) error {
		if err := current(tx, r); err != nil {
			return err
		}
		var err error
		planID, err = savePlan(tx, "hc-review", Plan{HardcoverReview: &p})
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/hardcover/reviews/"+planID)
}

func (a *App) hardcoverReviewPlan(w http.ResponseWriter, r *http.Request) error {
	a.hardcoverMu.Lock()
	defer a.hardcoverMu.Unlock()
	planID := r.PathValue("id")
	kind, plan, rev, err := readPlan(a.store.DB, planID)
	if err != nil {
		return err
	}
	if kind != "hc-review" || plan.HardcoverReview == nil {
		return sql.ErrNoRows
	}
	p := *plan.HardcoverReview
	if r.Method == http.MethodGet {
		return a.render(w, r, "hardcover_review.html", page{"Title": "Share review", "Nav": "hardcover", "PlanID": planID, "Plan": p, "Review": readingEntry{Review: &p.Entry, ReviewFormat: "markdown"}, "Privacy": hcPrivacyNames[p.Remote.Privacy]}, 200)
	}
	if err := a.hardcoverReady(); err != nil {
		return err
	}
	if r.FormValue("confirm") != "share" {
		return bad("Confirm sharing this review.")
	}
	if err := current(a.store.DB, r); err != nil {
		return err
	}
	currentRev, err := revision(a.store.DB)
	if err != nil {
		return err
	}
	if currentRev != rev {
		return conflict("Your library changed. Preview sharing the review again.")
	}
	e, err := getReviewReading(a.store.DB, p.ReadingID)
	if err != nil {
		return err
	}
	acct, err := loadHardcoverLink(a.store.DB)
	if err != nil {
		return err
	}
	link, err := getBookLink(a.store.DB, p.BookID)
	if err != nil {
		return err
	}
	if e.BookID != p.BookID || e.Review == nil || *e.Review != p.Entry || e.ReviewFormat != "markdown" || acct == nil || acct.UserID != p.UserID || link == nil || link.HardcoverBookID != p.HardcoverBookID {
		return conflict("The review, account, or book link changed. Preview sharing again.")
	}
	remote, err := a.hardcover.bookReview(r.Context(), p.UserID, p.HardcoverBookID)
	if err != nil {
		return hardcoverHTTPError(err)
	}
	if remote.ID != p.Remote.ID || remote.Privacy != p.Remote.Privacy {
		return conflict("The Hardcover entry or its privacy changed. Preview sharing again.")
	}
	// A successful send with a lost response is safe to retry without rewriting.
	if remote.Markdown != p.Entry {
		if remote != p.Remote {
			return conflict("The review on Hardcover changed. Preview it again before replacing it.")
		}
		var out struct {
			Update struct {
				ID    flexInt   `json:"id"`
				Error string    `json:"error"`
				Book  *hcReview `json:"user_book"`
			} `json:"update_user_book"`
		}
		if err := a.hardcover.do(r.Context(), hcReviewMutation, map[string]any{"id": int64(remote.ID), "review": p.Entry}, &out); err != nil {
			if definiteRefusal(err) {
				return hardcoverHTTPError(err)
			}
			return httpError{Status: http.StatusBadGateway, Message: "Hardcover did not confirm the review. It may have saved it; retry this preview to check. Your local review is safe."}
		}
		if out.Update.Error != "" || out.Update.Book == nil || out.Update.Book.ID != remote.ID || out.Update.Book.Markdown != p.Entry {
			return hardcoverHTTPError(errHardcoverRefused)
		}
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec("DELETE FROM plans WHERE id=?", planID)
		return err
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d?notice=hc-review-sent#reading", p.BookID))
}
