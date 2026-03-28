package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// HFDL frequency data
// ---------------------------------------------------------------------------

type gsFrequency struct {
	FreqKHz  int  `json:"freq_khz"`
	Timeslot int  `json:"timeslot"`
	Enabled  bool `json:"enabled"`
}

type groundStation struct {
	GSID     int           `json:"gs_id"`
	Location string        `json:"location"`
	Lat      float64       `json:"lat"`
	Lon      float64       `json:"lon"`
	Freqs    []gsFrequency `json:"frequencies"`
}

// ---------------------------------------------------------------------------
// IQ mode table
// ---------------------------------------------------------------------------

// iqModeInfo holds the sample rate (Hz) and usable bandwidth (kHz) for an IQ mode.
type iqModeInfo struct {
	sampleRateHz int
	bandwidthKHz int
}

// launcherModes lists the IQ modes available to the launcher, in ascending
// bandwidth order.  iq384 is intentionally excluded.
var launcherModes = []struct {
	name string
	info iqModeInfo
}{
	{"iq48", iqModeInfo{sampleRateHz: 48000, bandwidthKHz: 48}},
	{"iq96", iqModeInfo{sampleRateHz: 96000, bandwidthKHz: 96}},
	{"iq192", iqModeInfo{sampleRateHz: 192000, bandwidthKHz: 192}},
}

// iqModes is kept for buildArgs lookups by mode name.
var iqModes = map[string]iqModeInfo{
	"iq48":  {sampleRateHz: 48000, bandwidthKHz: 48},
	"iq96":  {sampleRateHz: 96000, bandwidthKHz: 96},
	"iq192": {sampleRateHz: 192000, bandwidthKHz: 192},
}

// ---------------------------------------------------------------------------
// Frequency grouping
// ---------------------------------------------------------------------------

// freqGroup is a set of HFDL frequencies that fit within one IQ window.
type freqGroup struct {
	centerKHz int
	freqsKHz  []int
	iqMode    string
}

// groupFrequencies packs sorted unique frequencies into windows using a greedy
// left-to-right sweep, selecting the smallest IQ mode that covers each cluster.
func groupFrequencies(freqs []int) []freqGroup {
	if len(freqs) == 0 {
		return nil
	}

	var groups []freqGroup
	i := 0

	for i < len(freqs) {
		anchor := freqs[i]

		chosenMode := launcherModes[len(launcherModes)-1]
		prevCount := 0
		for _, m := range launcherModes {
			windowEnd := anchor + m.info.bandwidthKHz
			count := 0
			for j := i; j < len(freqs) && freqs[j] <= windowEnd; j++ {
				count++
			}
			if count > prevCount {
				chosenMode = m
				prevCount = count
			} else {
				break
			}
		}

		windowEnd := anchor + chosenMode.info.bandwidthKHz
		var members []int
		for i < len(freqs) && freqs[i] <= windowEnd {
			members = append(members, freqs[i])
			i++
		}

		lo := members[0]
		hi := members[len(members)-1]
		half := chosenMode.info.bandwidthKHz / 2
		center := (lo + hi) / 2

		if center-half > lo {
			center = lo + half
		}
		if center+half < hi {
			center = hi - half
		}

		groups = append(groups, freqGroup{
			centerKHz: center,
			freqsKHz:  members,
			iqMode:    chosenMode.name,
		})
	}

	return groups
}

// ---------------------------------------------------------------------------
// Fetch frequencies
// ---------------------------------------------------------------------------

// fetchResult holds the output of fetchFrequencies.
type fetchResult struct {
	Freqs         []int           // sorted unique enabled frequencies in kHz
	DisabledFreqs []int           // sorted unique disabled frequencies in kHz
	GSNames       map[int]string  // gs_id → location name (all stations)
	FreqGSID      map[int][]int   // freq_khz → sorted list of gs_ids that use it
	Stations      []groundStation // all ground stations sorted by gs_id
}

// fetchFrequencies fetches the HFDL ground station JSONL and returns:
//   - a sorted list of unique frequencies in kHz (filtered by stationIDs if set)
//   - a map of all gs_id → location name (unfiltered)
//   - a map of freq_khz → []gs_id for all stations (unfiltered)
func fetchFrequencies(freqURL string, stationIDs map[int]bool) (fetchResult, error) {
	var scanner *bufio.Scanner

	if strings.HasPrefix(freqURL, "file://") {
		path := strings.TrimPrefix(freqURL, "file://")
		f, err := os.Open(path)
		if err != nil {
			return fetchResult{}, fmt.Errorf("open %s: %w", path, err)
		}
		defer f.Close()
		scanner = bufio.NewScanner(f)
	} else {
		resp, err := http.Get(freqURL) //nolint:noctx
		if err != nil {
			return fetchResult{}, fmt.Errorf("fetch %s: %w", freqURL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fetchResult{}, fmt.Errorf("fetch %s: HTTP %d", freqURL, resp.StatusCode)
		}
		scanner = bufio.NewScanner(resp.Body)
	}

	seen := make(map[int]bool)
	seenDisabled := make(map[int]bool)
	gsNames := make(map[int]string)
	freqGSID := make(map[int]map[int]bool) // freq_khz → set of gs_ids
	var allStations []groundStation
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var gs groundStation
		if err := json.Unmarshal([]byte(line), &gs); err != nil {
			return fetchResult{}, fmt.Errorf("parse line: %w", err)
		}
		// Always record name, freq→gs mapping, and full station list
		if gs.Location != "" {
			gsNames[gs.GSID] = gs.Location
		}
		allStations = append(allStations, gs)
		for _, f := range gs.Freqs {
			if !f.Enabled {
				seenDisabled[f.FreqKHz] = true
				continue
			}
			if freqGSID[f.FreqKHz] == nil {
				freqGSID[f.FreqKHz] = make(map[int]bool)
			}
			freqGSID[f.FreqKHz][gs.GSID] = true
		}
		// Only include frequencies for filtered stations
		if len(stationIDs) > 0 && !stationIDs[gs.GSID] {
			continue
		}
		for _, f := range gs.Freqs {
			if !f.Enabled {
				continue
			}
			seen[f.FreqKHz] = true
		}
	}
	// Sort stations by ID
	sort.Slice(allStations, func(i, j int) bool {
		return allStations[i].GSID < allStations[j].GSID
	})
	if err := scanner.Err(); err != nil {
		return fetchResult{}, fmt.Errorf("read response: %w", err)
	}

	freqs := make([]int, 0, len(seen))
	for f := range seen {
		freqs = append(freqs, f)
	}
	sort.Ints(freqs)

	// Collect disabled frequencies (exclude any that are also enabled somewhere)
	disabledFreqs := make([]int, 0, len(seenDisabled))
	for f := range seenDisabled {
		if !seen[f] {
			disabledFreqs = append(disabledFreqs, f)
		}
	}
	sort.Ints(disabledFreqs)

	// Convert freq→gs set to sorted slices
	freqGSIDSlice := make(map[int][]int, len(freqGSID))
	for fkhz, gsSet := range freqGSID {
		ids := make([]int, 0, len(gsSet))
		for id := range gsSet {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		freqGSIDSlice[fkhz] = ids
	}

	return fetchResult{Freqs: freqs, DisabledFreqs: disabledFreqs, GSNames: gsNames, FreqGSID: freqGSIDSlice, Stations: allStations}, nil
}
