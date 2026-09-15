package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Verbatim head of BTCUSDT-bookDepth-2026-09-01.csv. The modern dumps carry a
// header, render percentages with two decimals, and cover twelve bands.
const sampleBookDepth2026 = `timestamp,percentage,depth,notional
2026-09-01 00:00:04,-5.00,10267.92200000,788466627.08110000
2026-09-01 00:00:04,-4.00,8967.16600000,690917971.29840000
2026-09-01 00:00:04,-3.00,6391.67300000,495652738.52610000
2026-09-01 00:00:04,-2.00,5172.11200000,402217876.47380000
2026-09-01 00:00:04,-1.00,2393.25400000,187143664.21670000
2026-09-01 00:00:04,-0.20,813.69100000,63843569.41780000
2026-09-01 00:00:04,0.20,410.56800000,32283149.59130000
2026-09-01 00:00:04,1.00,1895.92700000,149678592.00540000
2026-09-01 00:00:04,2.00,4832.06100000,383685577.93930000
2026-09-01 00:00:04,3.00,5397.92300000,429238407.34130000
2026-09-01 00:00:04,4.00,6219.64400000,496091150.93760000
2026-09-01 00:00:04,5.00,7103.90400000,568607291.79200000
2026-09-01 00:00:31,-5.00,10375.57500000,796845570.40630000
2026-09-01 00:00:31,-0.20,876.46600000,68764580.11590000
2026-09-01 00:00:31,0.20,397.55200000,31254659.07540000
2026-09-01 00:00:31,5.00,7243.93700000,579666803.28680000
`

// Verbatim head of BTCUSDT-bookDepth-2023-05-16.csv. The older dumps render
// percentages without decimals and have no 0.20 band at all.
const sampleBookDepth2023 = `timestamp,percentage,depth,notional
2023-05-16 00:25:31,-5,13195.57600000,352665919.22385000
2023-05-16 00:25:31,-4,11989.49400000,321256286.98499000
2023-05-16 00:25:31,-3,10026.52500000,269615051.59955000
`

func collectBookDepth(t *testing.T, csv string) ([]BookDepthSnapshot, int64) {
	t.Helper()
	var got []BookDepthSnapshot
	n, err := parseBookDepthCSV(strings.NewReader(csv), func(s BookDepthSnapshot) error {
		got = append(got, s)
		return nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return got, n
}

func TestParseBookDepthCSVGroupsSnapshots(t *testing.T) {
	got, n := collectBookDepth(t, sampleBookDepth2026)

	if n != 16 {
		t.Fatalf("want 16 data rows, got %d", n)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 snapshots, got %d", len(got))
	}
	if len(got[0].Bands) != 12 || len(got[1].Bands) != 4 {
		t.Fatalf("band counts = %d/%d, want 12/4", len(got[0].Bands), len(got[1].Bands))
	}

	// The header row must not be mistaken for data.
	want := time.Date(2026, 9, 1, 0, 0, 4, 0, time.UTC)
	if !got[0].SnapshotTime.Equal(want) {
		t.Errorf("snapshot time = %s, want %s", got[0].SnapshotTime, want)
	}

	// Bands are sorted ascending so reruns cannot depend on dump row order.
	for i := 1; i < len(got[0].Bands); i++ {
		if got[0].Bands[i].Percentage <= got[0].Bands[i-1].Percentage {
			t.Fatalf("bands not sorted ascending at %d: %v", i, got[0].Bands)
		}
	}
	if got[0].Bands[0].Percentage != -5 || got[0].Bands[11].Percentage != 5 {
		t.Errorf("band range = %v..%v, want -5..5",
			got[0].Bands[0].Percentage, got[0].Bands[11].Percentage)
	}
	if got[0].Bands[0].Depth != 10267.922 {
		t.Errorf("depth = %v, want 10267.922", got[0].Bands[0].Depth)
	}
	if got[0].Bands[0].Notional != 788466627.0811 {
		t.Errorf("notional = %v, want 788466627.0811", got[0].Bands[0].Notional)
	}
}

// The 2023 dumps write "-5" where the 2026 dumps write "-5.00", so percentages
// must be parsed as numbers. String-matching the band label would silently drop
// every row of the older half of history.
func TestParseBookDepthCSVLegacyPercentageFormat(t *testing.T) {
	got, n := collectBookDepth(t, sampleBookDepth2023)

	if n != 3 || len(got) != 1 {
		t.Fatalf("want 3 rows in 1 snapshot, got n=%d snapshots=%d", n, len(got))
	}
	if got[0].Bands[0].Percentage != -5 {
		t.Errorf("percentage = %v, want -5", got[0].Bands[0].Percentage)
	}
	if got[0].SnapshotTime.UTC().Year() != 2023 {
		t.Errorf("timestamp decoded to %s, want a 2023 instant", got[0].SnapshotTime.UTC())
	}
}

// bookDepth carries a datetime string, but aggTrades switched from millisecond
// to microsecond epochs mid-history without notice. Misreading the unit places
// every record in 1970 and produces a silently empty output rather than an
// error, so both epoch units are covered.
func TestParseBookDepthTimeUnits(t *testing.T) {
	cases := []struct {
		name string
		in   string
		year int
	}{
		{"datetime layout", "2026-09-01 00:00:04", 2026},
		{"millisecond epoch", "1788220800322", 2026},
		{"microsecond epoch", "1788220800322740", 2026},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ts, ok := parseBookDepthTime(c.in)
			if !ok {
				t.Fatalf("parseBookDepthTime(%q) reported failure", c.in)
			}
			if ts.UTC().Year() != c.year {
				t.Errorf("%q decoded to %s, want year %d", c.in, ts.UTC(), c.year)
			}
		})
	}

	for _, bad := range []string{"", "timestamp", "not-a-time"} {
		if _, ok := parseBookDepthTime(bad); ok {
			t.Errorf("parseBookDepthTime(%q) accepted a non-timestamp", bad)
		}
	}
}

