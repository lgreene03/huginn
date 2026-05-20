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

	"github.com/lgreene03/huginn/internal/model"
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
	)

	// Ensure output directory exists
	if err := os.MkdirAll("data", 0755); err != nil {
		slog.Error("Failed to create data directory", "error", err)
		os.Exit(1)
	}

	// Create output file
	outFile, err := os.Create(*outputFlag)
	if err != nil {
		slog.Error("Failed to create output file", "error", err)
		os.Exit(1)
	}
	defer outFile.Close()

	// Fetch trades & aggregate them
	err = fetchAndProcess(outFile, *symbolFlag, startTime, endTime, windowDuration)
	if err != nil {
		slog.Error("Fetching and processing failed", "error", err)
		os.Exit(1)
	}

	slog.Info("Historical fetching and processing completed successfully", "output", *outputFlag)
}

func fetchAndProcess(writer io.Writer, symbol string, start, end time.Time, window time.Duration) error {
	client := &http.Client{Timeout: 10 * time.Second}
	encoder := json.NewEncoder(writer)

	startMs := start.UnixMilli()
	endMs := end.UnixMilli()

	var fromID int64 = -1
	var windowMap = make(map[time.Time]*WindowData)
	var minWindowStart time.Time

	// Map symbol name to standardized instrument (e.g., BTCUSDT -> BTC-USD)
	instrument := formatInstrument(symbol)

	for {
		// Construct Request URL
		baseURL := "https://api.binance.com/api/v3/aggTrades"
		params := url.Values{}
		params.Add("symbol", symbol)
		params.Add("limit", "1000")

		if fromID != -1 {
			params.Add("fromId", strconv.FormatInt(fromID, 10))
		} else {
			params.Add("startTime", strconv.FormatInt(startMs, 10))
			// Binance doesn't allow startTime and endTime to be more than 1 hour apart if fromId is omitted,
			// but we will pagination-chain by setting fromId after the first request.
			// Just in case there are no trades, we limit end time for first request to startMs + 1 hour
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
			resp.Body.Close()
			return fmt.Errorf("unexpected HTTP status code: %d", resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
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

		slog.Info("Fetched batch of trades", "count", len(trades), "first_id", trades[0].TradeID, "last_id", trades[len(trades)-1].TradeID)

		// Process trades in batch
		var maxTimestamp int64
		for _, trade := range trades {
			maxTimestamp = trade.Timestamp
			if trade.Timestamp > endMs {
				break
			}

			tradeTime := time.UnixMilli(trade.Timestamp)
			winStart := tradeTime.Truncate(window)
			winEnd := winStart.Add(window)

			if minWindowStart.IsZero() || winStart.Before(minWindowStart) {
				minWindowStart = winStart
			}

			wData, exists := windowMap[winStart]
			if !exists {
				wData = &WindowData{
					StartTime: winStart,
					EndTime:   winEnd,
				}
				windowMap[winStart] = wData
			}

			p, err := strconv.ParseFloat(trade.Price, 64)
			if err != nil {
				continue
			}
			q, err := strconv.ParseFloat(trade.Quantity, 64)
			if err != nil {
				continue
			}

			wData.PriceSum += p * q
			if trade.IsBuyerMaker {
				wData.SellVol += q // Taker sell
			} else {
				wData.BuyVol += q // Taker buy
			}
		}

		// Check if we crossed the end date
		if maxTimestamp > endMs {
			slog.Info("Reached the end date limit. Stopping fetch.")
			break
		}

		// Update fromID to point to the next trade
		fromID = trades[len(trades)-1].TradeID + 1

		// Sleep briefly to avoid getting rate-limited
		time.Sleep(100 * time.Millisecond)
	}

	// Write computed windows in chronological order
	slog.Info("Writing computed metrics to JSONL file...", "total_windows", len(windowMap))
	
	// We want to write windows sequentially. We start from minWindowStart and increment.
	currWindow := minWindowStart
	var writtenCount int
	for !currWindow.After(end) {
		wData, exists := windowMap[currWindow]
		if exists {
			totalVol := wData.BuyVol + wData.SellVol
			var vwap float64
			var obi float64
			var vpin float64

			if totalVol > 0 {
				vwap = wData.PriceSum / totalVol
				obi = (wData.BuyVol - wData.SellVol) / totalVol
				vpin = (wData.BuyVol - wData.SellVol)
				if vpin < 0 {
					vpin = -vpin
				}
				vpin = vpin / totalVol
			}

			event := model.FeatureEvent{
				EventID:        fmt.Sprintf("hist-%s-%d", instrument, wData.StartTime.Unix()),
				EventTime:      wData.EndTime,
				FeatureName:    "market_features",
				FeatureVersion: "v1",
				Instrument:     instrument,
				WindowStart:    wData.StartTime,
				WindowEnd:      wData.EndTime,
				Values: map[string]float64{
					"obi":        obi,
					"vpin":       vpin,
					"microPrice": vwap,
					"vwap":       vwap,
					"volume":     totalVol,
				},
			}

			if err := encoder.Encode(event); err != nil {
				return fmt.Errorf("failed to encode feature event: %w", err)
			}
			writtenCount++
		}
		currWindow = currWindow.Add(window)
	}

	slog.Info("Finished serialization", "written_events", writtenCount)
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
