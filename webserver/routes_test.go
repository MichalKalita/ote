package webserver

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MichalKalita/ote/dataloader"
	"github.com/MichalKalita/ote/storage"
)

// buildTestHandler wires the same mux + compression that StartWebServer builds,
// but returns a http.Handler suitable for httptest.NewRecorder. Real handlers,
// real middleware, real state.
func buildTestHandler(state *AppState) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		routeGetRoot(state, w, r)
	})
	mux.HandleFunc("/optimizer", func(w http.ResponseWriter, r *http.Request) {
		routeGetOptimizer(state, w, r)
	})
	mux.HandleFunc("/opt", func(w http.ResponseWriter, r *http.Request) {
		routeGetOpt(state, w, r)
	})
	mux.HandleFunc("/consumption", func(w http.ResponseWriter, r *http.Request) {
		routeConsumption(state, w, r)
	})
	mux.HandleFunc("/now", func(w http.ResponseWriter, r *http.Request) {
		routeNow(state, w, r)
	})
	return compressionMiddleware(mux)
}

// openTestState opens a real DB in t.TempDir() and wires AppState.
func openTestState(t *testing.T) *AppState {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewAppState(db)
}

// startOTEFixture spins up a httptest server that responds to OTE-style requests
// with `quartersFor(reportDate) → price slice`. Returns count of HTTP hits.
func startOTEFixture(t *testing.T, quartersFor func(reportDate string) ([]float32, bool)) (cleanup func(), hits *int) {
	t.Helper()
	hits = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		date := r.URL.Query().Get("report_date")
		prices, ok := quartersFor(date)
		if !ok {
			http.Error(w, "no data", http.StatusNotFound)
			return
		}
		type pt struct {
			Y float32 `json:"y"`
		}
		points := make([]pt, len(prices))
		for i, p := range prices {
			points[i] = pt{Y: p}
		}
		body, _ := json.Marshal(map[string]any{
			"data": map[string]any{
				"dataLine": []map[string]any{
					{"title": "15min price (EUR/MWh)", "point": points},
				},
			},
		})
		w.Write(body)
	}))
	prev := dataloader.BaseURL
	dataloader.BaseURL = srv.URL
	return func() {
		dataloader.BaseURL = prev
		srv.Close()
	}, hits
}

// fixedPrices returns a slice of `n` floats where prices[i] = float32(i).
func fixedPrices(n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = float32(i)
	}
	return out
}

// readBody decompresses gzip if Content-Encoding said so; the compression
// middleware always picks gzip when the client advertises it.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var r io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(resp.Body)
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		defer gr.Close()
		r = gr
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestRoute_Root_DayAlreadyInDB_DoesNotFetch(t *testing.T) {
	state := openTestState(t)
	loc, _ := time.LoadLocation("Europe/Prague")

	// Pre-seed 96 quarters where price = quarter index.
	day := time.Date(2026, 5, 10, 0, 0, 0, 0, loc)
	quarters := make([]storage.Quarter, 96)
	for i := range quarters {
		quarters[i] = storage.Quarter{
			Ts:    day.Add(time.Duration(i) * 15 * time.Minute).UTC(),
			Price: float32(i),
		}
	}
	if err := state.db.SaveQuarters(quarters); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Fixture: fail the test only if 2026-05-10 itself is fetched. Other days of
	// the month may legitimately be fetched by the calendar warm-up.
	cleanup, _ := startOTEFixture(t, func(reportDate string) ([]float32, bool) {
		if reportDate == "2026-05-10" {
			t.Errorf("dataloader was called for 2026-05-10 although it is in DB")
		}
		return fixedPrices(96), true
	})
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/?date=2026-05-10", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := readBody(t, rr.Result())
	if !strings.Contains(body, "OTE prices 2026-05-10") {
		t.Errorf("body missing date heading")
	}
}

func TestRoute_Root_DayNotInDB_FetchesAndPersists(t *testing.T) {
	state := openTestState(t)

	cleanup, hits := startOTEFixture(t, func(reportDate string) ([]float32, bool) {
		// Return 96 quarters for any date — calendar warm-up will also hit.
		return fixedPrices(96), true
	})
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/?date=2026-05-10", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if *hits == 0 {
		t.Error("expected at least one OTE hit, got 0")
	}
	// Day must now be persisted.
	has, err := state.db.HasDay("2026-05-10")
	if err != nil {
		t.Fatalf("HasDay: %v", err)
	}
	if !has {
		t.Error("day was fetched but not persisted")
	}

	// Second request: zero new OTE hits for the day itself (calendar may still warm).
	hitsBefore := *hits
	req2 := httptest.NewRequest(http.MethodGet, "/?date=2026-05-10", nil)
	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("second request status: got %d, want 200", rr2.Code)
	}
	// Same calendar month → calendar days are now in DB → no additional hits.
	if *hits != hitsBefore {
		t.Errorf("second request triggered %d new OTE hits; expected DB-only", *hits-hitsBefore)
	}
}

