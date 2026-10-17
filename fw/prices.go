package fw

// Price is the cost per million tokens.
type Price struct {
	Input  float64 `yaml:"input" json:"input"`
	Output float64 `yaml:"output" json:"output"`
}

// Prices resolves the price of a model selector.
type Prices interface {
	Price(selector string) (Price, bool)
}

// PriceTable is Prices from a map keyed by selector.
type PriceTable map[string]Price

// Price implements Prices.
func (t PriceTable) Price(selector string) (Price, bool) {
	p, ok := t[selector]
	return p, ok
}

// Cost returns the cost in US dollars of a usage at a price.
func Cost(u Usage, p Price) float64 {
	return (float64(u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens)*p.Input +
		float64(u.OutputTokens)*p.Output) / 1_000_000
}
