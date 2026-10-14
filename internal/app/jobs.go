package app

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// AI requests run in the background so the reader is not stuck on a page
// while a slow or self-hosted model answers. Asking checks the request
// against its preview right away, starts the job, and shows a waiting page
// that reloads itself; every other page carries a note until the reader has
// seen the outcome. There is one job at a time, matching askAI's lock, and
// jobs live in memory: the app runs as a single replica, and a restart only
// loses an answer the reader can ask for again.

// aiJob is the latest AI request. Fields after Started change once, under
// App.jobMu, when the request finishes.
type aiJob struct {
	ID string
	// Label finishes "The AI is working on …"; Back is where the request was
	// made; Warning says what would make the answer unusable meanwhile.
	Label, Back, Warning string
	Started              time.Time
	Done                 bool
	Location             string // the result, once done without error
	Err                  error
	Seen                 bool // the reader opened the finished job's page
}

// startAIJob runs send in the background and returns the job's id. send
// returns where to show its result.
func (a *App) startAIJob(label, back, warning string, send func(context.Context) (string, error)) (string, error) {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	if a.job != nil && !a.job.Done {
		return "", errAIBusy
	}
	job := &aiJob{ID: randomToken(), Label: label, Back: back, Warning: warning, Started: a.now()}
	a.job = job
	a.jobs.Add(1)
	go func() {
		defer a.jobs.Done()
		location, err := send(a.jobCtx)
		a.jobMu.Lock()
		defer a.jobMu.Unlock()
		job.Done, job.Location, job.Err = true, location, err
	}()
	return job.ID, nil
}

// currentAIJob returns a copy of the latest job, or nil.
func (a *App) currentAIJob() *aiJob {
	a.jobMu.Lock()
	defer a.jobMu.Unlock()
	if a.job == nil {
		return nil
	}
	job := *a.job
	return &job
}

// aiJobPage shows a running job, sends the reader on to a finished job's
// result, or explains why it failed.
func (a *App) aiJobPage(w http.ResponseWriter, r *http.Request) error {
	a.jobMu.Lock()
	job := a.job
	if job == nil || job.ID != r.PathValue("id") {
		a.jobMu.Unlock()
		return httpError{Status: 404, Message: "That AI request is no longer shown here. Anything it saved is where you asked for it."}
	}
	if job.Done {
		job.Seen = true
	}
	snapshot := *job
	a.jobMu.Unlock()
	if snapshot.Done && snapshot.Err == nil {
		return redirect(w, r, snapshot.Location)
	}
	ai, err := a.aiStatus(a.store.DB)
	if err != nil {
		return err
	}
	data := page{"Title": "Asking the AI", "Job": snapshot, "AI": ai, "Nav": "", "Waited": waited(a.now().Sub(snapshot.Started))}
	status := 200
	if snapshot.Done {
		data["Title"] = "The AI request did not finish"
		status, data["Message"] = a.failure(r, snapshot.Err)
	} else {
		data["Refresh"] = 3
	}
	return a.render(w, r, "ai_job.html", data, status)
}

// waited says how long a request has run, in words.
func waited(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 5:
		return "a moment"
	case s < 60:
		return fmt.Sprintf("%d seconds", s)
	}
	m := s / 60
	return fmt.Sprintf("%d %s %d seconds", m, plural(m, "minute", "minutes"), s%60)
}
