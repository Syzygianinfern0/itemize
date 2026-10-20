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
