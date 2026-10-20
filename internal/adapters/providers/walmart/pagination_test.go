package walmart

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/adapters/providers"
	walmartclient "github.com/eshaffer321/walmart-client-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagedWalmartClient serves purchase history in pages keyed by cursor.
type pagedWalmartClient struct {
	fakeWalmartClient
	pages   map[string][]walmartclient.OrderSummary
	next    map[string]string
	cursors []string
}

func (p *pagedWalmartClient) GetPurchaseHistory(_ context.Context, req walmartclient.PurchaseHistoryRequest) (*walmartclient.PurchaseHistoryResponse, error) {
	p.cursors = append(p.cursors, req.Cursor)
	resp := &walmartclient.PurchaseHistoryResponse{}
	resp.Data.OrderHistoryV2.OrderGroups = p.pages[req.Cursor]
	resp.Data.OrderHistoryV2.PageInfo.NextPageCursor = p.next[req.Cursor]
	return resp, nil
}

func summaries(prefix string, n int) []walmartclient.OrderSummary {
	out := make([]walmartclient.OrderSummary, n)
	for i := range out {
		out[i] = walmartclient.OrderSummary{OrderID: fmt.Sprintf("%s-%d", prefix, i), FulfillmentType: "IN_STORE"}
	}
	return out
}

func fetchOpts() providers.FetchOptions {
	return providers.FetchOptions{StartDate: time.Now().AddDate(-1, 0, 0), EndDate: time.Now()}
}

func TestFetchOrders_FollowsPageCursors(t *testing.T) {
	client := &pagedWalmartClient{
		pages: map[string][]walmartclient.OrderSummary{"": summaries("p1", 10), "c2": summaries("p2", 10), "c3": summaries("p3", 3)},
		next:  map[string]string{"": "c2", "c2": "c3"},
	}
	orders, err := newProvider(client, nil).FetchOrders(context.Background(), fetchOpts())
	require.NoError(t, err)

	assert.Equal(t, []string{"", "c2", "c3"}, client.cursors)
	assert.Len(t, orders, 23, "orders beyond the first page of 10 must be fetched")
	assert.Equal(t, "p3-2", orders[22].GetID())
}

func TestFetchOrders_DeduplicatesAcrossPages(t *testing.T) {
	client := &pagedWalmartClient{
		pages: map[string][]walmartclient.OrderSummary{"": summaries("a", 2), "c2": summaries("a", 3)},
		next:  map[string]string{"": "c2"},
	}
	orders, err := newProvider(client, nil).FetchOrders(context.Background(), fetchOpts())
	require.NoError(t, err)
	assert.Len(t, orders, 3)
}

func TestFetchOrders_StopsAtPageCap(t *testing.T) {
	client := &pagedWalmartClient{
		pages: map[string][]walmartclient.OrderSummary{"loop": summaries("x", 1), "": summaries("x", 1)},
		next:  map[string]string{"": "loop", "loop": "loop"},
	}
	_, err := newProvider(client, nil).FetchOrders(context.Background(), fetchOpts())
	require.NoError(t, err)
	assert.Len(t, client.cursors, maxHistoryPages)
}
