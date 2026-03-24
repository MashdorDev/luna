package luna

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func init() {
	registerMarketProvider("yahoo", &yahooProvider{}, &RateLimitConfig{
		MaxCredits:   30, // Yahoo tolerates ~30 symbols per burst
		RefillRate:   30,
		RefillPeriod: 1 * time.Minute,
	})
}

type yahooProvider struct{}

type yahooResponseJson struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Currency           string  `json:"currency"`
				Symbol             string  `json:"symbol"`
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				ChartPreviousClose float64 `json:"chartPreviousClose"`
				ShortName          string  `json:"shortName"`
				PriceHint          int     `json:"priceHint"`
			} `json:"meta"`
			Indicators struct {
				Quote []struct {
					Close []float64 `json:"close,omitempty"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
	} `json:"chart"`
}

func (p *yahooProvider) FetchMarkets(marketRequests []marketRequest, _ string) (marketList, error) {
	requests := make([]*http.Request, 0, len(marketRequests))

	for i := range marketRequests {
		request, _ := http.NewRequest("GET",
			fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?range=1mo&interval=1d",
				marketRequests[i].Symbol), nil)
		setBrowserUserAgentHeader(request)
		requests = append(requests, request)
	}

	job := newJob(decodeJsonFromRequestTask[yahooResponseJson](defaultHTTPClient), requests)
	responses, errs, err := workerPoolDo(job)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoContent, err)
	}

	markets := make(marketList, 0, len(responses))
	var failed int

	for i := range responses {
		if errs[i] != nil {
			failed++
			slog.Error("Failed to fetch market data", "provider", "yahoo",
				"symbol", marketRequests[i].Symbol, "error", errs[i])
			continue
		}

		response := responses[i]

		if len(response.Chart.Result) == 0 {
			failed++
			slog.Error("Market response contains no data", "provider", "yahoo",
				"symbol", marketRequests[i].Symbol)
			continue
		}

		result := &response.Chart.Result[0]
		prices := result.Indicators.Quote[0].Close

		if len(prices) > marketChartDays {
			prices = prices[len(prices)-marketChartDays:]
		}

		previous := result.Meta.RegularMarketPrice
		if len(prices) >= 2 && prices[len(prices)-2] != 0 {
			previous = prices[len(prices)-2]
		}

		points := svgPolylineCoordsFromYValues(100, 50, maybeCopySliceWithoutZeroValues(prices))
		currency := resolveCurrencySymbol(strings.ToUpper(result.Meta.Currency))

		markets = append(markets, market{
			marketRequest: marketRequests[i],
			Price:         result.Meta.RegularMarketPrice,
			Currency:      currency,
			PriceHint:     result.Meta.PriceHint,
			Name: ternary(marketRequests[i].CustomName == "",
				result.Meta.ShortName,
				marketRequests[i].CustomName,
			),
			PercentChange: percentChange(
				result.Meta.RegularMarketPrice,
				previous,
			),
			SvgChartPoints: points,
		})
	}

	return marketsFailed(len(marketRequests), failed, markets)
}
