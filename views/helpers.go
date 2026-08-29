// Package views contains the templ components that render every htmx fragment
// of the web UI. The old html/template fragments (playlists, player, splits,
// history, stations, favorites, settings, active recordings) were converted to
// a-h/templ so the markup is type-checked at compile time.
package views

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/hibooboo2/gradio/db"
	"github.com/hibooboo2/gradio/models"
)

// MusicURL returns the web URL that serves a split's output file, based on the
// output_path stored on the split row.
func MusicURL(outputPath string) string {
	rel := strings.TrimPrefix(filepath.ToSlash(outputPath), "split_music/")
	return "/music/" + rel
}

// DerivedSongTitle produces the default human-friendly label for a split's
// output file, e.g. "Slotex · gradio-2026-08-19_09-47-01 #3".
func DerivedSongTitle(s models.Split) string {
	base := strings.TrimSuffix(filepath.Base(s.SourcePath), filepath.Ext(s.SourcePath))
	radio := RadioFromPath(context.Background(), s.SourcePath)
	return fmt.Sprintf("%s · %s #%d", radio, base, s.Index+1)
}

// SongTitle produces the display label for a split. A user-set custom title
// (a display-only rename) wins; otherwise the title is derived from the source
// file name and stream position.
func SongTitle(s models.Split) string {
	if s.CustomTitle != "" {
		return s.CustomTitle
	}
	return DerivedSongTitle(s)
}

// RadioFromPath extracts the radio name from a source file path. Files are
// stored as recordings/<hash>/<file>.mp3 (or recordings/<radio>/<file>.mp3 for
// legacy recordings), or directly in recordings/ when no radio directory is
// present. A hashed directory is resolved back to the original station name
// via the recordings table so the UI shows display names, not hashes.
func RadioFromPath(ctx context.Context, path string) string {
	dir := filepath.Base(filepath.Dir(path))
	if dir == "." || dir == "" || dir == "recordings" {
		return "manual"
	}
	return db.RadioDisplayName(ctx, dir)
}

// ClsLabel returns the emoji + description label for a classification value.
func ClsLabel(cls string) string {
	return models.ClsLabel(cls)
}

// ClassificationOptions returns every classification the UI dropdowns show.
func ClassificationOptions() []models.ClassificationOption {
	return models.ClassificationOptions
}

// URLQ percent-encodes a value for use inside a URL query string.
func URLQ(s string) string {
	return url.QueryEscape(s)
}

// TimeStr renders a seconds count as "M:SS" (or "0:00" for negatives).
func TimeStr(seconds float64) string {
	if seconds < 0 {
		return "0:00"
	}
	total := int(seconds)
	m := total / 60
	s := total % 60
	return fmt.Sprintf("%d:%02d", m, s)
}

// TimeFmt renders a time in local time, or "" for a zero value.
func TimeFmt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// FmtFloat renders a float with one decimal place (for split boundaries).
func FmtFloat(f float64) string {
	return fmt.Sprintf("%.1f", f)
}

// Plural returns word when n is 1 and word+"s" otherwise, so templates can
// write "1 playlist" / "3 playlists" without an inline conditional.
func Plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// ActiveClass returns the player queue item class, marking the row that
// matches the split playback should start on.
func ActiveClass(active bool) string {
	if active {
		return "queue-item active"
	}
	return "queue-item"
}

// BoolAttr renders a boolean as the string "true" or "false" for data-*
// attributes.
func BoolAttr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ExpandVals builds the hx-vals JSON used by playlist forms so the fragment
// re-renders with the same playlist expanded after a POST.
func ExpandVals(id int64) string {
	return fmt.Sprintf(`{"expand": "%d"}`, id)
}

// RecordButtonLabel returns the text shown on a station's record button based
// on its current recording state.
func RecordButtonLabel(s models.StationViewRow) string {
	switch {
	case s.Recording:
		return "⚡ Recording"
	case s.Queued:
		return fmt.Sprintf("⏳ Queued (position %d for %s)", s.QueuePos, s.Domain)
	default:
		return "🔽 Record & Play"
	}
}

// RecordButtonTitle returns the title attribute for a station's record button.
func RecordButtonTitle(s models.StationViewRow) string {
	switch {
	case s.Recording:
		return "Already recording"
	case s.Queued:
		return "Queued for the next recording slot for " + s.Domain
	default:
		return "Record this station and play its songs"
	}
}

// RecordButtonDisabled reports whether a station's record button should be
// disabled (it is already recording or queued).
func RecordButtonDisabled(s models.StationViewRow) bool {
	return s.Recording || s.Queued
}

// HistorySummary builds the "N songs played — sorted by X, grouped by radio"
// line for the history view header.
func HistorySummary(total int, sort string, group bool) string {
	order := "recency"
	if sort == "frequency" {
		order = "frequency"
	}
	s := fmt.Sprintf("%d %s played — sorted by %s", total, Plural(total, "song"), order)
	if group {
		s += ", grouped by radio"
	}
	return s
}
