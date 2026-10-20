package categorizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Item represents a Walmart item to be categorized
type Item struct {
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
	Quantity int     `json:"quantity,omitempty"`
}

// Category represents a Monarch category
type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ItemCategorization represents the categorization result for a single item.
// ItemIndex is the item's position in the slice passed to CategorizeItems.
type ItemCategorization struct {
	ItemIndex    int     `json:"item_index"`
	ItemName     string  `json:"item_name"`
	CategoryID   string  `json:"category_id"`
	CategoryName string  `json:"category_name"`
	Confidence   float64 `json:"confidence"`
}

// CategorizationResult contains all categorization results
type CategorizationResult struct {
	Categorizations []ItemCategorization `json:"categorizations"`
}

// llmCategorization is one entry of the LLM's JSON response. ItemNumber is the
// 1-based number the prompt gave the item.
type llmCategorization struct {
	ItemNumber   int     `json:"item_index"`
	ItemName     string  `json:"item_name"`
	CategoryID   string  `json:"category_id"`
	CategoryName string  `json:"category_name"`
	Confidence   float64 `json:"confidence"`
}

type llmResponse struct {
	Categorizations []llmCategorization `json:"categorizations"`
}

// Chat-completion request/response types.
// The shape mirrors OpenAI's chat-completions API and is treated as the
// shared vocabulary that adapters translate to and from. Adapters for
// non-OpenAI backends (e.g. Anthropic) consume the same types.
type ChatCompletionRequest struct {
	Model           string          `json:"model"`
	Messages        []Message       `json:"messages"`
	Temperature     *float64        `json:"temperature,omitempty"`
	ReasoningEffort *string         `json:"reasoning_effort,omitempty"`
	ResponseFormat  *ResponseFormat `json:"response_format,omitempty"`
}

