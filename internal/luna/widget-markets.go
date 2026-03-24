package luna

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"sort"
	"strings"
	"time"
)

var marketsWidgetTemplate = mustParseTemplate("markets.html", "widget-base.html")

type marketsWidget struct {
	widgetBase         `yaml:",inline"`
	StocksRequests     []marketRequest `yaml:"stocks"`
	MarketRequests     []marketRequest `yaml:"markets"`
	ChartLinkTemplate  string          `yaml:"chart-link-template"`
	SymbolLinkTemplate string          `yaml:"symbol-link-template"`
	Sort               string          `yaml:"sort-by"`
	Provider           string          `yaml:"provider"`
	APIKey             string          `yaml:"api-key"`
	Markets            marketList      `yaml:"-"`
}

func (widget *marketsWidget) initialize() error {
	widget.withTitle("Markets").withCacheDuration(time.Hour)

	if widget.Provider == "" {
		widget.Provider = "yahoo"
	}

	// legacy support, remove in v0.10.0
	if len(widget.MarketRequests) == 0 {
		widget.MarketRequests = widget.StocksRequests
	}

	for i := range widget.MarketRequests {
		m := &widget.MarketRequests[i]

		if widget.ChartLinkTemplate != "" && m.ChartLink == "" {
			m.ChartLink = strings.ReplaceAll(widget.ChartLinkTemplate, "{SYMBOL}", m.Symbol)
		}

		if widget.SymbolLinkTemplate != "" && m.SymbolLink == "" {
			m.SymbolLink = strings.ReplaceAll(widget.SymbolLinkTemplate, "{SYMBOL}", m.Symbol)
		}
	}

	return nil
}

func (widget *marketsWidget) update(ctx context.Context) {
	provider, err := getMarketProvider(widget.Provider)
	if err != nil {
		widget.canContinueUpdateAfterHandlingErr(err)
		return
	}

	markets, err := provider.FetchMarkets(widget.MarketRequests, widget.APIKey)
	if !widget.canContinueUpdateAfterHandlingErr(err) {
		return
	}

	if widget.Sort == "absolute-change" {
		markets.sortByAbsChange()
	} else if widget.Sort == "change" {
		markets.sortByChange()
	}

	widget.Markets = markets
}

func (widget *marketsWidget) Render() template.HTML {
	return widget.renderTemplate(widget, marketsWidgetTemplate)
}

// ============================================================
// Shared types
// ============================================================

type marketRequest struct {
	CustomName string `yaml:"name"`
	Symbol     string `yaml:"symbol"`
	ChartLink  string `yaml:"chart-link"`
	SymbolLink string `yaml:"symbol-link"`
}

type market struct {
	marketRequest
	Name           string
	Currency       string
	Price          float64
	PriceHint      int
	PercentChange  float64
	SvgChartPoints string
}

type marketList []market

func (t marketList) sortByAbsChange() {
	sort.Slice(t, func(i, j int) bool {
		return math.Abs(t[i].PercentChange) > math.Abs(t[j].PercentChange)
	})
}

func (t marketList) sortByChange() {
	sort.Slice(t, func(i, j int) bool {
		return t[i].PercentChange > t[j].PercentChange
	})
}

// TODO: allow changing chart time frame
const marketChartDays = 21

var currencyToSymbol = map[string]string{
	"USD": "$",
	"EUR": "€",
	"JPY": "¥",
	"CAD": "C$",
	"AUD": "A$",
	"GBP": "£",
	"CHF": "Fr",
	"NZD": "N$",
	"INR": "₹",
	"BRL": "R$",
	"RUB": "₽",
	"TRY": "₺",
	"ZAR": "R",
	"CNY": "¥",
	"KRW": "₩",
	"HKD": "HK$",
	"SGD": "S$",
	"SEK": "kr",
	"NOK": "kr",
	"DKK": "kr",
	"PLN": "zł",
	"PHP": "₱",
	"ILS": "₪",
	"MXN": "Mex$",
	"COP": "Col$",
	"THB": "฿",
	"TWD": "NT$",
	"MYR": "RM",
}

func resolveCurrencySymbol(code string) string {
	symbol, exists := currencyToSymbol[strings.ToUpper(code)]
	if !exists {
		return code
	}
	return symbol
}

func marketsFailed(total, failed int, markets marketList) (marketList, error) {
	if len(markets) == 0 {
		return nil, errNoContent
	}
	if failed > 0 {
		return markets, fmt.Errorf("%w: could not fetch data for %d market(s)", errPartialContent, failed)
	}
	return markets, nil
}
