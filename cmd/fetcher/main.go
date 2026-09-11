package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Ingest sources. Bulk dumps are the default: paging the REST endpoint costs
// many minutes per day of history, which makes it unusable for the longer,
// multi-regime window that docs/EDGE_VERDICT.md calls for.
const (
	sourceREST   = "rest"
	sourceVision = "vision"
)

// BinanceAggTrade represents a single aggregate trade record returned by Binance public API.
type BinanceAggTrade struct {
	TradeID      int64  `json:"a"`
	Price        string `json:"p"`
	Quantity     string `json:"q"`
	FirstTradeID int64  `json:"f"`
	LastTradeID  int64  `json:"l"`
	Timestamp    int64  `json:"T"`
	IsBuyerMaker bool   `json:"m"`
	BestMatch    bool   `json:"M"`
}

// WindowData accumulates trades for a single time slice.
type WindowData struct {
	StartTime time.Time
	EndTime   time.Time
	BuyVol    float64
	SellVol   float64
	PriceSum  float64 // Sum of Price * Quantity
}

func main() {
	symbolFlag := flag.String("symbol", "BTCUSDT", "Binance symbol to fetch (e.g., BTCUSDT, ETHUSDT)")
	startFlag := flag.String("start", "", "Start date (YYYY-MM-DD, default: 1 day ago)")
	endFlag := flag.String("end", "", "End date (YYYY-MM-DD, default: today)")
	windowFlag := flag.String("window", "1m", "Sliding window size (e.g., 1m, 5m, 15m)")
	outputFlag := flag.String("output", "data/historical_features.jsonl", "Path to output JSONL file")
	sourceFlag := flag.String("source", sourceVision,
		"Ingest source: \"vision\" for bulk data.binance.vision daily dumps (fast, use for multi-day backfills), "+
			"\"rest\" for the paged public API (slow: ~1000 trades per request)")
	flag.Parse()

	// Logger
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Parse date range
	var startTime, endTime time.Time
	var err error

	if *startFlag == "" {
		startTime = time.Now().AddDate(0, 0, -1)
	} else {
		startTime, err = time.Parse("2006-01-02", *startFlag)
		if err != nil {
			slog.Error("Invalid start date format, must be YYYY-MM-DD", "error", err)
			os.Exit(1)
		}
	}

	if *endFlag == "" {
		endTime = time.Now()
	} else {
		endTime, err = time.Parse("2006-01-02", *endFlag)
		if err != nil {
			slog.Error("Invalid end date format, must be YYYY-MM-DD", "error", err)
			os.Exit(1)
		}
	}

	// Parse window duration
	windowDuration, err := time.ParseDuration(*windowFlag)
	if err != nil {
		slog.Error("Invalid window duration format (e.g., 1m, 5m)", "error", err)
		os.Exit(1)
	}

	slog.Info("Starting Historical Data Fetcher",
		"symbol", *symbolFlag,
		"start", startTime.Format("2006-01-02 15:04:05"),
		"end", endTime.Format("2006-01-02 15:04:05"),
		"window", windowDuration.String(),
		"output", *outputFlag,
		"source", *sourceFlag,
	)

	// Ensure output directory exists
	if err := os.MkdirAll("data", 0o750); err != nil {
		slog.Error("Failed to create data directory", "error", err)
		os.Exit(1)
	}

	// Create output file
	outFile, err := os.Create(*outputFlag)
	if err != nil {
		slog.Error("Failed to create output file", "error", err)
		os.Exit(1)
	}
	defer func() { _ = outFile.Close() }()

	// Fetch trades & aggregate them
	err = fetchAndProcess(outFile, *symbolFlag, *sourceFlag, startTime, endTime, windowDuration)
	if err != nil {
		slog.Error("Fetching and processing failed", "error", err)
		os.Exit(1)
	}

	slog.Info("Historical fetching and processing completed successfully", "output", *outputFlag)
}

func fetchAndProcess(writer io.Writer, symbol, source string, start, end time.Time, window time.Duration) error {
	instrument := formatInstrument(symbol)
	agg := newAggregator(window)

	switch source {
	case sourceVision:
		if err := fetchVisionRange(agg, symbol, start, end); err != nil {
			return err
		}
	case sourceREST:
		if err := fetchREST(agg, symbol, start, end); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown --source %q (want %q or %q)", source, sourceREST, sourceVision)
	}

	return agg.write(writer, instrument, end)
}