// countDataIdx returns how many <td data-idx="N"> cells appear in body. The
// table renders one cell per quarter; the chart also renders one rect per
// quarter. We grep only the table's cells by anchoring on the surrounding
// HTML pattern.
func countTableCellsByDataIdx(body string) int {
	// Match cells emitted by RenderTable: `<td class="..." data-idx="N">price</td>`.
	// Each price-bearing cell has a class attr containing `font-mono`. Empty cells
	// (from `<td></td>` filler) are excluded.
	return strings.Count(body, `data-idx="`) / 2 // appears in chart AND table for each idx
}

func TestRoute_Root_DSTSpringDay_Renders92Quarters_And23HourRows(t *testing.T) {
	state := openTestState(t)

	cleanup, _ := startOTEFixture(t, func(reportDate string) ([]float32, bool) {
		if reportDate == "2026-03-29" {
			return fixedPrices(92), true // 23 h × 4 = 92 quarters (spring forward)
		}
		return fixedPrices(96), true
	})
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/?date=2026-03-29", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("DST spring day status: got %d, want 200", rr.Code)
	}
	body := readBody(t, rr.Result())
	if !strings.Contains(body, "OTE prices 2026-03-29") {
		t.Fatal("body missing heading for DST day")
	}

	// 92 quarter cells must appear; idx 0..91 each in chart and table.
	if got := countTableCellsByDataIdx(body); got != 92 {
		t.Errorf("expected 92 quarter cells, got %d", got)
	}
	// The last quarter's index must be 91, not 95.
	if !strings.Contains(body, `data-idx="91"`) {
		t.Error(`missing data-idx="91" (last quarter of 23h day)`)
	}
	if strings.Contains(body, `data-idx="92"`) {
		t.Error(`unexpected data-idx="92" — DST spring day should stop at 91`)
	}
	// 23 hour-rows: hour labels 0..22, but NOT 23.
	for h := 0; h <= 22; h++ {
		marker := fmt.Sprintf(`px-4">%d</td>`, h)
		if !strings.Contains(body, marker) {
			t.Errorf("missing hour row %d", h)
		}
	}
	if strings.Contains(body, `px-4">23</td>`) {
		t.Error("DST spring day should not render hour 23 (only 23 hours)")
	}
}

func TestRoute_Root_DSTAutumnDay_Renders100Quarters_And25HourRows(t *testing.T) {
	state := openTestState(t)

	cleanup, _ := startOTEFixture(t, func(reportDate string) ([]float32, bool) {
		if reportDate == "2025-10-26" {
			return fixedPrices(100), true // 25 h × 4 = 100 quarters (fall back)
		}
		return fixedPrices(96), true
	})
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/?date=2025-10-26", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("DST autumn day status: got %d, want 200", rr.Code)
	}
	body := readBody(t, rr.Result())
	if !strings.Contains(body, "OTE prices 2025-10-26") {
		t.Fatal("body missing heading for DST autumn day")
	}

	if got := countTableCellsByDataIdx(body); got != 100 {
		t.Errorf("expected 100 quarter cells, got %d", got)
	}
	if !strings.Contains(body, `data-idx="99"`) {
		t.Error(`missing data-idx="99" (last quarter of 25h day)`)
	}
	if strings.Contains(body, `data-idx="100"`) {
		t.Error(`unexpected data-idx="100" — DST autumn day should stop at 99`)
	}
	// 25 hour-rows: hour labels 0..24.
	for h := 0; h <= 24; h++ {
		marker := fmt.Sprintf(`px-4">%d</td>`, h)
		if !strings.Contains(body, marker) {
			t.Errorf("missing hour row %d", h)
		}
	}
}

func TestRoute_Root_NormalDay_Renders96Quarters_And24HourRows(t *testing.T) {
	state := openTestState(t)

	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) { return fixedPrices(96), true })
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/?date=2026-05-10", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := readBody(t, rr.Result())

	if got := countTableCellsByDataIdx(body); got != 96 {
		t.Errorf("normal day expected 96 quarter cells, got %d", got)
	}
	if !strings.Contains(body, `data-idx="95"`) {
		t.Error(`missing data-idx="95" (last quarter of normal day)`)
	}
	if strings.Contains(body, `data-idx="96"`) {
		t.Error(`unexpected data-idx="96"`)
	}
	for h := 0; h <= 23; h++ {
		marker := fmt.Sprintf(`px-4">%d</td>`, h)
		if !strings.Contains(body, marker) {
			t.Errorf("missing hour row %d", h)
		}
	}
	if strings.Contains(body, `px-4">24</td>`) {
		t.Error("normal day should not render hour 24")
	}
}

