package matcher

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAnySubsetSums(t *testing.T) {
	amounts := []float64{-1.84, -5.86, -42.24, -12.00}
	assert.True(t, AnySubsetSums(amounts, []float64{49.94}, 4, 0.011), "three charges")
	assert.True(t, AnySubsetSums(amounts, []float64{99, 12.00}, 4, 0.011), "any target")
	assert.False(t, AnySubsetSums(amounts, []float64{61.94}, 2, 0.011), "needs more than maxSize")
	assert.False(t, AnySubsetSums(amounts, []float64{3.00}, 4, 0.011))
	assert.False(t, AnySubsetSums(nil, []float64{0}, 4, 0.011), "the empty set never matches")
}
