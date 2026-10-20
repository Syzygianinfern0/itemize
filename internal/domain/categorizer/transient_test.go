package categorizer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type timeoutChatClient struct{ calls int }

func (c *timeoutChatClient) CreateChatCompletion(context.Context, ChatCompletionRequest) (*ChatCompletionResponse, error) {
	c.calls++
	return nil, context.DeadlineExceeded
}

type rejectingChatClient struct{}

func (rejectingChatClient) CreateChatCompletion(context.Context, ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, errors.New("OpenAI API error: invalid api key")
}

func TestCategorizeItems_TimeoutsAreRetriedThenMarkedTransient(t *testing.T) {
	if testing.Short() {
		t.Skip("waits through retry backoff")
	}
	client := &timeoutChatClient{}
	start := time.Now()
	_, err := NewCategorizer(client, NewMemoryCache(), "m").CategorizeItems(context.Background(), []Item{{Name: "Bread"}}, alignmentCategories)
	require.Error(t, err)
	assert.Equal(t, maxRetries, client.calls, "timeouts are retried")
	assert.True(t, errors.Is(err, ErrTransient))
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
	assert.GreaterOrEqual(t, time.Since(start), baseDelay)
}

func TestCategorizeItems_PermanentErrorsAreNotTransient(t *testing.T) {
	_, err := NewCategorizer(rejectingChatClient{}, NewMemoryCache(), "m").CategorizeItems(context.Background(), []Item{{Name: "Bread"}}, alignmentCategories)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrTransient))
}
