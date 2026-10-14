package app

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"net/http"
)

// estimate shows a book's estimated rating: the baseline, any saved AI
// estimate, and, when AI is on, exactly what asking the AI would send. It
// never calls a provider.
func (a *App) estimate(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	inputs, err := loadEstimateInputs(a.store.DB, id)
	if err != nil {
		return err
	}
	saved, err := readEstimate(a.store.DB, id)
	if err != nil {
		return err
	}
	ai, err := a.aiStatus(a.store.DB)
	if err != nil {
		return err
	}
	data := page{"Title": "Estimated rating: " + inputs.Book.Title, "Book": inputs.Book, "Inputs": inputs, "Saved": saved, "AI": ai,
		"Stale": saved != nil && saved.Fingerprint != inputs.Fingerprint}
	if inputs.Rated {
		acc, err := loadAccuracy(a.store.DB, id)
		if err != nil {
			return err
		}
		data["Accuracy"] = acc
	}
	if ai.Available() && !inputs.Rated && inputs.Baseline.Rated > 0 {
		payload := buildEstimatePayload(inputs.Book, inputs.History, inputs.Baseline, false)
		req, err := estimateRequest(payload)
		if err != nil {
			return err
		}
		prompt := a.previewPrompt(req)
		if inputs.KeepReviews {
			prompt.Note = "If you also send your review text, the books you reviewed gain a \"your_review\" field with up to 1,000 characters of it."
		}
		data["Payload"], data["Prompt"] = payload, prompt
		if inputs.KeepReviews {
			with := buildEstimatePayload(inputs.Book, inputs.History, inputs.Baseline, true)
			reviewed := []estimateRow{}
			for _, row := range with.RatedBooks {
				if row.Review != "" {
					reviewed = append(reviewed, row)
				}
			}
			data["Reviewed"] = reviewed
		}
	}
	return a.render(w, r, "estimate.html", data, 200)
}

// predict saves the baseline estimate. Nothing in the library changes, so
// the revision stays the same.
func (a *App) predict(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	err = a.store.Write(func(tx *sql.Tx) error {
		inputs, err := loadEstimateInputs(tx, id)
		if err != nil {
			return err
		}
		if inputs.Rated {
			return bad("You have already rated this book, so there is nothing to estimate.")
		}
		if inputs.Baseline.Rated == 0 {
			return nil // the page explains that ratings are needed
		}
		return saveBaseline(tx, id, inputs.Fingerprint, inputs.Baseline.Estimate, a.now())
	})
	if err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d/estimate", id))
}

// predictAI checks the previewed request and sends it to the AI provider in
// the background.
func (a *App) predictAI(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	if ai, err := a.aiStatus(a.store.DB); err != nil || !ai.Available() {
		return cmp.Or(err, error(errAIOff))
	}
	title, send, err := a.prepareEstimateAI(id, r.FormValue("fingerprint"), r.FormValue("reviews") != "")
	if err != nil {
		return err
	}
	back := fmt.Sprintf("/books/%d/estimate", id)
	job, err := a.startAIJob("an estimated rating for "+title, back, "If you rate books or edit this one before it answers, the estimate cannot be saved and you can ask again.", func(ctx context.Context) (string, error) {
		return back + "?notice=estimated", send(ctx)
	})
	if err != nil {
		return err
	}
	return redirect(w, r, "/ai/jobs/"+job)
}
