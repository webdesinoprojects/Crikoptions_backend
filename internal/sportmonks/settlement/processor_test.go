package settlement

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
)

type fakeStore struct {
	job       *store.SettlementJob
	completed string
	failed    string
	retryAt   time.Time
}

func (f *fakeStore) ClaimSettlementJob(context.Context, string, time.Time, time.Duration) (*store.SettlementJob, error) {
	job := f.job
	f.job = nil
	return job, nil
}
func (f *fakeStore) CompleteSettlementJob(_ context.Context, id, _ string, _ time.Time) error {
	f.completed = id
	return nil
}
func (f *fakeStore) RenewSettlementJob(context.Context, string, string, time.Time, time.Duration) error {
	return nil
}
func (f *fakeStore) ClaimTradingGateJob(context.Context, string, time.Time, time.Duration) (*store.TradingGateJob, error) {
	return nil, nil
}
func (f *fakeStore) RenewTradingGateJob(context.Context, string, string, time.Time, time.Duration) error {
	return nil
}
func (f *fakeStore) CompleteTradingGateJob(context.Context, string, string, time.Time) error {
	return nil
}
func (f *fakeStore) FailTradingGateJob(context.Context, string, string, error, time.Time, time.Time) error {
	return nil
}
func (f *fakeStore) FailSettlementJob(_ context.Context, id, _ string, _ error, _ time.Time, retryAt time.Time) error {
	f.failed = id
	f.retryAt = retryAt
	return nil
}

type fakeRunner struct {
	settlement        string
	err               error
	cancelGateVersion int64
}

func (r *fakeRunner) SettleProviderInnings(_ context.Context, matchID string, innings int, finalRevision int64) error {
	r.settlement = fmt.Sprintf("%d:%s:%d", innings, matchID, finalRevision)
	return r.err
}
func (r *fakeRunner) CancelProviderWorkingOrders(_ context.Context, _ string, gateTradingVersion int64) (int, error) {
	r.cancelGateVersion = gateTradingVersion
	return 0, nil
}
func (r *fakeRunner) VoidProviderInningsMarket(context.Context, string, int) error {
	return nil
}

func TestProcessorCompletesSecondInningsJob(t *testing.T) {
	jobs := &fakeStore{job: &store.SettlementJob{ID: "m:2", MatchID: "m", Innings: 2, FormulaVersion: "innings_score_v1", FinalRevision: 17}}
	runner := &fakeRunner{}
	processor, err := NewProcessor(jobs, runner, "api-1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.runNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runner.settlement != "2:m:17" || jobs.completed != "m:2" || jobs.failed != "" {
		t.Fatalf("runner=%q completed=%q failed=%q", runner.settlement, jobs.completed, jobs.failed)
	}
}

func TestProcessorPersistsRetryableFailure(t *testing.T) {
	jobs := &fakeStore{job: &store.SettlementJob{ID: "m:1", MatchID: "m", Innings: 1, FormulaVersion: "innings_score_v1", Attempts: 3}}
	runner := &fakeRunner{err: errors.New("transient")}
	processor, _ := NewProcessor(jobs, runner, "api-1", time.Minute)
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	processor.now = func() time.Time { return now }
	if err := processor.runNext(context.Background()); err == nil || jobs.failed != "m:1" || jobs.completed != "" {
		t.Fatalf("error=%v completed=%q failed=%q", err, jobs.completed, jobs.failed)
	}
	if want := now.Add(20 * time.Second); !jobs.retryAt.Equal(want) {
		t.Fatalf("retryAt = %s, want %s", jobs.retryAt, want)
	}
}

func TestJobRetryDelayIsBounded(t *testing.T) {
	if got := jobRetryDelay(1); got != 5*time.Second {
		t.Fatalf("first delay = %s", got)
	}
	if got := jobRetryDelay(100); got != 5*time.Minute {
		t.Fatalf("bounded delay = %s", got)
	}
}
