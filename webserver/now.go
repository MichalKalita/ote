package webserver

import (
	"fmt"
	"html"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// /now is a mobile-first page showing every quarter-hour price for the
// loaded days (yesterday + today + tomorrow when published) as a vertically
// scrollable list. The current quarter is highlighted and auto-scrolled into
// view. Auto-reloads on tab visibility change and on a periodic timer.

var czWeekdays = [7]string{"ne", "po", "út", "st", "čt", "pá", "so"}

type nowQuarterRow struct {
	Day      time.Time // Prague-local day
	Hour     int       // 0..23
	Quarter  int       // 0..3 (15-min offset within the hour)
	Price    float32   // already converted to display currency
	IsNow    bool      // current Prague-local quarter
	Progress float32   // 0..1, fraction of the current quarter elapsed
}

func buildNowQuarters(state *AppState, now time.Time, currency Currency, includeDist bool) []nowQuarterRow {
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	yesterday := today.AddDate(0, 0, -1)
	tomorrow := today.AddDate(0, 0, 1)

	dist := &state.Distribution
	var rows []nowQuarterRow

	addDay := func(prices []float32, day time.Time) {
		for idx, price := range prices {
			h := idx / 4
			q := idx % 4
			p := price
			if includeDist {
				if containsByte(dist.HighHours, byte(h)) {
					p += dist.HighPrice
				} else {
					p += dist.LowPrice
				}
			}
			rows = append(rows, nowQuarterRow{
				Day:     day,
				Hour:    h,
				Quarter: q,
				Price:   currency.Convert(p),
			})
		}
	}

	if p, ok := state.GetPrices(yesterday); ok {
		addDay(p.Prices, yesterday)
	}
	if p, ok := state.GetPrices(today); ok {
		addDay(p.Prices, today)
	}
	if now.Hour() >= NextDayPricesHour {
		if p, ok := state.GetPrices(tomorrow); ok {
			addDay(p.Prices, tomorrow)
		}
	}

	return rows
}

// findCurrentQuarterIdx returns the index in rows that matches the current
// Prague-local quarter, or -1 if today's data isn't loaded.
func findCurrentQuarterIdx(rows []nowQuarterRow, now time.Time) int {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	currentQuarter := now.Hour()*4 + now.Minute()/15
	for i, r := range rows {
		if r.Day.Equal(today) && r.Hour*4+r.Quarter == currentQuarter {
			return i
		}
	}
	return -1
}

func routeNow(state *AppState, w http.ResponseWriter, r *http.Request) {
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)

	q := r.URL.Query()
	currency := CurrencyCzk
	if cur := q.Get("cur"); cur != "" {
		if c, err := ParseCurrency(cur); err == nil {
			currency = c
		}
	}
	includeDist := true
	if q.Get("dist") == "false" {
		includeDist = false
	}

	rows := buildNowQuarters(state, now, currency, includeDist)
	currentIdx := findCurrentQuarterIdx(rows, now)

	if currentIdx >= 0 {
		rows[currentIdx].IsNow = true
		rows[currentIdx].Progress = float32(now.Second()+now.Nanosecond()/1e9) / 900.0 // 900 sec/quarter
	}

	var minP, maxP float32 = float32(math.Inf(1)), float32(math.Inf(-1))
	for _, r := range rows {
		if r.Price < minP {
			minP = r.Price
		}
		if r.Price > maxP {
			maxP = r.Price
		}
	}
	span := maxP - minP
	if span <= 0 {
		span = 1
	}

	var body strings.Builder
	body.WriteString(`<div class="grid">`)
	if len(rows) == 0 {
		body.WriteString(`<div class="empty">Žádná data — zkus později.</div>`)
	}

	// Group quarters into hour rows. Each input day yields 24 hour rows of 4
	// cells. We rely on rows being in chronological order, 4 quarters per hour.
	for i := 0; i < len(rows); i += 4 {
		hourQs := rows[i : i+4]
		head := hourQs[0]
		wd := czWeekdays[int(head.Day.Weekday())]
		label := fmt.Sprintf("%s %02d", wd, head.Hour)

		// Mark the whole row when any cell is "now" so we can scroll it into view.
		rowClass := "row"
		for _, qr := range hourQs {
			if qr.IsNow {
				rowClass = "row has-now"
				break
			}
		}

		fmt.Fprintf(&body, `<div class="%s">`, rowClass)
		fmt.Fprintf(&body, `<span class="t">%s</span>`, html.EscapeString(label))
		for _, qr := range hourQs {
			norm := (qr.Price - minP) / span
			hue := (1 - norm) * 120
			bgL := fmt.Sprintf("hsl(%.0f,70%%,82%%)", hue)
			bgD := fmt.Sprintf("hsl(%.0f,45%%,24%%)", hue)

			cellClass := "q"
			style := fmt.Sprintf("--bg-l:%s;--bg-d:%s", bgL, bgD)
			if qr.IsNow {
				cellClass = "q now"
				style = fmt.Sprintf("%s;--prog:%.2f%%", style, qr.Progress*100)
			}
			fmt.Fprintf(&body, `<span class="%s" style="%s">%s</span>`,
				cellClass, style,
				html.EscapeString(formatNowPrice(qr.Price, currency)))
		}
		body.WriteString(`</div>`)
	}
	body.WriteString(`</div>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, renderNowPage(body.String(), currency))
}

func formatNowPrice(p float32, currency Currency) string {
	switch currency {
	case CurrencyCzk:
		return fmt.Sprintf("%.2f", p)
	default:
		return fmt.Sprintf("%.0f", p)
	}
}

func renderNowPage(content string, currency Currency) string {
	unit := html.EscapeString(currency.ShortLabel())
	return `<!DOCTYPE html>
<html lang="cs">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<title>OTE — ceny (` + unit + `)</title>
<style>
*,::before,::after{box-sizing:border-box;margin:0;padding:0}
html,body{background:#fff;color:#111;font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
.grid{display:flex;flex-direction:column}
.row{display:grid;grid-template-columns:56px repeat(4,1fr);align-items:stretch;border-bottom:1px solid rgba(0,0,0,.07);scroll-margin-top:40vh;scroll-margin-bottom:40vh}
.row .t{display:flex;align-items:center;justify-content:flex-start;padding:0 8px;font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;font-size:13px;opacity:.8}
.row .q{display:flex;align-items:center;justify-content:center;height:38px;background:var(--bg-l);border-left:1px solid rgba(0,0,0,.07);font-family:ui-monospace,SFMono-Regular,Menlo,Monaco,Consolas,monospace;font-size:14px;font-weight:600;position:relative;overflow:hidden}
.row .q.now{outline:2px solid #2563eb;outline-offset:-2px;font-weight:800;z-index:1}
.row .q.now::after{content:"";position:absolute;left:0;top:0;bottom:0;width:var(--prog,0%);background:rgba(37,99,235,.22);pointer-events:none}
.empty{padding:40px 0;text-align:center;color:#888;font-size:0.9em}
@media (prefers-color-scheme:dark){
  html,body{background:#0b0f17;color:#f3f4f6}
  .row{border-bottom-color:rgba(255,255,255,.06)}
  .row .q{background:var(--bg-d);border-left-color:rgba(255,255,255,.06)}
  .empty{color:#aaa}
}
</style>
<script>
(function(){
  document.addEventListener('visibilitychange',function(){
    if(!document.hidden) location.reload();
  });
  setTimeout(function(){ location.reload(); }, 5*60*1000);
  var nowEl=document.querySelector('.row.has-now');
  if(nowEl) nowEl.scrollIntoView({block:'center'});
})();
</script>
</head>
<body>
` + content + `
</body>
</html>`
}

