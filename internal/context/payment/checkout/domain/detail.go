package domain

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Detail is the fiscal breakdown a receipt may carry.
type Detail struct {
	// ReceiptType is 0 for a sale or return, and is required.
	ReceiptType int       `json:"receipt_type"`
	Shipping    *Shipping `json:"shipping,omitempty"`
	Items       []Item    `json:"items"`
}

// Shipping is the optional delivery line.
type Shipping struct {
	Title string `json:"title"`
	Price int64  `json:"price"`
}

// Item is one product line. Prices are in tiyin.
type Item struct {
	Title       string `json:"title"`
	Price       int64  `json:"price"`
	Count       int    `json:"count"`
	Code        string `json:"code"`         // ИКПУ product classifier
	PackageCode string `json:"package_code"` // unit of measure
	VatPercent  int    `json:"vat_percent"`
	Discount    int64  `json:"discount,omitempty"`
	Units       int    `json:"units,omitempty"`
}

// Total is what the line costs after its discount.
func (i Item) Total() int64 {
	return i.Price*int64(i.Count) - i.Discount
}

// Validate checks the fields the fiscal rules require.
func (d *Detail) Validate() error {
	if len(d.Items) == 0 {
		return fmt.Errorf("%w: detail needs at least one item", ErrMalformed)
	}

	for i, item := range d.Items {
		switch {
		case item.Title == "":
			return fmt.Errorf("%w: item %d has no title", ErrMalformed, i)
		case item.Price < 0:
			return fmt.Errorf("%w: item %d has a negative price", ErrMalformed, i)
		case item.Count <= 0:
			return fmt.Errorf("%w: item %d has no count", ErrMalformed, i)
		case item.Code == "":
			return fmt.Errorf("%w: item %d has no ИКПУ code", ErrMalformed, i)
		}
	}

	return nil
}

// Total sums every line plus shipping, which is what the payment must equal.
func (d *Detail) Total() int64 {
	var total int64
	for _, item := range d.Items {
		total += item.Total()
	}
	if d.Shipping != nil {
		total += d.Shipping.Price
	}
	return total
}

// EncodeDetail renders a detail as the base64 JSON a checkout form carries.
// Detail holds only marshalable types, so encoding cannot fail.
func EncodeDetail(d *Detail) string {
	raw, _ := json.Marshal(d)
	return base64.StdEncoding.EncodeToString(raw)
}

// DecodeDetail parses the base64 JSON a checkout form carries.
func DecodeDetail(encoded string) (*Detail, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: detail is not base64", ErrMalformed)
	}

	var d Detail
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("%w: detail is not JSON", ErrMalformed)
	}

	return &d, nil
}
