package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eshaffer321/itemize/internal/application/sync/handlers"
	"github.com/eshaffer321/itemize/internal/domain/categorizer"
	"github.com/eshaffer321/itemize/internal/infrastructure/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordingOrchestrator() (*Orchestrator, *storage.MockRepository) {
	store := storage.NewMockRepository()
	return &Orchestrator{
		storage: store,
		logger:  slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}, store
}

func TestHandleResult_DryRunFailuresAreRecordedAsDryRun(t *testing.T) {
	o, store := recordingOrchestrator()
	order := &mockOrder{id: "SYN-DRY-ERR", date: time.Now(), total: 9.99}

	_, _, _ = o.handleResult(order, nil, errors.New("LLM categorization failed"), Options{DryRun: true})
	record, err := store.GetRecord(order.id)
	require.NoError(t, err)
	assert.True(t, record.DryRun, "a dry-run failure must not look like a real-run failure")
}

func TestHandleResult_DryRunSkipsAndPendingAreRecordedAsDryRun(t *testing.T) {
	o, store := recordingOrchestrator()

	skipped := &mockOrder{id: "SYN-DRY-SKIP", date: time.Now(), total: 5}
	_, _, _ = o.handleResult(skipped, &handlers.ProcessResult{Skipped: true, SkipReason: "no matching transaction found"}, nil, Options{DryRun: true})
	record, err := store.GetRecord(skipped.id)
	require.NoError(t, err)
	assert.True(t, record.DryRun)

	pending := &mockOrder{id: "SYN-DRY-PENDING", date: time.Now(), total: 5}
	_, _, _ = o.handleResult(pending, &handlers.ProcessResult{Skipped: true, SkipReason: "payment pending"}, nil, Options{DryRun: true})
	record, err = store.GetRecord(pending.id)
	require.NoError(t, err)
	assert.True(t, record.DryRun)
}

func TestHandleResult_RealRunFailuresAreNotDryRun(t *testing.T) {
	o, store := recordingOrchestrator()
	order := &mockOrder{id: "SYN-REAL-ERR", date: time.Now(), total: 9.99}

	_, _, _ = o.handleResult(order, nil, errors.New("boom"), Options{})
	record, err := store.GetRecord(order.id)
	require.NoError(t, err)
	assert.False(t, record.DryRun)
}

func TestHandleResult_TransientLLMFailureIsMarkedRetryable(t *testing.T) {
	o, store := recordingOrchestrator()
	order := &mockOrder{id: "SYN-LLM-TIMEOUT", date: time.Now(), total: 8.09}

	err := fmt.Errorf("split creation error: LLM categorization failed: %w", fmt.Errorf("%w: %w", categorizer.ErrTransient, context.DeadlineExceeded))
	_, _, _ = o.handleResult(order, nil, err, Options{DryRun: true})
	record, getErr := store.GetRecord(order.id)
	require.NoError(t, getErr)
	assert.Equal(t, "failed", record.Status)
	assert.True(t, strings.HasPrefix(record.ErrorMessage, "[retryable] "))
	assert.False(t, store.IsProcessed(order.id), "it is retried on the next run")
}
