package main

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/hibooboo2/gradio/db"
	"github.com/hibooboo2/gradio/models"
	"github.com/hibooboo2/gradio/views"
)

// historyDefaultLimit is how many history entries the view shows by default.
const historyDefaultLimit = 100

// historyMaxLimit caps the ?limit= query param so a single request cannot ask
// for an unbounded history dump.
const historyMaxLimit = 1000

// historyParams parses the sort/group/limit query params shared by the history
// view fragment and the JSON API. sort defaults to "recency"; group is enabled
// when group=radio; limit is clamped to [1, historyMaxLimit].
func historyParams(r *http.Request) (sort string, group bool, limit int) {
	sort = r.URL.Query().Get("sort")
	if sort != "frequency" {
		sort = "recency"
	}
	group = r.URL.Query().Get("group") == "radio"
	limit = historyDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= historyMaxLimit {
			limit = n
		}
	}
	return sort, group, limit
}

// handleHistoryView renders the play history tab fragment. It supports
// ?sort=recency|frequency (default recency), ?group=radio to group entries by
// radio, and ?limit=N to cap the number of entries.
func handleHistoryView(w http.ResponseWriter, r *http.Request) {
	sort, group, limit := historyParams(r)

	var entries []models.HistoryEntry
	var groups []models.RadioHistoryGroup
	var err error

	if group {
		groups, err = db.FetchPlayHistoryGrouped(r.Context(), limit)
	} else if sort == "frequency" {
		entries, err = db.FetchPlayHistoryFrequency(r.Context(), limit)
	} else {
		entries, err = db.FetchPlayHistoryRecency(r.Context(), limit)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "load play history", "err", err)
		http.Error(w, "failed to load play history", http.StatusInternalServerError)
		return
	}

	total := len(entries)
	if group {
		total = 0
		for _, g := range groups {
			total += len(g.Plays)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.HistoryView(models.HistoryViewData{
		Sort:    sort,
		Group:   group,
		Limit:   limit,
		Total:   total,
		Entries: entries,
		Groups:  groups,
	}).Render(r.Context(), w); err != nil {
		slog.ErrorContext(r.Context(), "render play history view", "err", err)
	}
}

// handleHistoryJSON returns the play history as JSON. It accepts the same
// ?sort, ?group and ?limit query params as the view fragment.
func handleHistoryJSON(w http.ResponseWriter, r *http.Request) {
	sort, group, limit := historyParams(r)

	if group {
		groups, err := db.FetchPlayHistoryGrouped(r.Context(), limit)
		if err != nil {
			slog.ErrorContext(r.Context(), "play history json", "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, groups)
		return
	}

	var entries []models.HistoryEntry
	var err error
	if sort == "frequency" {
		entries, err = db.FetchPlayHistoryFrequency(r.Context(), limit)
	} else {
		entries, err = db.FetchPlayHistoryRecency(r.Context(), limit)
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "play history json", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
