package luna

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// MarketProvider is the interface all market data providers must implement.
// To add a new provider:
//  1. Create a new file: market-provider-<name>.go
//  2. Implement this interface
//  3. Call registerMarketProvider("<name>", &yourProvider{}) in an init() func
type MarketProvider interface {
	// FetchMarkets retrieves market data for the given symbols.
	// apiKey may be empty for providers that don't require one.
	FetchMarkets(requests []marketRequest, apiKey string) (marketList, error)
}

// rateLimitedProvider wraps a MarketProvider with a token bucket rate limiter.
// Credits are consumed per symbol fetched. When the bucket is empty, the
// provider returns a rate-limit error instead of making API calls.
type rateLimitedProvider struct {
	provider     MarketProvider
	mu           sync.Mutex
	credits      int
	maxCredits   int
	refillRate   int           // credits added per refill
	refillPeriod time.Duration // how often credits are refilled
	lastRefill   time.Time
}

func (r *rateLimitedProvider) FetchMarkets(requests []marketRequest, apiKey string) (marketList, error) {
	r.mu.Lock()
	r.refill()
	cost := len(requests) // each symbol = 1 credit
	if r.credits < cost {
		r.mu.Unlock()
		slog.Warn("Market provider rate limited",
			"needed", cost,
			"available", r.credits,
			"refill_in", r.refillPeriod.String(),
		)
		return nil, fmt.Errorf("%w: rate limited — need %d credits, have %d (resets every %s)",
			errNoContent, cost, r.credits, r.refillPeriod.String())
	}
	r.credits -= cost
	r.mu.Unlock()

	return r.provider.FetchMarkets(requests, apiKey)
}

func (r *rateLimitedProvider) refill() {
	now := time.Now()
	elapsed := now.Sub(r.lastRefill)
	periods := int(elapsed / r.refillPeriod)
	if periods > 0 {
		r.credits += periods * r.refillRate
		if r.credits > r.maxCredits {
			r.credits = r.maxCredits
		}
		r.lastRefill = r.lastRefill.Add(time.Duration(periods) * r.refillPeriod)
	}
}

// providerRegistry holds all registered market providers.
var (
	providersMu sync.RWMutex
	providers   = make(map[string]MarketProvider)
)

// registerMarketProvider registers a provider under the given name.
// If rateLimit is non-nil, the provider is wrapped with rate limiting.
func registerMarketProvider(name string, provider MarketProvider, rateLimit *RateLimitConfig) {
	providersMu.Lock()
	defer providersMu.Unlock()

	if rateLimit != nil {
		provider = &rateLimitedProvider{
			provider:     provider,
			credits:      rateLimit.MaxCredits,
			maxCredits:   rateLimit.MaxCredits,
			refillRate:   rateLimit.RefillRate,
			refillPeriod: rateLimit.RefillPeriod,
			lastRefill:   time.Now(),
		}
	}

	providers[strings.ToLower(name)] = provider
}

// getMarketProvider returns the provider registered under the given name.
func getMarketProvider(name string) (MarketProvider, error) {
	providersMu.RLock()
	defer providersMu.RUnlock()

	p, exists := providers[strings.ToLower(name)]
	if !exists {
		available := make([]string, 0, len(providers))
		for k := range providers {
			available = append(available, k)
		}
		return nil, fmt.Errorf("%w: unknown market provider %q (available: %s)",
			errNoContent, name, strings.Join(available, ", "))
	}
	return p, nil
}

// RateLimitConfig defines rate limiting parameters for a provider.
type RateLimitConfig struct {
	MaxCredits   int           // bucket size (burst capacity)
	RefillRate   int           // credits added per refill period
	RefillPeriod time.Duration // how often credits are refilled
}
