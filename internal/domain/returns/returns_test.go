package returns

import (
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type item struct {
	name  string
	price float64
	qty   float64
}

func (i item) GetName() string        { return i.name }
func (i item) GetPrice() float64      { return i.price }
func (i item) GetQuantity() float64   { return i.qty }
func (i item) GetUnitPrice() float64  { return i.price / i.qty }
func (i item) GetDescription() string { return "" }
func (i item) GetSKU() string         { return "" }
func (i item) GetCategory() string    { return "" }

type order struct {
	id                   string
	date                 time.Time
	total, subtotal, tax float64
	items                []providers.OrderItem
	returned             []providers.OrderItem
}

func (o *order) GetID() string                   { return o.id }
func (o *order) GetDate() time.Time              { return o.date }
func (o *order) GetTotal() float64               { return o.total }
func (o *order) GetSubtotal() float64            { return o.subtotal }
func (o *order) GetTax() float64                 { return o.tax }
func (o *order) GetTip() float64                 { return 0 }
func (o *order) GetFees() float64                { return 0 }
func (o *order) GetItems() []providers.OrderItem { return o.items }
func (o *order) GetProviderName() string         { return "walmart" }
func (o *order) GetRawData() interface{}         { return nil }
func (o *order) GetRefundItems() ([]providers.OrderItem, error) {
	return o.returned, nil
}

var refundDay = time.Date(2025, 8, 3, 0, 0, 0, 0, time.UTC)

// A 10% tax store receipt with three items.
func storeReceipt(daysBefore int) *order {
	return &order{
		id: "R1", date: refundDay.AddDate(0, 0, -daysBefore), subtotal: 40, tax: 4, total: 44,
		items: []providers.OrderItem{
			item{"Fire Extinguisher", 25, 1},
			item{"Ice Trays (2)", 4, 2},
			item{"Cooler", 11, 1},
		},
	}
}

func TestFind_SingleItemWithOrderTaxRate(t *testing.T) {
	c, reason := Pick(DefaultFinder.Find(27.50, refundDay, []providers.Order{storeReceipt(70)}))
	require.NotNil(t, c, reason)
	assert.Equal(t, "Fire Extinguisher", c.Items[0].GetName())
	assert.False(t, c.WholeOrder)
	assert.Equal(t, 27.50, c.Expected)
}

func TestFind_OneUnitOfAMultiple(t *testing.T) {
	c, reason := Pick(DefaultFinder.Find(2.20, refundDay, []providers.Order{storeReceipt(10)}))
	require.NotNil(t, c, reason)
	assert.Equal(t, "Ice Trays (2)", c.Items[0].GetName())
}

func TestFind_WholeOrderReturn(t *testing.T) {
	c, reason := Pick(DefaultFinder.Find(44.00, refundDay, []providers.Order{storeReceipt(5)}))
	require.NotNil(t, c, reason)
	assert.True(t, c.WholeOrder)
	assert.Len(t, c.Items, 3)
}

func TestFind_OutsideReturnWindow(t *testing.T) {
	_, reason := Pick(DefaultFinder.Find(27.50, refundDay, []providers.Order{storeReceipt(120)}))
	assert.Contains(t, reason, "no purchase")
	// A purchase after the refund cannot be its source.
	_, reason = Pick(DefaultFinder.Find(27.50, refundDay, []providers.Order{storeReceipt(-5)}))
	assert.Contains(t, reason, "no purchase")
}

func TestPick_AmbiguousMatchesAreNotGuessed(t *testing.T) {
	a, b := storeReceipt(10), storeReceipt(20)
	b.id = "R2"
	c, reason := Pick(DefaultFinder.Find(12.10, refundDay, []providers.Order{a, b}))
	assert.Nil(t, c)
	assert.Contains(t, reason, "ambiguous: 2")
}

func TestFind_MarkedReturnWinsWithLooserTax(t *testing.T) {
	// Online order: item-level tax differs from the order average, so the
	// refund (27.88 + 10.3%) does not equal price * order rate.
	online := &order{
		id: "W1", date: refundDay.AddDate(0, 0, -15), subtotal: 43.83, tax: 3.77, total: 19.20,
		items: []providers.OrderItem{
			item{"Shorts", 6.98, 1}, item{"Water 6pk", 8.97, 1}, item{"Carry-on", 27.88, 1},
		},
		returned: []providers.OrderItem{item{"Carry-on", 27.88, 1}},
	}
	decoy := &order{id: "S9", date: refundDay.AddDate(0, 0, -3), subtotal: 30.74, total: 30.74,
		items: []providers.OrderItem{item{"Something", 30.74, 1}}}

	c, reason := Pick(DefaultFinder.Find(30.74, refundDay, []providers.Order{online, decoy}))
	require.NotNil(t, c, reason)
	assert.True(t, c.Marked)
	assert.Equal(t, "W1", c.Order.GetID())
	assert.Equal(t, "Carry-on", c.Items[0].GetName())
}