type ResponseFormat struct {
	Type string `json:"type"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	Choices []Choice `json:"choices"`
}

type Choice struct {
	Message Message `json:"message"`
}

// ChatClient is the LLM-backend interface the categorizer depends on.
// Concrete implementations live in internal/adapters/clients/{openai,anthropic}.
type ChatClient interface {
	CreateChatCompletion(ctx context.Context, request ChatCompletionRequest) (*ChatCompletionResponse, error)
}

// Cache interface for category mappings
type Cache interface {
	Get(key string) (string, bool)
	Set(key string, value string)
}

// Categorizer handles item categorization using a pluggable LLM backend.
type Categorizer struct {
	client ChatClient
	cache  Cache
	Model  string
}

// NewCategorizer creates a new categorizer
func NewCategorizer(client ChatClient, cache Cache, model string) *Categorizer {
	if strings.TrimSpace(model) == "" {
		model = DefaultModel
	}

	return &Categorizer{
		client: client,
		cache:  cache,
		Model:  model,
	}
}

const DefaultModel = "gpt-6-luna"

// llmAttempts is how many times uncategorized items are sent to the LLM. Items
// the LLM omits or gives an unknown category are re-sent once.
const llmAttempts = 2

// CategorizeItems categorizes a list of items using available categories.
//
// The result has exactly one entry per input item, in input order, with
// ItemIndex set to the item's position. Cached and LLM results are merged by
// position, never appended, so callers can pair results with items safely.
// If the LLM still leaves an item uncategorized after a retry, an error is
// returned rather than a partial result.
func (c *Categorizer) CategorizeItems(ctx context.Context, items []Item, categories []Category) (*CategorizationResult, error) {
	if len(items) == 0 {
		return &CategorizationResult{Categorizations: []ItemCategorization{}}, nil
	}

	categoryByID := make(map[string]Category, len(categories))
	categoryByName := make(map[string]Category, len(categories))
	for _, cat := range categories {
		categoryByID[cat.ID] = cat
		categoryByName[strings.ToLower(cat.Name)] = cat
	}

	assigned := make([]*ItemCategorization, len(items))
	var pending []int
	for i, item := range items {
		if categoryID, found := c.cache.Get(c.normalizeItemName(item.Name)); found {
			// A cached ID for a category that no longer exists is ignored.
			if cat, ok := categoryByID[categoryID]; ok {
				assigned[i] = &ItemCategorization{
					ItemIndex:    i,
					ItemName:     item.Name,
					CategoryID:   cat.ID,
					CategoryName: cat.Name,
					Confidence:   1.0, // 100% confidence for cached items
				}
				continue
			}
		}
		pending = append(pending, i)
	}

	for attempt := 0; attempt < llmAttempts && len(pending) > 0; attempt++ {
		batch := make([]Item, len(pending))
		for k, idx := range pending {
			batch[k] = items[idx]
		}

		llmResult, err := c.callLLM(ctx, batch, categories)
		if err != nil {
			return nil, fmt.Errorf("LLM categorization failed: %w", err)
		}

		for _, entry := range llmResult.Categorizations {
			k := c.resolveBatchPosition(entry, batch, pending, assigned)
			if k < 0 {
				continue
			}
			cat, ok := categoryByID[entry.CategoryID]
			if !ok {
				// Recover an invented ID via the category name; otherwise leave
				// the item pending so it is asked again.
				if cat, ok = categoryByName[strings.ToLower(entry.CategoryName)]; !ok {
					continue
				}
			}
			idx := pending[k]
			assigned[idx] = &ItemCategorization{
				ItemIndex:    idx,
				ItemName:     items[idx].Name,
				CategoryID:   cat.ID,
				CategoryName: cat.Name,
				Confidence:   entry.Confidence,
			}
			c.cache.Set(c.normalizeItemName(items[idx].Name), cat.ID)
		}

		var still []int
		for _, idx := range pending {
			if assigned[idx] == nil {
				still = append(still, idx)
			}
		}
		pending = still
	}

	if len(pending) > 0 {
		names := make([]string, len(pending))
		for k, idx := range pending {
			names[k] = fmt.Sprintf("%q", items[idx].Name)
		}
		return nil, fmt.Errorf("LLM categorization incomplete: no valid category for %d item(s): %s",
			len(pending), strings.Join(names, ", "))
	}

	result := &CategorizationResult{Categorizations: make([]ItemCategorization, len(items))}
	for i, cat := range assigned {
		result.Categorizations[i] = *cat
	}
	return result, nil
}

// resolveBatchPosition returns the position in batch that an LLM entry refers
// to, or -1. It prefers the item number from the prompt and falls back to an
// exact (case-insensitive) name match. Items that already have a category are
// never reassigned, so duplicate entries are ignored.
func (c *Categorizer) resolveBatchPosition(entry llmCategorization, batch []Item, pending []int, assigned []*ItemCategorization) int {
	if k := entry.ItemNumber - 1; k >= 0 && k < len(batch) {
		if assigned[pending[k]] == nil {
			return k
		}
		return -1
	}
	name := c.normalizeItemName(entry.ItemName)
	for k, item := range batch {
		if assigned[pending[k]] == nil && c.normalizeItemName(item.Name) == name {
			return k
		}
	}
	return -1
}

// ErrTransient marks a categorization failure caused by a transient LLM
// problem (timeout, connection error, 5xx) that persisted through retries.
// The order is worth retrying on a later run.
var ErrTransient = errors.New("LLM temporarily unavailable")

// Retry configuration
const (
	maxRetries = 3
	baseDelay  = 1 * time.Second
	maxDelay   = 8 * time.Second
)

// isRetryableError determines if an error is transient and worth retrying
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	// Context deadline exceeded (timeout)
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Connection errors, timeouts - check error message
	errMsg := err.Error()
	return strings.Contains(errMsg, "timeout") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "connection reset") ||
		strings.Contains(errMsg, "temporary failure") ||
		strings.Contains(errMsg, "502") ||
		strings.Contains(errMsg, "503") ||
		strings.Contains(errMsg, "504")
}

// callLLM makes the actual API call to the LLM with retry logic
func (c *Categorizer) callLLM(ctx context.Context, items []Item, categories []Category) (*llmResponse, error) {
	prompt := c.buildPrompt(items, categories)

	request := ChatCompletionRequest{
		Model: c.Model,
		ResponseFormat: &ResponseFormat{
			Type: "json_object",
		},
		Messages: []Message{
			{
				Role:    "system",
				Content: "You are a helpful assistant that categorizes shopping items into appropriate categories. Always respond with valid JSON.",
			},
			{
				Role:    "user",
				Content: prompt,
			},
		},
	}
	if isReasoningModel(c.Model) {
		reasoningEffort := "low"
		request.ReasoningEffort = &reasoningEffort
	} else {
		temperature := 0.1
		request.Temperature = &temperature
	}

	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		response, err := c.client.CreateChatCompletion(ctx, request)
		if err != nil {
			lastErr = err
			if !isRetryableError(err) || attempt == maxRetries {
				break
			}
			// Exponential backoff: 1s, 2s, 4s
			delay := baseDelay * time.Duration(1<<(attempt-1))
			if delay > maxDelay {
				delay = maxDelay
			}
			time.Sleep(delay)
			continue
		}

		if len(response.Choices) == 0 {
			return nil, fmt.Errorf("no response from LLM")
		}

		// Parse JSON response
		var result llmResponse
		if err := json.Unmarshal([]byte(response.Choices[0].Message.Content), &result); err != nil {
			return nil, fmt.Errorf("failed to parse LLM response: %w", err)
		}

		return &result, nil
	}

	if isRetryableError(lastErr) {
		return nil, fmt.Errorf("%w: %w after %d attempts", ErrTransient, lastErr, maxRetries)
	}
	return nil, fmt.Errorf("%w after %d attempts", lastErr, maxRetries)
}

// isReasoningModel reports whether model is a GPT-5-or-later reasoning model,
// which takes reasoning_effort and rejects a custom temperature.
func isReasoningModel(model string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-")
	if !ok {
		return false
	}
	end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	if end == -1 {
		end = len(rest)
	}
	generation, err := strconv.Atoi(rest[:end])
	return err == nil && generation >= 5
}

// buildPrompt creates the prompt for OpenAI
func (c *Categorizer) buildPrompt(items []Item, categories []Category) string {
	var itemsList strings.Builder
	for i, item := range items {
		fmt.Fprintf(&itemsList, "%d. %s - $%.2f\n", i+1, item.Name, item.Price)
	}

	var categoriesList strings.Builder
	for _, cat := range categories {
		fmt.Fprintf(&categoriesList, "- %s (ID: %s)\n", cat.Name, cat.ID)
	}

	prompt := fmt.Sprintf(`Please categorize the following items into the most appropriate categories.

Items to categorize:
%s

Available categories (use ONLY these exact IDs):
%s

IMPORTANT Instructions:
1. Match each item to the MOST appropriate category from the list above. Return exactly one entry per numbered item, with "item_index" set to the item's number
2. You MUST use the exact category_id values shown in the list — do NOT invent IDs or use words like "Uncategorized"
3. If no category is a good fit, pick the closest one available
4. Distinguish between different types of items:
   - "Groceries" should be used ONLY for food items (milk, bread, meat, produce, snacks, beverages)
   - "Home & Garden" for cleaning supplies, paper products, laundry, trash bags, home maintenance
   - "Personal Care" for toiletries: shampoo, deodorant, toothpaste, soap, cosmetics
   - "Health & Wellness" for vitamins, medicine, first aid
5. Do NOT put non-food items in Groceries
6. Provide a confidence score (0.0 to 1.0) for each categorization

Return the result as a JSON object with this structure:
{
  "categorizations": [
    {
      "item_index": 1,
      "item_name": "exact item name",
      "category_id": "exact ID from the list above",
      "category_name": "category name",
      "confidence": 0.95
    }
  ]
}`, itemsList.String(), categoriesList.String())

	return prompt
}

// normalizeItemName normalizes an item name for cache key
func (c *Categorizer) normalizeItemName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
