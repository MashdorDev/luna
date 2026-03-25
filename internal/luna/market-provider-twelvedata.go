package luna

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func init() {
	registerMarketProvider("twelvedata", &twelveDataProvider{}, &RateLimitConfig{
		MaxCredits:   8, // free tier: 8 credits per minute
		RefillRate:   8,
		RefillPeriod: 1 * time.Minute,
	})
}

type twelveDataProvider struct{}

type twelveDataTimeSeriesResponse struct {
	Meta struct {
		Symbol        string `json:"symbol"`
		Currency      string `json:"currency"`
		CurrencyQuote string `json:"currency_quote"`
	} `json:"meta"`
	Values []struct {
		Close string `json:"close"`
	} `json:"values"`
	Status string `json:"status"`
}

func (p *twelveDataProvider) FetchMarkets(marketRequests []marketRequest, apiKey string) (marketList, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("%w: twelvedata provider requires an api-key", errNoContent)
	}

	symbols := make([]string, len(marketRequests))
	for i, r := range marketRequests {
		symbols[i] = r.Symbol
	}

	url := fmt.Sprintf(
		"https://api.twelvedata.com/time_series?symbol=%s&interval=1day&outputsize=%d&apikey=%s",
		strings.Join(symbols, ","),
		marketChartDays+1,
		apiKey,
	)

	request, _ := http.NewRequest("GET", url, nil)
	resp, err := defaultHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoContent, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: unexpected status code %d from twelvedata", errNoContent, resp.StatusCode)
	}

	markets := make(marketList, 0, len(marketRequests))
	var failed int

	if len(marketRequests) == 1 {
		var single twelveDataTimeSeriesResponse
		if err := json.NewDecoder(resp.Body).Decode(&single); err != nil {
			return nil, fmt.Errorf("%w: %v", errNoContent, err)
		}

		if single.Status == "error" {
			return nil, fmt.Errorf("%w: twelvedata returned error for %s", errNoContent, marketRequests[0].Symbol)
		}

		m, err := parseTwelveDataSymbol(single, marketRequests[0])
		if err != nil {
			return nil, err
		}
		markets = append(markets, m)
	} else {
		// Batch response: map of symbol->data plus top-level code/message/status
		// fields. Decode into raw messages to handle the mixed types.
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: %v", errNoContent, err)
		}

		if codeRaw, exists := raw["code"]; exists {
			var code int
			if json.Unmarshal(codeRaw, &code) == nil && code != 200 {
				msg := ""
				if msgRaw, ok := raw["message"]; ok {
					json.Unmarshal(msgRaw, &msg)
				}
				return nil, fmt.Errorf("%w: twelvedata API error %d: %s", errNoContent, code, msg)
			}
		}

		for _, req := range marketRequests {
			dataRaw, exists := raw[req.Symbol]
			if !exists {
				failed++
				slog.Error("No data returned from Twelve Data", "symbol", req.Symbol)
				continue
			}

			var data twelveDataTimeSeriesResponse
			if err := json.Unmarshal(dataRaw, &data); err != nil {
				failed++
				slog.Error("Failed to decode Twelve Data response", "symbol", req.Symbol, "error", err)
				continue
			}

			if data.Status == "error" {
				failed++
				slog.Error("Twelve Data returned error for symbol", "symbol", req.Symbol)
				continue
			}

			m, err := parseTwelveDataSymbol(data, req)
			if err != nil {
				failed++
				slog.Error("Failed to parse Twelve Data response", "symbol", req.Symbol, "error", err)
				continue
			}
			markets = append(markets, m)
		}
	}

	return marketsFailed(len(marketRequests), failed, markets)
}

func parseTwelveDataSymbol(data twelveDataTimeSeriesResponse, req marketRequest) (market, error) {
	if len(data.Values) < 2 {
		return market{}, fmt.Errorf("insufficient data for %s", req.Symbol)
	}

	// Values are returned newest-first, reverse for chart
	prices := make([]float64, 0, len(data.Values))
	for i := len(data.Values) - 1; i >= 0; i-- {
		var p float64
		fmt.Sscanf(data.Values[i].Close, "%f", &p)
		prices = append(prices, p)
	}

	if len(prices) > marketChartDays {
		prices = prices[len(prices)-marketChartDays:]
	}

	currentPrice := prices[len(prices)-1]
	var previous float64
	if len(prices) >= 2 && prices[len(prices)-2] != 0 {
		previous = prices[len(prices)-2]
	} else {
		previous = currentPrice
	}

	points := svgPolylineCoordsFromYValues(100, 50, maybeCopySliceWithoutZeroValues(prices))

	currencyCode := data.Meta.Currency
	if currencyCode == "" {
		// For crypto pairs (BTC/CAD), currency_quote returns full name
		// ("Canadian Dollar") instead of code. Extract from symbol instead.
		if parts := strings.SplitN(req.Symbol, "/", 2); len(parts) == 2 {
			currencyCode = parts[1]
		} else {
			currencyCode = data.Meta.CurrencyQuote
		}
	}
	currency := resolveCurrencySymbol(currencyCode)

	return market{
		marketRequest: req,
		Price:         currentPrice,
		Currency:      currency,
		PriceHint:     2,
		Name: ternary(req.CustomName == "",
			req.Symbol,
			req.CustomName,
		),
		PercentChange:  percentChange(currentPrice, previous),
		SvgChartPoints: points,
	}, nil
}
