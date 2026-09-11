package main

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Binance publishes daily aggTrade dumps at data.binance.vision. One day of
// BTCUSDT is ~15MB zipped (~1M trades) and downloads in seconds, where paging
// the REST aggTrades endpoint at 1000 records per request takes many minutes
// for the same day. Bulk is the only practical source for a multi-month window.
const visionBaseURL = "https://data.binance.vision/data/spot/daily/aggTrades"

// visionTSThreshold separates millisecond from microsecond epochs. Older dumps
// use milliseconds (~1.7e12); dumps from 2025 onward use microseconds (~1.7e15).
// Anything above this is treated as microseconds. The REST path is unaffected:
// the API still returns milliseconds.
const visionTSThreshold = 1e14

// fetchVisionDay downloads one daily aggTrades dump and feeds every trade to
// sink. Trades are streamed straight out of the zip: an 86MB CSV is never held
// in memory, only the per-window accumulator is.
func fetchVisionDay(client *http.Client, symbol string, day time.Time,
	sink func(ts time.Time, price, qty float64, isBuyerMaker bool),
) (int64, error) {
	name := fmt.Sprintf("%s-aggTrades-%s", symbol, day.Format("2006-01-02"))
	url := fmt.Sprintf("%s/%s/%s.zip", visionBaseURL, symbol, name)

	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// A missing day is normal at the edges of the range (today's dump is
		// not published until the day closes). Skip rather than abort so a
		// long backfill is not lost to one absent file.
		slog.Warn("No dump published for day, skipping", "day", day.Format("2006-01-02"))
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download %s: status %d", name, resp.StatusCode)
	}

	// archive/zip needs io.ReaderAt, so the compressed file is spooled to a
	// temp file. Only the ~15MB zip touches disk, never the expanded CSV.
	tmp, err := os.CreateTemp("", "aggtrades-*.zip")
	if err != nil {
		return 0, fmt.Errorf("temp file: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	size, err := io.Copy(tmp, resp.Body)
	if err != nil {
		return 0, fmt.Errorf("spool %s: %w", name, err)
	}

	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return 0, fmt.Errorf("open zip %s: %w", name, err)
	}
	if len(zr.File) == 0 {
		return 0, fmt.Errorf("zip %s is empty", name)
	}

	rc, err := zr.File[0].Open()
	if err != nil {
		return 0, fmt.Errorf("open member of %s: %w", name, err)
	}
	defer func() { _ = rc.Close() }()

	return parseAggTradesCSV(rc, sink)
}

// parseAggTradesCSV reads the headerless aggTrades layout:
//
//	id, price, qty, firstId, lastId, timestamp, isBuyerMaker, isBestMatch
//
// Fields are split by hand rather than with encoding/csv: this runs over tens
// of millions of rows per backfill and the format has no quoting to honour.
func parseAggTradesCSV(r io.Reader, sink func(time.Time, float64, float64, bool)) (int64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var n int64
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}

		// Some dumps carry a header row; detect it by a non-numeric first field.
		if n == 0 && (line[0] < '0' || line[0] > '9') {
			continue
		}

		f := strings.Split(line, ",")
		if len(f) < 7 {
			continue
		}

		price, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		qty, err := strconv.ParseFloat(f[2], 64)
		if err != nil {
			continue
		}
		rawTS, err := strconv.ParseInt(f[5], 10, 64)
		if err != nil {
			continue
		}

		var ts time.Time
		if float64(rawTS) > visionTSThreshold {
			ts = time.UnixMicro(rawTS)
		} else {
			ts = time.UnixMilli(rawTS)
		}

		// Dumps render the flag as True/False rather than the API's JSON bool.
		isBuyerMaker := f[6] == "True" || f[6] == "true" || f[6] == "1"

		sink(ts, price, qty, isBuyerMaker)
		n++
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("scan aggTrades: %w", err)
	}
	return n, nil
}