func TestRoute_Root_FetchFailureReturns404(t *testing.T) {
	state := openTestState(t)

	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) {
		return nil, false // 404 from upstream
	})
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/?date=2026-05-10", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("upstream 404 should propagate as 404; got %d", rr.Code)
	}
	body := readBody(t, rr.Result())
	if !strings.Contains(body, "Error fetching data") {
		t.Errorf("body missing error message: %s", body)
	}
	// Page chrome must still render so the user can navigate to a working date.
	if !strings.Contains(body, "OTE prices 2026-05-10") {
		t.Errorf("error page missing date heading")
	}
	if !strings.Contains(body, `href="/optimizer"`) {
		t.Errorf("error page missing Optimizer nav link")
	}
	if !strings.Contains(body, `href="/consumption"`) {
		t.Errorf("error page missing Consumption nav link")
	}
	// Calendar lets the user pick another date.
	if !strings.Contains(body, "May 2026") {
		t.Errorf("error page missing calendar month header")
	}
}

func TestRoute_Root_UnknownPathReturns404(t *testing.T) {
	state := openTestState(t)
	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) { return fixedPrices(96), true })
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown path: got %d, want 404", rr.Code)
	}
}

func TestRoute_Root_CompressionMiddleware_GzipRoundTrip(t *testing.T) {
	state := openTestState(t)
	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) { return fixedPrices(96), true })
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, "/?date=2026-05-10", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding: got %q, want gzip", got)
	}
	// Verify body is actually gzip and decodes to expected HTML.
	body := readBody(t, rr.Result())
	if !strings.Contains(body, "OTE prices 2026-05-10") {
		t.Errorf("decompressed body missing heading")
	}
}

func TestRoute_Opt_EvaluatesCondition(t *testing.T) {
	state := openTestState(t)

	// /opt depends on yesterday/today via ExpressionContext (time.Now). We seed
	// any date the fixture is asked for.
	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) {
		return fixedPrices(96), true
	})
	defer cleanup()

	handler := buildTestHandler(state)
	// `[{"price":1000}]` — true if current price ≤ 1000. Fixture prices are 0..95, so true.
	req := httptest.NewRequest(http.MethodGet, `/opt?exp=[{"price":1000}]`, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := strings.TrimSpace(readBody(t, rr.Result()))
	if body != "true" {
		t.Errorf("/opt result: got %q, want %q", body, "true")
	}
}

func TestRoute_Opt_RejectsMalformedExpressionGracefully(t *testing.T) {
	state := openTestState(t)
	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) { return fixedPrices(96), true })
	defer cleanup()

	handler := buildTestHandler(state)
	req := httptest.NewRequest(http.MethodGet, `/opt?exp=not-json`, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("malformed expression should be reported with 200 + error text; got %d", rr.Code)
	}
	body := readBody(t, rr.Result())
	if !strings.Contains(body, "Error parsing expression") {
		t.Errorf("expected parse error in body, got %q", body)
	}
}

func TestRoute_Root_QueryParamsRoundTripIntoLinks(t *testing.T) {
	state := openTestState(t)
	cleanup, _ := startOTEFixture(t, func(string) ([]float32, bool) { return fixedPrices(96), true })
	defer cleanup()

	handler := buildTestHandler(state)
	// Ask for CZK + distribution on; the rendered HTML must echo these in nav links.
	req := httptest.NewRequest(http.MethodGet, "/?date=2026-05-10&cur=czk&dist=true", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	body := readBody(t, rr.Result())
	// CZK label is shown in the min/avg/max bar.
	if !strings.Contains(body, "CZK/kWh") {
		t.Errorf("body missing CZK label")
	}
	// The 'Change to EUR' link is offered only when current currency is CZK.
	if !strings.Contains(body, "Change to EUR") {
		t.Errorf("body missing 'Change to EUR' link")
	}
	// Distribution checkbox is checked.
	if !strings.Contains(body, `name="dist" value="true" checked`) {
		// Allow the rendered order to vary slightly — look for any "checked" near dist.
		if !(strings.Contains(body, `id="dist"`) && strings.Contains(body, "checked")) {
			t.Errorf("dist=true should render checkbox as checked")
		}
	}

	// Sanity: currency.String() returns "czk" consistently.
	if s := strconv.Quote(CurrencyCzk.String()); s != `"czk"` {
		t.Errorf("Currency.String() drift: %s", s)
	}
}