// bookDepth exists only for futures, but the live obi-bridge reads the spot
// book. An untagged "BTC-USD" record would hide that substitution.
func TestPerpetualInstrumentIsTagged(t *testing.T) {
	if got := perpetualInstrument("BTC-USD"); got != "BTC-USD-PERP" {
		t.Errorf("perpetualInstrument(BTC-USD) = %q, want BTC-USD-PERP", got)
	}
	if got := perpetualInstrument("BTC-USD-PERP"); got != "BTC-USD-PERP" {
		t.Errorf("perpetualInstrument is not idempotent: got %q", got)
	}
}

// Reading the same bytes twice must produce the same records. This is the
// in-process half of the P1 exit criterion; the network half is evidenced in
// docs/BOOKDEPTH_FINDING.md.
func TestParseBookDepthCSVIsReproducible(t *testing.T) {
	render := func() []byte {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		_, err := parseBookDepthCSV(strings.NewReader(sampleBookDepth2026),
			func(s BookDepthSnapshot) error {
				s.Instrument = "BTC-USD-PERP"
				return enc.Encode(s)
			})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return buf.Bytes()
	}

	if first, second := render(), render(); !bytes.Equal(first, second) {
		t.Errorf("two passes differ:\nfirst:  %s\nsecond: %s", first, second)
	}
}

// KILL-CRITERION GUARD. docs/ROADMAP.md ends Track A at P1 if the bookDepth
// dumps cannot reconstruct the live obi, and they cannot: the finest band is
// 0.2% of mid where the live path sums the top 10 discrete levels, a span of
// roughly 0.002%. The one failure mode that would undo that finding is someone
// quietly relabelling band depth as `obi`, so this test fails if the emitted
// record ever grows a feature-shaped field. Do not "fix" it by renaming the
// field; see docs/BOOKDEPTH_FINDING.md.
func TestBookDepthSnapshotCarriesNoDerivedFeatures(t *testing.T) {
	got, _ := collectBookDepth(t, sampleBookDepth2026)
	if len(got) == 0 {
		t.Fatal("no snapshots parsed")
	}

	blob, err := json.Marshal(got[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, banned := range []string{"obi", "vpin", "microPrice", "vwap"} {
		if _, present := fields[banned]; present {
			t.Errorf("BookDepthSnapshot exposes %q; percentage-band depth is not "+
				"the live order-book imbalance and must not be published as a feature", banned)
		}
	}
}
