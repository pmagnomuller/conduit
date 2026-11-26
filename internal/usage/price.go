package usage

import (
	"strings"

	"github.com/pedro-mueller/conduit/internal/config"
)

// BillingFor decides how a provider's traffic is billed:
//
//   - anthropic: subscription (Claude Code forwards its plan OAuth credential)
//     unless ANTHROPIC_API_KEY is set, which makes it metered API traffic.
//   - glm (Z.ai): subscription (a coding plan) unless [spend] glm_billing =
//     "metered" says the key is pay-as-you-go.
//   - deepseek, and anything unknown: metered. Assuming a provider is paid
//     errs toward an alert, never toward hiding spend.
func BillingFor(cfg config.Config, provider string) string {
	switch provider {
	case "anthropic":
		if strings.TrimSpace(cfg.AnthropicAPIKey) != "" {
			return BillingMetered
		}
		return BillingSubscription
	case "glm":
		if cfg.Spend.GLMBilling == BillingMetered {
			return BillingMetered
		}
		return BillingSubscription
	}
	return BillingMetered
}

// Prices is the price table used to cost a request, USD per million tokens.
// It is the Jev catalog (which already carries list prices per provider/model
// and is where prices are configured) overlaid with [spend.prices] for models
// the catalog does not name.
type Prices struct {
	byKey map[string]config.Candidate
	// top is the most expensive entry per provider: the conservative price for
	// a model the table does not know.
	top map[string]config.Candidate
}

// NewPrices indexes the catalog and config overrides into a lookup.
func NewPrices(cfg config.Config) *Prices {
	p := &Prices{byKey: map[string]config.Candidate{}, top: map[string]config.Candidate{}}
	add := func(c config.Candidate) {
		if c.Model == "" || c.PriceIn <= 0 && c.PriceOut <= 0 {
			return
		}
		p.byKey[c.Key()] = c
		if t, ok := p.top[c.Provider]; !ok || c.PriceOut > t.PriceOut {
			p.top[c.Provider] = c
		}
	}
	// Defaults first so a catalog override without prices still costs.
	for _, c := range config.DefaultCatalog() {
		add(c)
	}
	for _, c := range cfg.Jev.Catalog {
		add(c)
	}
	for _, c := range cfg.Spend.Prices {
		add(c)
	}
	return p
}

// Price returns the price for provider/model and whether it is the model's own
// price. A dated id ("claude-sonnet-5-20260101") matches its undated entry. An
// unknown model of a known provider is priced at that provider's most
// expensive entry, with exact=false; an unknown provider costs nothing.
func (p *Prices) Price(provider, model string) (config.Candidate, bool) {
	if c, ok := p.byKey[provider+"/"+model]; ok {
		return c, true
	}
	if i := strings.Index(model, "-20"); i > 0 {
		if c, ok := p.byKey[provider+"/"+model[:i]]; ok {
			return c, true
		}
	}
	return p.top[provider], false
}

// USD costs one response's usage on provider/model, rounded to 1/10000 USD,
// and reports whether the price was the model's own (false: estimated from the
// provider's most expensive model).
//
// Counts follow the Anthropic Messages API, which every upstream here speaks:
// input_tokens excludes the cache-read and cache-creation tokens, which are
// reported (and billed) separately. A missing cache rate falls back to the
// input rate — an overestimate, which is the safe direction for a spend alert.
func (p *Prices) USD(provider, model string, u Usage) (float64, bool) {
	price, exact := p.Price(provider, model)
	read := price.PriceCacheRead
	if read <= 0 {
		read = price.PriceIn
	}
	write := price.PriceCacheWrite
	if write <= 0 {
		write = price.PriceIn
	}
	usd := (float64(u.InputTokens)*price.PriceIn +
		float64(u.CacheReadTokens)*read +
		float64(u.CacheCreationTokens)*write +
		float64(u.OutputTokens)*price.PriceOut) / 1e6
	// Four decimals: one cheap call costs a fraction of a cent, and dropping
	// that would make day totals drift low as calls accumulate.
	return float64(int64(usd*1e4+0.5)) / 1e4, exact
}
