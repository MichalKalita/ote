package webserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MichalKalita/ote/storage"
)

// seedPragueDay stores 96 quarters with monotonically increasing prices
// (price = quarter index) for the given Prague-local date.
func seedPragueDay(t *testing.T, state *AppState, day time.Time) {
	t.Helper()
	quarters := make([]storage.Quarter, 96)
	for i := range quarters {
		quarters[i] = storage.Quarter{
			Ts:    day.Add(time.Duration(i) * 15 * time.Minute).UTC(),
			Price: float32(i),
		}
	}
	if err := state.db.SaveQuarters(quarters); err != nil {
		t.Fatalf("seed %s: %v", day.Format("2006-01-02"), err)
	}
}

// TestBuildNowQuarters_QuarterPricesArePassthrough verifies that quarter-hour
// prices are converted correctly (with distribution surcharge when applicable).
func TestBuildNowQuarters_QuarterPricesArePassthrough(t *testing.T) {
	state := openTestState(t)
	loc, _ := time.LoadLocation("Europe/Prague")
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, loc)
	seedPragueDay(t, state, day.AddDate(0, 0, -1))
	seedPragueDay(t, state, day)

	now := time.Date(2026, 5, 10, 12, 30, 0, 0, loc)
	rows := buildNowQuarters(state, now, CurrencyEur, false)

	// rows contains all quarters from yesterday + today = 96 + 96 = 192 rows.
	// Today quarter 20 = today hour 5, quarter 0 → price should be 20.
	todayStart := 96
	q20 := rows[todayStart+20]
	if got := q20.Price; got != 20 {
		t.Errorf("today quarter 20 price: got %v, want 20", got)
	}
	if q20.Hour != 5 || q20.Quarter != 0 {
		t.Errorf("quarter 20 offset: got hour=%d quarter=%d, want hour=5 quarter=0",
			q20.Hour, q20.Quarter)
	}
}


// TestBuildNowRows_DistributionAddedPerHour confirms that the high-tariff hours
// pick up the higher surcharge — this is what makes "CZK incl. distribution"
// the right default for the page.
func TestBuildNowQuarters_DistributionAddedPerQuarter(t *testing.T) {
	state := openTestState(t)
	loc, _ := time.LoadLocation("Europe/Prague")
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, loc)
	seedPragueDay(t, state, day.AddDate(0, 0, -1))
	seedPragueDay(t, state, day)

	now := time.Date(2026, 5, 10, 12, 0, 0, 0, loc)
	rows := buildNowQuarters(state, now, CurrencyEur, true)

	// Verify that distribution surcharge differs between high/low hours.
	// Today quarter 40 = hour 10, quarter 0 (high tariff)
	// Today quarter 36 = hour 9, quarter 0 (low tariff)
	todayStart := 96
	q36Low := rows[todayStart+36]
	q40High := rows[todayStart+40]

	expectedDiff := state.Distribution.HighPrice - state.Distribution.LowPrice
	// Both are quarter 0, same base price (36 vs 40 = 4 difference), plus surcharge diff.
	actualDiff := q40High.Price - q36Low.Price
	wantDiff := float32(4) + expectedDiff
	if !approxEq(actualDiff, wantDiff, 1e-3) {
		t.Errorf("quarter 40 - quarter 36 with dist: got %v, want %v", actualDiff, wantDiff)
	}
}

// TestFindCurrentQuarterIdx_LocatesTodayQuarter confirms the helper finds the
// row representing the current Prague-local quarter — that's what drives the
// "now" highlight and the scroll-into-view target on the page.
func TestFindCurrentQuarterIdx_LocatesTodayQuarter(t *testing.T) {
	state := openTestState(t)
	loc, _ := time.LoadLocation("Europe/Prague")
	today := time.Date(2026, 5, 10, 0, 0, 0, 0, loc)
	seedPragueDay(t, state, today.AddDate(0, 0, -1))
	seedPragueDay(t, state, today)

	now := time.Date(2026, 5, 10, 14, 0, 0, 0, loc)
	rows := buildNowQuarters(state, now, CurrencyEur, false)
	idx := findCurrentQuarterIdx(rows, now)

	// 96 yesterday rows + 56 (today 14:00 = quarter 56).
	wantIdx := 96 + 56
	if idx != wantIdx {
		t.Fatalf("current quarter index: got %d, want %d", idx, wantIdx)
	}
	if got := rows[idx].Hour; got != 14 || rows[idx].Quarter != 0 {
		t.Errorf("current quarter: got hour=%d quarter=%d, want hour=14 quarter=0",
			rows[idx].Hour, rows[idx].Quarter)
	}
}

// TestRoute_Now_BasicRender exercises the full route once today's data is
// seeded, and checks for the markers the mobile UI depends on: the page-fill
// container, an active-hour row, and a per-row gradient style.
func TestRoute_Now_BasicRender(t *testing.T) {
	state := openTestState(t)
	loc, _ := time.LoadLocation("Europe/Prague")

	// Seed yesterday + today so the route's lookback has data. The route still
	// uses time.Now() internally — we accept that here and just check that the
	// rendered shape is correct rather than that the active row matches a
	// specific hour.
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	seedPragueDay(t, state, today)
	seedPragueDay(t, state, today.AddDate(0, 0, -1))

	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) {
		return fixedPrices(96), true
	})
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/now", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := readBody(t, rr.Result())

	if !strings.Contains(body, `class="grid"`) {
		t.Errorf("missing viewport grid container")
	}
	if !strings.Contains(body, `class="q now"`) {
		t.Errorf("missing active-quarter cell (expected `class=\"q now\"`)")
	}
	if !strings.Contains(body, `class="row has-now"`) {
		t.Errorf("missing hour row containing the now quarter (expected `class=\"row has-now\"`)")
	}
	if !strings.Contains(body, "--bg-l:hsl(") {
		t.Errorf("missing per-quarter HSL gradient style")
	}
	// One row per hour for each available day → 24 rows per day, ≥ 2 days seeded.
	gotRows := strings.Count(body, `class="row`)
	if gotRows < 24*2 || gotRows%24 != 0 {
		t.Errorf("hour-row count: got %d, want a positive multiple of 24 ≥ 48", gotRows)
	}
	// 4 quarter cells per hour row.
	gotCells := strings.Count(body, `class="q`)
	if gotCells != gotRows*4 {
		t.Errorf("quarter-cell count: got %d, want %d (4× rows)", gotCells, gotRows*4)
	}
	// Default unit must be CZK/kWh.
	if !strings.Contains(body, "CZK/kWh") {
		t.Errorf("default unit should be CZK/kWh; body title=%q", titleOf(body))
	}
}

// TestRoute_Now_EurOverrideViaQuery confirms the cur=eur escape hatch flips
// the title unit. Distribution still defaults on; we only assert what we
// changed.
func TestRoute_Now_EurOverrideViaQuery(t *testing.T) {
	state := openTestState(t)
	loc, _ := time.LoadLocation("Europe/Prague")
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	seedPragueDay(t, state, today)

	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) {
		return fixedPrices(96), true
	})
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/now?cur=eur", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := readBody(t, rr.Result())
	if !strings.Contains(body, "EUR/MWh") {
		t.Errorf("cur=eur should show EUR/MWh unit; got title=%q", titleOf(body))
	}
}

func approxEq(a, b, eps float32) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func titleOf(body string) string {
	i := strings.Index(body, "<title>")
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</title>")
	if j < 0 {
		return ""
	}
	return body[i+7 : i+j]
}
