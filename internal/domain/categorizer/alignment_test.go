package categorizer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedChatClient returns one canned response per call and records prompts.
type scriptedChatClient struct {
	responses []CategorizationResult
	prompts   []string
}

func (s *scriptedChatClient) CreateChatCompletion(_ context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	s.prompts = append(s.prompts, req.Messages[len(req.Messages)-1].Content)
	if len(s.responses) == 0 {
		return &ChatCompletionResponse{Choices: []Choice{{Message: Message{Content: `{"categorizations":[]}`}}}}, nil
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	body, _ := json.Marshal(next)
	return &ChatCompletionResponse{Choices: []Choice{{Message: Message{Content: string(body)}}}}, nil
}

var alignmentCategories = []Category{
	{ID: "groceries", Name: "Groceries"},
	{ID: "clothing", Name: "Clothing"},
	{ID: "travel", Name: "Travel"},
}

func categoryIDsByIndex(t *testing.T, result *CategorizationResult, n int) []string {
	t.Helper()
	require.Len(t, result.Categorizations, n)
	ids := make([]string, n)
	for i, c := range result.Categorizations {
		assert.Equal(t, i, c.ItemIndex, "results must be returned in input order")
		ids[i] = c.CategoryID
	}
	return ids
}

func TestCategorizeItems_CachedAndUncachedMixKeepsInputOrder(t *testing.T) {
	cache := NewMemoryCache()
	cache.Set("athletic shorts", "clothing")
	client := &scriptedChatClient{responses: []CategorizationResult{{
		Categorizations: []ItemCategorization{
			{ItemIndex: 1, ItemName: "Spring Water 6pk", CategoryID: "groceries", CategoryName: "Groceries"},
			{ItemIndex: 2, ItemName: "Carry-on Suitcase", CategoryID: "travel", CategoryName: "Travel"},
		},
	}}}
	c := NewCategorizer(client, cache, "gpt-4o-mini")

	items := []Item{
		{Name: "Spring Water 6pk", Price: 8},
		{Name: "Athletic Shorts", Price: 7},
		{Name: "Carry-on Suitcase", Price: 28},
	}
	result, err := c.CategorizeItems(context.Background(), items, alignmentCategories)
	require.NoError(t, err)

	assert.Equal(t, []string{"groceries", "clothing", "travel"}, categoryIDsByIndex(t, result, 3))
	assert.Equal(t, "Athletic Shorts", result.Categorizations[1].ItemName)
}

func TestCategorizeItems_LLMReordersOutput(t *testing.T) {
	client := &scriptedChatClient{responses: []CategorizationResult{{
		Categorizations: []ItemCategorization{
			{ItemIndex: 3, ItemName: "Carry-on Suitcase", CategoryID: "travel"},
			{ItemIndex: 1, ItemName: "Spring Water 6pk", CategoryID: "groceries"},
			{ItemIndex: 2, ItemName: "Athletic Shorts", CategoryID: "clothing"},
		},
	}}}
	c := NewCategorizer(client, NewMemoryCache(), "gpt-4o-mini")

	items := []Item{{Name: "Spring Water 6pk"}, {Name: "Athletic Shorts"}, {Name: "Carry-on Suitcase"}}
	result, err := c.CategorizeItems(context.Background(), items, alignmentCategories)
	require.NoError(t, err)

	assert.Equal(t, []string{"groceries", "clothing", "travel"}, categoryIDsByIndex(t, result, 3))
	assert.Equal(t, "Travel", result.Categorizations[2].CategoryName, "name filled from the category list")
}

func TestCategorizeItems_LLMOmitsItemIsAskedAgain(t *testing.T) {
	client := &scriptedChatClient{responses: []CategorizationResult{
		{Categorizations: []ItemCategorization{
			{ItemIndex: 1, ItemName: "Spring Water 6pk", CategoryID: "groceries"},
			{ItemIndex: 3, ItemName: "Carry-on Suitcase", CategoryID: "travel"},
		}},
		{Categorizations: []ItemCategorization{
			{ItemIndex: 1, ItemName: "Athletic Shorts", CategoryID: "clothing"},
		}},
	}}
	c := NewCategorizer(client, NewMemoryCache(), "gpt-4o-mini")

	items := []Item{{Name: "Spring Water 6pk"}, {Name: "Athletic Shorts"}, {Name: "Carry-on Suitcase"}}
	result, err := c.CategorizeItems(context.Background(), items, alignmentCategories)
	require.NoError(t, err)

	assert.Equal(t, []string{"groceries", "clothing", "travel"}, categoryIDsByIndex(t, result, 3))
	require.Len(t, client.prompts, 2)
	assert.Contains(t, client.prompts[1], "Athletic Shorts")
	assert.NotContains(t, client.prompts[1], "Carry-on Suitcase", "only the missing item is re-sent")
}

func TestCategorizeItems_LLMKeepsOmittingItemFails(t *testing.T) {
	client := &scriptedChatClient{responses: []CategorizationResult{
		{Categorizations: []ItemCategorization{{ItemIndex: 1, ItemName: "Spring Water 6pk", CategoryID: "groceries"}}},
		{Categorizations: []ItemCategorization{}},
	}}
	c := NewCategorizer(client, NewMemoryCache(), "gpt-4o-mini")

	items := []Item{{Name: "Spring Water 6pk"}, {Name: "Athletic Shorts"}}
	_, err := c.CategorizeItems(context.Background(), items, alignmentCategories)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Athletic Shorts")
}

func TestCategorizeItems_DuplicateItemsEachGetACategory(t *testing.T) {
	client := &scriptedChatClient{responses: []CategorizationResult{{
		Categorizations: []ItemCategorization{
			{ItemIndex: 1, ItemName: "Plastic Hangers", CategoryID: "clothing"},
			{ItemIndex: 1, ItemName: "Plastic Hangers", CategoryID: "groceries"}, // duplicate claim: ignored
			{ItemIndex: 2, ItemName: "Bananas", CategoryID: "groceries"},
			{ItemIndex: 3, ItemName: "Plastic Hangers", CategoryID: "clothing"},
		},
	}}}
	c := NewCategorizer(client, NewMemoryCache(), "gpt-4o-mini")

	items := []Item{{Name: "Plastic Hangers"}, {Name: "Bananas"}, {Name: "Plastic Hangers"}}
	result, err := c.CategorizeItems(context.Background(), items, alignmentCategories)
	require.NoError(t, err)

	assert.Equal(t, []string{"clothing", "groceries", "clothing"}, categoryIDsByIndex(t, result, 3))
}

func TestCategorizeItems_MissingIndexFallsBackToName(t *testing.T) {
	client := &scriptedChatClient{responses: []CategorizationResult{{
		Categorizations: []ItemCategorization{
			{ItemName: "athletic shorts", CategoryID: "clothing"},
			{ItemName: "Spring Water 6pk", CategoryID: "groceries"},
		},
	}}}
	c := NewCategorizer(client, NewMemoryCache(), "gpt-4o-mini")

	items := []Item{{Name: "Spring Water 6pk"}, {Name: "Athletic Shorts"}}
	result, err := c.CategorizeItems(context.Background(), items, alignmentCategories)
	require.NoError(t, err)

	assert.Equal(t, []string{"groceries", "clothing"}, categoryIDsByIndex(t, result, 2))
}

func TestCategorizeItems_StaleCacheEntryIsRecategorized(t *testing.T) {
	cache := NewMemoryCache()
	cache.Set("bananas", "deleted-category")
	client := &scriptedChatClient{responses: []CategorizationResult{{
		Categorizations: []ItemCategorization{{ItemIndex: 1, ItemName: "Bananas", CategoryID: "groceries"}},
	}}}
	c := NewCategorizer(client, cache, "gpt-4o-mini")

	result, err := c.CategorizeItems(context.Background(), []Item{{Name: "Bananas"}}, alignmentCategories)
	require.NoError(t, err)
	assert.Equal(t, []string{"groceries"}, categoryIDsByIndex(t, result, 1))
}

func TestBuildPrompt_NumbersItemsAndAsksForIndex(t *testing.T) {
	c := NewCategorizer(&scriptedChatClient{}, NewMemoryCache(), "gpt-4o-mini")
	prompt := c.buildPrompt([]Item{{Name: "A", Price: 1}, {Name: "B", Price: 2}}, alignmentCategories)
	assert.Contains(t, prompt, "1. A")
	assert.Contains(t, prompt, "2. B")
	assert.True(t, strings.Contains(prompt, `"item_index"`), "prompt must request item_index")
}
