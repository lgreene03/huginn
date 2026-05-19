package journal

import (
	"bufio"
	"encoding/json"
	"os"

	"github.com/lgreene/huginn/internal/model"
	"github.com/lgreene/huginn/internal/portfolio"
)

// RecoverPortfolio reads a JSONL journal and reconstructs a portfolio state.
func RecoverPortfolio(path string, initialCash float64) (*portfolio.Portfolio, error) {
	port := portfolio.New(initialCash)

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Start fresh if no journal exists
			return port, nil
		}
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var fill model.Fill
		if err := json.Unmarshal(scanner.Bytes(), &fill); err != nil {
			return nil, err
		}
		port.ApplyFill(fill)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return port, nil
}
