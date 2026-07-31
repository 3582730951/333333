// Package catalog contains SMS market data shared by provider adapters and the
// registration selector. Keeping these wire-neutral types in a leaf package avoids
// import cycles between provider and provider/sms.
package catalog

// CountryInfo is one provider country-catalog entry.
type CountryInfo struct {
	ID   int    `json:"id"`
	Eng  string `json:"eng"`
	Chn  string `json:"chn"`
	ISO  string `json:"iso,omitempty"`
	Dial string `json:"dial,omitempty"`
}

// CountryPrice is the best available offer for one country and service.
type CountryPrice struct {
	Country string  `json:"country"`
	Name    string  `json:"name,omitempty"`
	Price   float64 `json:"price"`
	Count   int     `json:"count"`
	Rank    int     `json:"rank"`
}
