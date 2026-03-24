package luna

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

func init() {
	registerMarketProvider("tmx", &tmxProvider{}, nil) // no rate limit needed — free public API
}

type tmxProvider struct{}

// TMX Money GraphQL responses

type tmxQuoteResponse struct {
	Data struct {
		GetQuoteBySymbol struct {
			Symbol        string  `json:"symbol"`
			Name          string  `json:"name"`
			Price         float64 `json:"price"`
			PriceChange   float64 `json:"priceChange"`
			PercentChange float64 `json:"percentChange"`
			PrevClose     float64 `json:"prevClose"`
		} `json:"getQuoteBySymbol"`
	} `json:"data"`
}

type tmxTimeSeriesResponse struct {
	Data struct {
		GetTimeSeriesData []struct {
			DateTime string  `json:"dateTime"`
			Close    float64 `json:"close"`
		} `json:"getTimeSeriesData"`
	} `json:"data"`
}

const tmxGraphQLEndpoint = "https://app-money.tmx.com/graphql"

func (p *tmxProvider) FetchMarkets(marketRequests []marketRequest, _ string) (marketList, error) {
	markets := make(marketList, 0, len(marketRequests))
	var failed int

	for _, req := range marketRequests {
		// Fetch quote
		quote, err := tmxFetchQuote(req.Symbol)
		if err != nil {
			failed++
			slog.Error("Failed to fetch TMX quote", "symbol", req.Symbol, "error", err)
			continue
		}

		// Fetch historical data for sparkline (interval=365 gives daily resolution)
		chartData, err := tmxFetchTimeSeries(req.Symbol, 365)
		var points string
		if err == nil && len(chartData) > 1 {
			// Data comes newest-first, reverse for chart
			prices := make([]float64, 0, len(chartData))
			for i := len(chartData) - 1; i >= 0; i-- {
				prices = append(prices, chartData[i])
			}
			if len(prices) > marketChartDays {
				prices = prices[len(prices)-marketChartDays:]
			}
			points = svgPolylineCoordsFromYValues(100, 50, maybeCopySliceWithoutZeroValues(prices))
		}

		markets = append(markets, market{
			marketRequest: req,
			Price:         quote.Price,
			Currency:      "C$",
			PriceHint:     2,
			Name: ternary(req.CustomName == "",
				quote.Name,
				req.CustomName,
			),
			PercentChange:  quote.PercentChange,
			SvgChartPoints: points,
		})
	}

	return marketsFailed(len(marketRequests), failed, markets)
}

type tmxQuoteData struct {
	Name          string
	Price         float64
	PercentChange float64
}

func tmxFetchQuote(symbol string) (tmxQuoteData, error) {
	query := `query getQuoteBySymbol($symbol: String!, $locale: String!) {
		getQuoteBySymbol(symbol: $symbol, locale: $locale) {
			symbol name price priceChange percentChange prevClose
		}
	}`

	body, _ := json.Marshal(map[string]interface{}{
		"operationName": "getQuoteBySymbol",
		"variables":     map[string]string{"symbol": symbol, "locale": "en"},
		"query":         query,
	})

	req, _ := http.NewRequest("POST", tmxGraphQLEndpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return tmxQuoteData{}, fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	var result tmxQuoteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return tmxQuoteData{}, fmt.Errorf("decode failed: %v", err)
	}

	q := result.Data.GetQuoteBySymbol
	if q.Price == 0 {
		return tmxQuoteData{}, fmt.Errorf("no data returned for %s", symbol)
	}

	return tmxQuoteData{
		Name:          q.Name,
		Price:         q.Price,
		PercentChange: q.PercentChange,
	}, nil
}

func tmxFetchTimeSeries(symbol string, interval int) ([]float64, error) {
	query := fmt.Sprintf(`{ getTimeSeriesData(symbol: "%s", interval: %d) { dateTime close } }`, symbol, interval)

	body, _ := json.Marshal(map[string]string{"query": query})

	req, _ := http.NewRequest("POST", tmxGraphQLEndpoint, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result tmxTimeSeriesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	prices := make([]float64, 0, len(result.Data.GetTimeSeriesData))
	for _, point := range result.Data.GetTimeSeriesData {
		prices = append(prices, point.Close)
	}

	return prices, nil
}
