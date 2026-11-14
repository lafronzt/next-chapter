package app

import (
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Manual AI is the optional alternative to a configured provider: the reader
// copies the exact prompt into any AI they like, runs it themselves, and pastes
// the answer back. It needs no provider, key, or network call from this app,
// and the answer goes through the same checks as one from a provider. It never
// counts toward the monthly cap.

// manualModel labels answers that were pasted in.
const manualModel = "your own AI (pasted)"

// maxManualAnswer bounds a pasted answer; real answers are a few kilobytes.
const maxManualAnswer = 64 << 10

// manualAI is what a preview page needs to offer the paste-back flow.
type manualAI struct {
	// Prompt is the whole request as one block of text to copy.
	Prompt string
	// Action is where the pasted answer is posted.
	Action string
	// Fingerprint ties the answer to what the reader saw.
	Fingerprint string
}

// manualPrompt renders a request as text for a chat window: the instructions,
// the library data, and the shape the answer must have.
func manualPrompt(req AIRequest) string {
	return req.System + "\n\n" + req.User + "\n\nReply with only one JSON object that matches this JSON Schema, with no other text:\n" + string(req.Schema) + "\n"
}

// extractAnswerJSON pulls the JSON object out of text a chat assistant wrote:
// code fences and a sentence before or after it are common, so take from the
// first "{" to the last "}". decodeAnswer then checks it strictly.
func extractAnswerJSON(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", bad("That answer is not text. Paste the AI's reply as plain text.")
	}
	if len(text) > maxManualAnswer {
		return "", bad("That answer is much longer than expected. Paste only the JSON the AI gave you.")
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return "", bad("Paste the AI's whole reply. It should contain a JSON object that starts with { and ends with }.")
	}
	return text[start : end+1], nil
}

// pastedAnswer reads the pasted answer from the form.
func pastedAnswer(r *http.Request) (string, error) {
	text, err := extractAnswerJSON(r.FormValue("answer"))
	if err != nil {
		return "", err
	}
	return text, nil
}

// manualFor builds the paste-back offer for a request.
func manualFor(req AIRequest, action, fingerprint string) manualAI {
	return manualAI{Prompt: manualPrompt(req), Action: action, Fingerprint: fingerprint}
}

// pasteEstimate saves a pasted answer as a book's AI estimate.
func (a *App) pasteEstimate(w http.ResponseWriter, r *http.Request) error {
	id, err := requestID(r)
	if err != nil {
		return err
	}
	text, err := pastedAnswer(r)
	if err != nil {
		return err
	}
	_, _, save, err := a.prepareEstimate(id, r.FormValue("fingerprint"), false)
	if err != nil {
		return err
	}
	if err = save(text, manualModel); err != nil {
		return err
	}
	return redirect(w, r, fmt.Sprintf("/books/%d/estimate?notice=estimated", id))
}

// pastePlan turns a pasted answer into the AI order of a queue suggestion.
func (a *App) pastePlan(w http.ResponseWriter, r *http.Request) error {
	text, err := pastedAnswer(r)
	if err != nil {
		return err
	}
	_, save, err := a.prepareRerank(r.PathValue("id"), r.FormValue("fingerprint"))
	if err != nil {
		return err
	}
	id, err := save(text, manualModel, 0, 0)
	if err != nil {
		return err
	}
	return redirect(w, r, "/plans/"+id)
}
