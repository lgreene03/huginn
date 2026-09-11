package main

import (
	"strings"
	"testing"
	"time"
)

// The dumps carry no header and render the maker flag as True/False rather than
// the API's JSON bool, so the parser is exercised against the real layout:
// id, price, qty, firstId, lastId, timestamp, isBuyerMaker, isBestMatch
const sampleCSV = `4051360437,78581.30000000,0.00012000,6642078844,6642078844,1788220800322740,False,True
4051360438,78581.40000000,0.01700000,6642078845,6642078845,1788220800491646,True,True
4051360439,78581.50000000,0.00056000,6642078846,6642078846,1788220860533037,False,True
`

func TestParseAggTradesCSV(t *testing.T) {
	type row struct {
		ts           time.Time
		price, qty   float64
		isBuyerMaker bool
	}
	var got []row

	n, err := parseAggTradesCSV(strings.NewReader(sampleCSV),
		func(ts time.Time, price, qty float64, isBuyerMaker bool) {
			got = append(got, row{ts, price, qty, isBuyerMaker})
		})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n != 3 || len(got) != 3 {
		t.Fatalf("want 3 trades, got n=%d len=%d", n, len(got))
	}

	if got[0].price != 78581.3 || got[0].qty != 0.00012 {
		t.Errorf("row 0: price/qty = %v/%v", got[0].price, got[0].qty)
	}
	// True means the buyer was the maker, so the aggressor sold.
	if got[0].isBuyerMaker || !got[1].isBuyerMaker {
		t.Errorf("maker flags = %v, %v; want false, true", got[0].isBuyerMaker, got[1].isBuyerMaker)
	}
	// 1788220800322740 is microseconds, not milliseconds. Misreading it as ms
	// would place the trade in 1970 and silently produce an empty fixture.
	if got[0].ts.UTC().Year() != 2026 {
		t.Errorf("timestamp decoded to %s, want a 2026 instant", got[0].ts.UTC())
	}
}

func TestParseAggTradesCSVSkipsHeader(t *testing.T) {
	withHeader := "agg_trade_id,price,quantity,first_trade_id,last_trade_id,transact_time,is_buyer_maker,is_best_match\n" + sampleCSV
	n, err := parseAggTradesCSV(strings.NewReader(withHeader), func(time.Time, float64, float64, bool) {})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n != 3 {
		t.Fatalf("want 3 trades past the header, got %d", n)
	}
}

func TestParseAggTradesCSVMillisecondFallback(t *testing.T) {
	// Older dumps use millisecond epochs; both must land in the same era.
	msRow := "1,100.0,1.0,1,1,1788220800322,False,True\n"
	var ts time.Time
	if _, err := parseAggTradesCSV(strings.NewReader(msRow),
		func(t2 time.Time, _, _ float64, _ bool) { ts = t2 }); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ts.UTC().Year() != 2026 {
		t.Errorf("millisecond timestamp decoded to %s, want 2026", ts.UTC())
	}
}

// The two ingest paths must fold trades identically, otherwise a fixture built
// from bulk dumps is not comparable to one built from the API.
func TestAggregatorFoldsTakerSide(t *testing.T) {
	agg := newAggregator(time.Minute)
	base := time.Date(2026, 5, 18, 1, 0, 0, 0, time.UTC)

	agg.add(base.Add(10*time.Second), 100, 3, false) // taker buy
	agg.add(base.Add(20*time.Second), 100, 1, true)  // taker sell
	agg.add(base.Add(90*time.Second), 200, 2, true)  // next window

	if len(agg.windows) != 2 {
		t.Fatalf("want 2 windows, got %d", len(agg.windows))
	}
	w := agg.windows[base]
	if w.BuyVol != 3 || w.SellVol != 1 {
		t.Fatalf("buy/sell = %v/%v, want 3/1", w.BuyVol, w.SellVol)
	}
	// obi = (3-1)/4 = 0.5, and vpin is currently defined as |obi|.
	if gotOBI := (w.BuyVol - w.SellVol) / (w.BuyVol + w.SellVol); gotOBI != 0.5 {
		t.Errorf("obi = %v, want 0.5", gotOBI)
	}
}
