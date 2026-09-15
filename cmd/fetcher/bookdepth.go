package main

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Binance publishes daily order-book depth dumps for USD-M futures at
// data.binance.vision. One day of BTCUSDT is ~0.5MB zipped, which is three
// orders of magnitude smaller than the same day of aggTrades, and that size
// difference is the first clue to what these files are: they are NOT per-level
// L2 books. See docs/BOOKDEPTH_FINDING.md for the full measurement.
//
// The layout is:
//
//	timestamp, percentage, depth, notional
//
// where each snapshot carries one row per percentage band, `depth` is the
// CUMULATIVE base-asset quantity resting between the mid price and that band,
// and `notional` is the matching quote-asset value. A negative percentage is
// the bid side, a positive percentage the ask side.
//
// IMPORTANT: this loader deliberately does NOT feed the trade aggregator in
// aggregate.go and does NOT emit an `obi` field. Band depth is not the quantity
// the live obi-bridge computes, and naming it `obi` would recreate the exact
// defect docs/ROADMAP.md exists to fix. It emits bands under their own names so
// the data can be inspected for what it actually is.
const bookDepthBaseURL = "https://data.binance.vision/data/futures/um/daily/bookDepth"

// bookDepthTimeLayout is the format every bookDepth dump inspected so far uses.
// Unlike aggTrades, which carries a numeric epoch, bookDepth renders a
// space-separated UTC datetime. The parser still accepts a numeric epoch as a
// fallback, because aggTrades switched units mid-history without notice and
// there is no guarantee bookDepth will not do the same.
const bookDepthTimeLayout = "2006-01-02 15:04:05"

// bookDepthTSThreshold separates millisecond from microsecond epochs, matching
// visionTSThreshold. It only applies to the numeric-epoch fallback.
const bookDepthTSThreshold = 1e14

// BookDepthBand is one percentage band of one snapshot. Percentage is signed:
// negative is the bid side, positive the ask side.
type BookDepthBand struct {
	Percentage float64 `json:"percentage"`
	Depth      float64 `json:"depth"`
	Notional   float64 `json:"notional"`
}

// BookDepthSnapshot is one timestamped set of bands, emitted as a single JSONL
// record. There is no `obi`, `vpin` or `microPrice` field, and there must not
// be: the dump does not carry the per-level detail those features need.
type BookDepthSnapshot struct {
	Instrument   string          `json:"instrument"`
	SnapshotTime time.Time       `json:"snapshotTime"`
	Bands        []BookDepthBand `json:"bands"`
}

// fetchBookDepthRange walks the date range one daily dump at a time and writes
// one JSONL snapshot record per timestamp, in chronological order. Two runs
// over the same range produce byte-identical output: days are visited in order,
// rows arrive in order within a day, and bands are sorted before emission, so
// nothing depends on map iteration order.
func fetchBookDepthRange(writer io.Writer, symbol, instrument string, start, end time.Time) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	encoder := json.NewEncoder(writer)

	var totalRows, totalSnapshots int64
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		var daySnapshots int64
		emit := func(snap BookDepthSnapshot) error {
			snap.Instrument = perpetualInstrument(instrument)
			daySnapshots++
			return encoder.Encode(snap)
		}

		n, err := fetchBookDepthDay(client, symbol, day, emit)
		if err != nil {
			return err
		}
		totalRows += n
		totalSnapshots += daySnapshots
		slog.Info("Loaded daily book-depth dump",
			"day", day.Format("2006-01-02"), "rows", n, "snapshots", daySnapshots,
			"total_rows", totalRows)
	}

	slog.Info("Book-depth load complete",
		"total_rows", totalRows, "total_snapshots", totalSnapshots)
	return nil
}

// perpetualInstrument tags the instrument as a USD-M perpetual. bookDepth is
// only published for futures, while the live obi-bridge reads the SPOT book, so
// an untagged "BTC-USD" record would be indistinguishable from a spot one.
// docs/ROADMAP.md requires this change of instrument to be stated wherever
// results are published rather than glossed, and the record is where it starts.
func perpetualInstrument(instrument string) string {
	if strings.HasSuffix(instrument, "-PERP") {
		return instrument
	}
	return instrument + "-PERP"
}

