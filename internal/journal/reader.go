package journal

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"

	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

// typeProbe sniffs the `type` discriminator. Records that omit it are
// legacy bare-Fill lines from pre-Phase-1 journals.
type typeProbe struct {
	Type string `json:"type"`
}

// RecoverPortfolio reads a JSONL journal and reconstructs a portfolio state.
// Strategy-state records (type=="strategy_state") are skipped here — they are
// loaded via LoadStrategyStateFromJSONL on demand.
func RecoverPortfolio(path string, initialCash float64) (*portfolio.Portfolio, error) {
	port := portfolio.New(initialCash)

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Start fresh if no journal exists
			return port, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Strategy-state blobs can be larger than the default 64KB scanner buffer
	// once we accumulate decayed-price history. 1MB is comfortable headroom.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		var probe typeProbe
		if err := json.Unmarshal(line, &probe); err == nil && probe.Type == "strategy_state" {
			continue
		}
		var fill model.Fill
		if err := json.Unmarshal(line, &fill); err != nil {
			return nil, err
		}
		port.ApplyFill(fill)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return port, nil
}

// LoadStrategyStateFromJSONL scans the journal and returns the most recent
// strategy-state blob for the given key. Returns (nil, nil) when no matching
// record exists, in which case the caller should start the strategy fresh.
func LoadStrategyStateFromJSONL(path, key string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var latest []byte
	for scanner.Scan() {
		line := scanner.Bytes()
		var probe typeProbe
		if err := json.Unmarshal(line, &probe); err != nil || probe.Type != "strategy_state" {
			continue
		}
		var rec strategyStateRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.StrategyKey != key {
			continue
		}
		// Copy: scanner reuses its underlying buffer on the next Scan().
		latest = append(latest[:0], rec.Blob...)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return latest, nil
}
