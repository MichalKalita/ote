package webserver

import (
	"compress/gzip"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/MichalKalita/ote/storage"
	"github.com/andybalholm/brotli"
)

// StartWebServer starts the HTTP server on $PORT (default 3000).
func StartWebServer(db *storage.DB) {
	state := NewAppState(db)

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

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := "0.0.0.0:" + port

	srv := &http.Server{
		Addr:    addr,
		Handler: compressionMiddleware(mux),
	}
	fmt.Printf("Web server started on %s\n", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// compressionMiddleware applies br/gzip compression based on Accept-Encoding.
func compressionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ae := r.Header.Get("Accept-Encoding")
		switch {
		case strings.Contains(ae, "br"):
			w.Header().Set("Content-Encoding", "br")
			w.Header().Add("Vary", "Accept-Encoding")
			bw := brotli.NewWriter(w)
			defer bw.Close()
			next.ServeHTTP(&compressionWriter{ResponseWriter: w, w: bw}, r)
		case strings.Contains(ae, "gzip"):
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			gw := gzip.NewWriter(w)
			defer gw.Close()
			next.ServeHTTP(&compressionWriter{ResponseWriter: w, w: gw}, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

type compressionWriter struct {
	http.ResponseWriter
	w io.Writer
}

func (c *compressionWriter) Write(b []byte) (int, error) {
	return c.w.Write(b)
}

func routeGetRoot(state *AppState, w http.ResponseWriter, r *http.Request) {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	q := r.URL.Query()
	inputDate := today
	if d := q.Get("date"); d != "" {
		if parsed, err := time.ParseInLocation("2006-01-02", d, loc); err == nil {
			inputDate = parsed
		}
	}
	currency := CurrencyEur
	if cur := q.Get("cur"); cur != "" {
		if c, err := ParseCurrency(cur); err == nil {
			currency = c
		}
	}
	includeDist := q.Get("dist") == "true"

	chart := DefaultChartSettings()

	prices, ok := state.GetPrices(inputDate)

	var sb strings.Builder
	fmt.Fprintf(&sb, `<h1 class="text-4xl font-bold">OTE prices %s</h1>`,
		html.EscapeString(inputDate.Format("2006-01-02")))
	sb.WriteString(`<p class="text-sm mb-8">` + Link("https://github.com/MichalKalita/ote", "github.com/MichalKalita/ote") + `</p>`)
	sb.WriteString(Link("/optimizer", "Optimizer"))
	sb.WriteString(" | ")
	sb.WriteString(Link("/consumption", "Consumption analysis"))
	sb.WriteString(" | ")
	sb.WriteString(Link("/now", "Live (mobile)"))
	sb.WriteString(`<div class="flex flex-row justify-center gap-2">`)
	curStr := currency.String()
	distStr := strconv.FormatBool(includeDist)
	datePrefix := ""
	if !inputDate.Equal(today) {
		datePrefix = fmt.Sprintf("date=%s&", inputDate.Format("2006-01-02"))
	}
	if currency == CurrencyEur {
		sb.WriteString(Link(fmt.Sprintf("/?%scur=czk&dist=%s", datePrefix, distStr), "Change to CZK"))
	} else {
		sb.WriteString(Link(fmt.Sprintf("/?%scur=eur&dist=%s", datePrefix, distStr), "Change to EUR"))
	}
	sb.WriteString(" | ")
	sb.WriteString(`<form method="GET" class="inline-flex items-center gap-1">`)
	if !inputDate.Equal(today) {
		fmt.Fprintf(&sb, `<input type="hidden" name="date" value="%s">`, inputDate.Format("2006-01-02"))
	}
	fmt.Fprintf(&sb, `<input type="hidden" name="cur" value="%s">`, curStr)
	checked := ""
	if includeDist {
		checked = " checked"
	}
	fmt.Fprintf(&sb, `<input type="checkbox" id="dist" name="dist" value="true"%s onchange="this.form.submit()">`, checked)
	sb.WriteString(`<label for="dist">Include distribution</label>`)
	sb.WriteString(`</form>`)
	sb.WriteString(`</div>`)

	maxDate := today
	if now.Hour() >= NextDayPricesHour {
		maxDate = today.AddDate(0, 0, 1)
	}
	monthAvgs := state.MonthAverages(inputDate.Year(), inputDate.Month(), loc, includeDist, maxDate)
	sb.WriteString(`<div class="flex justify-center">`)
	sb.WriteString(RenderCalendar(inputDate.Year(), inputDate.Month(), loc, inputDate, today, maxDate, monthAvgs, currency, includeDist))
	sb.WriteString(`</div>`)

	status := http.StatusOK
	if !ok {
		status = http.StatusNotFound
		sb.WriteString(`<p class="my-8 text-red-600 dark:text-red-400">Error fetching data for this date. Prices may not be published yet — try another date.</p>`)
	} else {
		totalPrices := prices.TotalPrices(&state.Distribution)
		var displayPrices []float32
		if includeDist {
			displayPrices = totalPrices
		} else {
			displayPrices = prices.Prices
		}
		cheapestIdx, minPrice := CheapestHour(displayPrices)
		expensiveIdx, maxPrice := ExpensiveHour(displayPrices)

		var sum float32
		for _, p := range displayPrices {
			sum += p
		}
		avgPrice := sum / float32(len(displayPrices))

		distLabels := state.Distribution.ByHours()
		labels := distLabels[:]

		fmt.Fprintf(&sb, `<div class="mb-4">Min: <span class="font-bold text-green-700 dark:text-green-400">%.2f</span> | Avg: <span class="font-bold">%.2f</span> | Max: <span class="font-bold text-red-700 dark:text-red-400">%.2f</span> %s</div>`,
			currency.Convert(minPrice),
			currency.Convert(avgPrice),
			currency.Convert(maxPrice),
			html.EscapeString(currency.ShortLabel()))

		fmt.Fprintf(&sb, `<div data-page-date="%s">`, inputDate.Format("2006-01-02"))
		sb.WriteString(`<h2 class="text-2xl font-semibold mb-4">Graph</h2>`)
		sb.WriteString(`<div class="mb-4 flex justify-center">`)
		sb.WriteString(chart.Render(displayPrices, labels, func(index int, price float32) string {
			if index == cheapestIdx || price < 0.0 {
				return "fill-green-600"
			}
			if index == expensiveIdx {
				return "fill-red-600"
			}
			return "fill-gray-500"
		}, currency))
		sb.WriteString(`</div>`)

		sb.WriteString(`<h2 class="text-2xl font-semibold mb-4">Table</h2>`)
		sb.WriteString(`<div class="mb-4 flex justify-center">`)
		sb.WriteString(prices.RenderTable(&state.Distribution, currency, includeDist))
		sb.WriteString(`</div>`)
		sb.WriteString(`</div>`)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, RenderLayout(sb.String()))
}

func parseOptQuery(q map[string][]string) (exp string, hours, from, to *uint8) {
	if v, ok := q["exp"]; ok && len(v) > 0 {
		exp = v[0]
	}
	if v, ok := q["hours"]; ok && len(v) > 0 {
		if n, err := strconv.ParseUint(v[0], 10, 8); err == nil {
			x := uint8(n)
			hours = &x
		}
	}
	if v, ok := q["from"]; ok && len(v) > 0 {
		if n, err := strconv.ParseUint(v[0], 10, 8); err == nil {
			x := uint8(n)
			from = &x
		}
	}
	if v, ok := q["to"]; ok && len(v) > 0 {
		if n, err := strconv.ParseUint(v[0], 10, 8); err == nil {
			x := uint8(n)
			to = &x
		}
	}
	return
}

func routeGetOptimizer(state *AppState, w http.ResponseWriter, r *http.Request) {
	exp, hours, from, to := parseOptQuery(r.URL.Query())

	var cheapCondition *CheapCondition
	if hours != nil && from != nil && to != nil {
		cheapCondition = &CheapCondition{Hours: *hours, From: *from, To: *to}
	}

	var condition Condition
	if exp != "" {
		parsed, err := ParseCondition(exp)
		if err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "Error parsing expression: %v", err)
			return
		}
		condition = parsed
	} else if cheapCondition != nil {
		condition = Condition{Kind: CondCheap, Cheap: *cheapCondition}
	} else {
		condition = Condition{Kind: CondAnd}
	}

	expCtx := state.ExpressionContext()
	if expCtx == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "Error creating expression context")
		return
	}

	const domain = "https://ota.kalita.cz"
	automationURL := fmt.Sprintf("%s/opt?exp=%s", domain, exp)
	examples := []string{`/optimizer?exp=[{"price":120},{"hours":[0,10]}]`}

	var sb strings.Builder
	sb.WriteString(`<h1 class="text-4xl font-bold">Optimizer, find cheapist hours</h1>`)
	sb.WriteString(`<p class="text-sm mb-8">` + Link("https://github.com/MichalKalita/ote", "github.com/MichalKalita/ote") + `</p>`)
	sb.WriteString(Link("/", "Homepage"))
	sb.WriteString(`<div class="text-left">`)
	sb.WriteString(`<h2 class="text-2xl font-semibold mb-4">Condition</h2>`)
	sb.WriteString(RenderCheapForm(cheapCondition))
	sb.WriteString(condition.RenderHTML())
	sb.WriteString(`<h2 class="text-2xl font-semibold mb-4">Evaluation</h2>`)
	sb.WriteString(`<pre>`)
	sb.WriteString(html.EscapeString(fmt.Sprintf("%v", condition.Evaluate(expCtx))))
	sb.WriteString(`</pre>`)
	fmt.Fprintf(&sb, `<a href="%s">URL for automation tools %s</a>`,
		html.EscapeString(automationURL), html.EscapeString(automationURL))
	sb.WriteString(`<h2 class="text-2xl font-semibold mb-4">Evaluate in Chart</h2>`)
	sb.WriteString(`<div class="mb-4 flex justify-center">`)
	sb.WriteString(condition.EvaluateAllInChart(expCtx))
	sb.WriteString(`</div>`)
	sb.WriteString(`<h2 class="text-2xl font-semibold mb-4">Examples</h2>`)
	sb.WriteString(`<ul>`)
	for _, ex := range examples {
		sb.WriteString(`<li>`)
		sb.WriteString(Link(ex, ex))
		sb.WriteString(`</li>`)
	}
	sb.WriteString(`</ul>`)
	sb.WriteString(`</div>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, RenderLayout(sb.String()))
}

func routeGetOpt(state *AppState, w http.ResponseWriter, r *http.Request) {
	exp, _, _, _ := parseOptQuery(r.URL.Query())

	var condition Condition
	if exp != "" {
		parsed, err := ParseCondition(exp)
		if err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "Error parsing expression: %v", err)
			return
		}
		condition = parsed
	} else {
		condition = Condition{Kind: CondAnd}
	}

	expCtx := state.ExpressionContext()
	if expCtx == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "Error creating expression context")
		return
	}

	result := condition.Evaluate(expCtx)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%v", result)
}

func routeConsumption(state *AppState, w http.ResponseWriter, r *http.Request) {
	currency := CurrencyEur
	if cur := r.URL.Query().Get("cur"); cur != "" {
		if c, err := ParseCurrency(cur); err == nil {
			currency = c
		}
	}
	curStr := currency.String()

	var sb strings.Builder
	sb.WriteString(`<h1 class="text-4xl font-bold">Consumption analysis</h1>`)
	sb.WriteString(`<p class="text-sm mb-8">` + Link("https://github.com/MichalKalita/ote", "github.com/MichalKalita/ote") + `</p>`)
	sb.WriteString(Link("/", "Homepage"))

	sb.WriteString(`<div class="my-4">`)
	if currency == CurrencyEur {
		sb.WriteString(Link("/consumption?cur=czk", "Change to CZK"))
	} else {
		sb.WriteString(Link("/consumption?cur=eur", "Change to EUR"))
	}
	sb.WriteString(`</div>`)

	if r.Method == http.MethodPost {
		const maxUpload = 10 << 20 // 10 MiB
		r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		if err := r.ParseMultipartForm(maxUpload); err != nil {
			renderConsumptionPage(w, sb.String()+
				`<p class="text-red-600 my-4">Upload too large or invalid form.</p>`+
				renderConsumptionForm(curStr), http.StatusBadRequest)
			return
		}
		file, _, err := r.FormFile("csv")
		if err != nil {
			renderConsumptionPage(w, sb.String()+
				`<p class="text-red-600 my-4">Missing CSV file.</p>`+
				renderConsumptionForm(curStr), http.StatusBadRequest)
			return
		}
		defer file.Close()

		quarters, err := ParseConsumptionCSV(file)
		if err != nil {
			body := sb.String() +
				fmt.Sprintf(`<p class="text-red-600 my-4">%s</p>`, html.EscapeString(err.Error())) +
				renderConsumptionForm(curStr)
			renderConsumptionPage(w, body, http.StatusBadRequest)
			return
		}
		if len(quarters) == 0 {
			renderConsumptionPage(w, sb.String()+
				`<p class="text-red-600 my-4">No data rows found in CSV.</p>`+
				renderConsumptionForm(curStr), http.StatusBadRequest)
			return
		}

		analysis, err := state.AnalyzeConsumption(quarters, time.Now())
		if err != nil {
			body := sb.String() +
				fmt.Sprintf(`<p class="text-red-600 my-4">%s</p>`, html.EscapeString(err.Error())) +
				renderConsumptionForm(curStr)
			renderConsumptionPage(w, body, http.StatusBadRequest)
			return
		}
		sb.WriteString(renderConsumptionResults(analysis, currency))
		sb.WriteString(`<div class="my-8 text-sm">Upload another file:</div>`)
		sb.WriteString(renderConsumptionForm(curStr))
		renderConsumptionPage(w, sb.String(), http.StatusOK)
		return
	}

	sb.WriteString(`<p class="my-4">Upload a CSV exported from ČEZ "Profilová náměřená data" (PND export). The file is processed in memory and is not stored on the server.</p>`)
	sb.WriteString(renderConsumptionForm(curStr))
	renderConsumptionPage(w, sb.String(), http.StatusOK)
}

func renderConsumptionPage(w http.ResponseWriter, body string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, RenderLayout(body))
}