// fetchBookDepthDay downloads one daily bookDepth dump and hands every
// completed snapshot to emit. Rows are streamed straight out of the zip and
// only one snapshot's worth of bands is ever held in memory.
func fetchBookDepthDay(client *http.Client, symbol string, day time.Time,
	emit func(BookDepthSnapshot) error,
) (int64, error) {
	name := fmt.Sprintf("%s-bookDepth-%s", symbol, day.Format("2006-01-02"))
	url := fmt.Sprintf("%s/%s/%s.zip", bookDepthBaseURL, symbol, name)

	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("download %s: %w", name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// A missing day is normal at the edges of the range: today's dump is
		// not published until the day closes. Skip rather than abort so a long
		// backfill is not lost to one absent file.
		slog.Warn("No book-depth dump published for day, skipping",
			"day", day.Format("2006-01-02"))
		return 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download %s: status %d", name, resp.StatusCode)
	}

	// archive/zip needs io.ReaderAt, so the compressed file is spooled to a
	// temp file. Only the small zip touches disk, never the expanded CSV.
	tmp, err := os.CreateTemp("", "bookdepth-*.zip")
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

	return parseBookDepthCSV(rc, emit)
}

// parseBookDepthCSV reads the bookDepth layout:
//
//	timestamp, percentage, depth, notional
//
// Rows for one snapshot are contiguous and share a timestamp, so a snapshot is
// flushed as soon as the timestamp changes. It returns the number of data rows
// read, not the number of snapshots emitted.
//
// Two real-world variations are handled, both found by inspecting actual dumps
// rather than assumed. Percentages render as "-5" in 2023-2025 dumps and
// "-5.00" from late 2025, so they are parsed as floats and never compared as
// strings. The band set also widened from ten bands to twelve over the same
// period, so no fixed band count is required.
func parseBookDepthCSV(r io.Reader, emit func(BookDepthSnapshot) error) (int64, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	var rows int64
	var cur BookDepthSnapshot
	var haveCur bool

	flush := func() error {
		if !haveCur {
			return nil
		}
		// Sort by percentage so the record is independent of row order in the
		// dump. Without this, byte-identical reruns would rely on the upstream
		// file never being re-ordered.
		sort.Slice(cur.Bands, func(i, j int) bool {
			return cur.Bands[i].Percentage < cur.Bands[j].Percentage
		})
		if err := emit(cur); err != nil {
			return err
		}
		haveCur = false
		cur = BookDepthSnapshot{}
		return nil
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}

		f := strings.Split(line, ",")
		if len(f) < 4 {
			continue
		}

		ts, ok := parseBookDepthTime(strings.TrimSpace(f[0]))
		if !ok {
			// The header row lands here, as does any malformed line.
			continue
		}
		pct, err := strconv.ParseFloat(strings.TrimSpace(f[1]), 64)
		if err != nil {
			continue
		}
		depth, err := strconv.ParseFloat(strings.TrimSpace(f[2]), 64)
		if err != nil {
			continue
		}
		notional, err := strconv.ParseFloat(strings.TrimSpace(f[3]), 64)
		if err != nil {
			continue
		}

		if haveCur && !ts.Equal(cur.SnapshotTime) {
			if err := flush(); err != nil {
				return rows, err
			}
		}
		if !haveCur {
			cur = BookDepthSnapshot{SnapshotTime: ts}
			haveCur = true
		}
		cur.Bands = append(cur.Bands, BookDepthBand{
			Percentage: pct, Depth: depth, Notional: notional,
		})
		rows++
	}

	if err := sc.Err(); err != nil {
		return rows, fmt.Errorf("scan bookDepth: %w", err)
	}
	if err := flush(); err != nil {
		return rows, err
	}
	return rows, nil
}

// parseBookDepthTime accepts the datetime layout every observed dump uses, and
// falls back to a numeric epoch with millisecond/microsecond auto-detection.
// The fallback exists because the aggTrades dumps silently switched from
// milliseconds to microseconds mid-history; misreading the unit puts every
// record in 1970 and yields an empty output rather than an error.
func parseBookDepthTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	if raw, err := strconv.ParseInt(s, 10, 64); err == nil {
		if float64(raw) > bookDepthTSThreshold {
			return time.UnixMicro(raw).UTC(), true
		}
		return time.UnixMilli(raw).UTC(), true
	}

	ts, err := time.ParseInLocation(bookDepthTimeLayout, s, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}
