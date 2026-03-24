package luna

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

func init() {
	registerMarketProvider("finnhub", &finnhubProvider{}, &RateLimitConfig{
		MaxCredits:   60, // free tier: 60 calls per minute
		RefillRate:   60,
		RefillPeriod: 1 * time.Minute,
	})
}

type finnhubProvider struct{}

type finnhubQuoteResponse struct {
	Current       float64 `json:"c"`
	Change        float64 `json:"d"`
	PercentChange float64 `json:"dp"`
	High          float64 `json:"h"`
	Low           float64 `json:"l"`
	Open          float64 `json:"o"`
	PreviousClose float64 `json:"pc"`
}

func (p *finnhubProvider) FetchMarkets(marketRequests []marketRequest, apiKey string) (marketList, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%w: finnhub provider requires an api-key", errNoContent)
	}

	requests := make([]*http.Request, 0, len(marketRequests))
	for i := range marketRequests {
		url := fmt.Sprintf("https://finnhub.io/api/v1/quote?symbol=%s&token=%s",
			marketRequests[i].Symbol, apiKey)
		request, _ := http.NewRequest("GET", url, nil)
		requests = append(requests, request)
	}

	job := newJob(decodeJsonFromRequestTask[finnhubQuoteResponse](defaultHTTPClient), requests)
	responses, errs, err := workerPoolDo(job)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoContent, err)
	}

	markets := make(marketList, 0, len(responses))
	var failed int

	for i := range responses {
		if errs[i] != nil {
			failed++
			slog.Error("Failed to fetch market data", "provider", "finnhub",
				"symbol", marketRequests[i].Symbol, "error", errs[i])
			continue
		}

		response := responses[i]

		if response.Current == 0 {
			failed++
			slog.Error("Finnhub returned no data", "symbol", marketRequests[i].Symbol)
			continue
		}

		markets = append(markets, market{
			marketRequest: marketRequests[i],
			Price:         response.Current,
			Currency:      "$", // Finnhub free tier is US stocks only
			PriceHint:     2,
			Name: ternary(marketRequests[i].CustomName == "",
				marketRequests[i].Symbol,
				marketRequests[i].CustomName,
			),
			PercentChange:  response.PercentChange,
			SvgChartPoints: "", // No historical data on Finnhub free tier
		})
	}

	return marketsFailed(len(marketRequests), failed, markets)
}
