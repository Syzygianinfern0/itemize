package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLLMTimeout(t *testing.T) {
	t.Setenv("LLM_TIMEOUT", "")
	cfg := &Config{}
	assert.Equal(t, DefaultLLMTimeout, cfg.LLMTimeout())

	cfg.Categorizer.Timeout = "45s"
	assert.Equal(t, 45*time.Second, cfg.LLMTimeout())

	t.Setenv("LLM_TIMEOUT", "2m")
	assert.Equal(t, 2*time.Minute, cfg.LLMTimeout(), "env overrides config")

	t.Setenv("LLM_TIMEOUT", "soon")
	cfg.Categorizer.Timeout = "-5s"
	assert.Equal(t, DefaultLLMTimeout, cfg.LLMTimeout(), "invalid values fall back to the default")
}
