package walmart

import (
	"testing"

	walmartclient "github.com/eshaffer321/walmart-client-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A walmart.com order paid through PayPal: PayPal bills the funding card, so
// Monarch shows "PAYPAL *WALMART COM" charges and refunds for it.
func paypalOrder() *Order {
	return &Order{
		walmartOrder: &walmartclient.Order{ID: "SYN-PAYPAL"},
		ledgerCache: &walmartclient.OrderLedger{
			OrderID: "SYN-PAYPAL",
			PaymentMethods: []walmartclient.PaymentMethodCharges{
				{PaymentType: "PAYPAL", CardType: "PAYPAL", FinalCharges: []float64{-4.50, 4.50, 4.50}},
				{PaymentType: "GIFTCARD", FinalCharges: []float64{2.00}},
			},
		},
	}
}

func TestPayPalChargesAreBankCharges(t *testing.T) {
	charges, err := paypalOrder().GetFinalCharges()
	require.NoError(t, err)
	assert.Equal(t, []float64{4.50, 4.50}, charges, "PayPal charges reach the bank feed; gift cards don't")
}

func TestPayPalRefundsAreBankRefunds(t *testing.T) {
	refunds, err := paypalOrder().GetRefundCharges()
	require.NoError(t, err)
	assert.Equal(t, []float64{4.50}, refunds)
}

func TestLedgerSkippable(t *testing.T) {
	card := []walmartclient.OrderPaymentMethod{{PaymentType: "CREDITCARD"}}
	order := &Order{walmartOrder: &walmartclient.Order{ID: "A", PaymentMethods: card}}
	assert.True(t, order.LedgerSkippable())

	giftAndCard := &Order{walmartOrder: &walmartclient.Order{ID: "B", PaymentMethods: append(card, walmartclient.OrderPaymentMethod{PaymentType: "GIFTCARD"})}}
	assert.False(t, giftAndCard.LedgerSkippable(), "a gift card makes the charge differ from the total")

	returned := &Order{walmartOrder: &walmartclient.Order{ID: "C", PaymentMethods: card, Groups: []walmartclient.OrderGroup{{
		Categories: []walmartclient.OrderCategory{{Items: []walmartclient.OrderItem{{ID: "1", ReturnID: "r1"}}}},
	}}}}
	assert.False(t, returned.LedgerSkippable(), "a returned item means the ledger may list a refund")
}