// fetchVisionRange walks the date range one daily dump at a time. Bulk dumps
// are the only practical source for a multi-month backfill: see the note on
// visionBaseURL for the throughput comparison against the REST path.
func fetchVisionRange(agg *aggregator, symbol string, start, end time.Time) error {
	client := &http.Client{Timeout: 10 * time.Minute}

	var total int64
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		n, err := fetchVisionDay(client, symbol, day, agg.add)
		if err != nil {
			return err
		}
		total += n
		slog.Info("Loaded daily dump",
			"day", day.Format("2006-01-02"), "trades", n, "total", total)
	}

	slog.Info("Bulk load complete", "total_trades", total)
	return nil
}

// fetchREST pages the public aggTrades endpoint, chaining on fromId. Retained
// for short ranges and for cross-checking the bulk path; it fetches 1000
// records per request, so a single day costs many minutes.
func fetchREST(agg *aggregator, symbol string, start, end time.Time) error {
	client := &http.Client{Timeout: 10 * time.Second}

	startMs := start.UnixMilli()
	endMs := end.UnixMilli()

	var fromID int64 = -1

	for {
		baseURL := "https://api.binance.com/api/v3/aggTrades"
		params := url.Values{}
		params.Add("symbol", symbol)
		params.Add("limit", "1000")

		if fromID != -1 {
			params.Add("fromId", strconv.FormatInt(fromID, 10))
		} else {
			params.Add("startTime", strconv.FormatInt(startMs, 10))
			// Binance rejects a startTime/endTime span wider than 1 hour when
			// fromId is omitted; after the first request we chain on fromId.
			params.Add("endTime", strconv.FormatInt(startMs+3600000, 10))
		}

		reqURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())
		slog.Debug("Querying Binance API", "url", reqURL)

		var trades []BinanceAggTrade
		var resp *http.Response
		var err error

		// Retry logic up to 5 times
		for attempt := 1; attempt <= 5; attempt++ {
			resp, err = client.Get(reqURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				break
			}
			if err == nil && resp.StatusCode == http.StatusTooManyRequests {
				slog.Warn("Rate limited (429) by Binance, sleeping...", "attempt", attempt)
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
				continue
			}
			slog.Warn("HTTP request failed, retrying...", "attempt", attempt, "error", err)
			time.Sleep(time.Duration(attempt) * time.Second)
		}

		if err != nil {
			return fmt.Errorf("failed to fetch trades after retries: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return fmt.Errorf("unexpected HTTP status code: %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}

		if err := json.Unmarshal(body, &trades); err != nil {
			return fmt.Errorf("failed to unmarshal JSON: %w", err)
		}

		if len(trades) == 0 {
			slog.Info("No more trades returned by Binance. Finishing.")
			break
		}

		slog.Info("Fetched batch of trades", "count", len(trades),
			"first_id", trades[0].TradeID, "last_id", trades[len(trades)-1].TradeID)

		var maxTimestamp int64
		for _, trade := range trades {
			maxTimestamp = trade.Timestamp
			if trade.Timestamp > endMs {
				break
			}

			p, err := strconv.ParseFloat(trade.Price, 64)
			if err != nil {
				continue
			}
			q, err := strconv.ParseFloat(trade.Quantity, 64)
			if err != nil {
				continue
			}

			agg.add(time.UnixMilli(trade.Timestamp), p, q, trade.IsBuyerMaker)
		}

		if maxTimestamp > endMs {
			slog.Info("Reached the end date limit. Stopping fetch.")
			break
		}

		fromID = trades[len(trades)-1].TradeID + 1

		// Sleep briefly to avoid getting rate-limited
		time.Sleep(100 * time.Millisecond)
	}

	return nil
}

func formatInstrument(symbol string) string {
	// Standardize e.g. BTCUSDT -> BTC-USD, ETHUSDT -> ETH-USD
	s := strings.ToUpper(symbol)
	if strings.HasSuffix(s, "USDT") {
		return strings.TrimSuffix(s, "USDT") + "-USD"
	}
	if strings.HasSuffix(s, "BUSD") {
		return strings.TrimSuffix(s, "BUSD") + "-USD"
	}
	return s
}
