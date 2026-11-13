package library

import (
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func validateMonitor(mode string) error {
	switch mode {
	case "", "all", "latest", "future", "new_seasons", "first_season", "none":
		return nil
	default:
		return fmt.Errorf("%w: unknown monitoring mode %q", ErrInvalidInput, mode)
	}
}

// applyMonitorPreset applies an explicit choice once and saves its boundary.
// Refresh never reapplies it to existing rows: their checkboxes are overrides.
func applyMonitorPreset(item *domain.MediaItem, mode string) {
	if item.Kind != domain.KindSeries {
		return
	}
	if mode == "" {
		mode = "all"
	}
	item.Monitor, item.MonitorSince = mode, time.Now().UTC().Format(time.DateOnly)
	item.MonitorSeason = 0
	if mode == "first_season" {
		// First season only (ADR 0028): the lowest real season, or season 1
		// when none is known yet. Later seasons arrive unmonitored.
		item.MonitorSeason = 1
		lowest := 0
		for _, season := range item.Seasons {
			if season.Number > 0 && (lowest == 0 || season.Number < lowest) {
				lowest = season.Number
			}
		}
		if lowest > 0 {
			item.MonitorSeason = lowest
		}
	}
	for _, season := range item.Seasons {
		if mode == "latest" && season.Number > item.MonitorSeason {
			item.MonitorSeason = season.Number
		}
		if mode == "new_seasons" {
			for _, e := range season.Episodes {
				if e.AirDate != "" && e.AirDate < item.MonitorSince && season.Number >= item.MonitorSeason {
					item.MonitorSeason = season.Number + 1
				}
			}
		}
	}
	for i := range item.Seasons {
		season := &item.Seasons[i]
		season.Monitored = monitorNewSeason(*item, season.Number)
		for j := range season.Episodes {
			season.Episodes[j].Monitored = season.Monitored && monitorNewEpisode(*item, season.Episodes[j])
		}
	}
}

func monitorNewSeason(item domain.MediaItem, number int) bool {
	if number == 0 || item.Monitor == "none" {
		return false
	}
	if item.Monitor == "first_season" {
		return number == item.MonitorSeason
	}
	if item.Monitor == "latest" || item.Monitor == "new_seasons" {
		return number >= item.MonitorSeason
	}
	return true
}

func monitorNewEpisode(item domain.MediaItem, e domain.Episode) bool {
	// Unknown dates remain selected so that announcing an air date later does
	// not strand an episode. Acquisition waits for a known, elapsed air date.
	return item.Monitor != "future" || e.AirDate == "" || e.AirDate >= item.MonitorSince
}
