package settlement

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/webdesinoprojects/Crikoptions/backend/internal/sportmonks/store"
)

type JobStore interface {
	ClaimSettlementJob(context.Context, string, time.Time, time.Duration) (*store.SettlementJob, error)
	RenewSettlementJob(context.Context, string, string, time.Time, time.Duration) error
	CompleteSettlementJob(context.Context, string, string, time.Time) error
	FailSettlementJob(context.Context, string, string, error, time.Time, time.Time) error
	ClaimTradingGateJob(context.Context, string, time.Time, time.Duration) (*store.TradingGateJob, error)
	RenewTradingGateJob(context.Context, string, string, time.Time, time.Duration) error
	CompleteTradingGateJob(context.Context, string, string, time.Time) error
	FailTradingGateJob(context.Context, string, string, error, time.Time, time.Time) error
}

type Runner interface {
	SettleProviderInnings(context.Context, string, int, int64) error
	CancelProviderWorkingOrders(context.Context, string) (int, error)
	VoidProviderInningsMarket(context.Context, string, int) error
}

type Processor struct {
	store    JobStore
	runner   Runner
	owner    string
	leaseTTL time.Duration
	now      func() time.Time
}

func NewProcessor(jobStore JobStore, runner Runner, owner string, leaseTTL time.Duration) (*Processor, error) {
	if jobStore == nil || runner == nil {
		return nil, errors.New("settlement processor requires a store and runner")
	}
	if strings.TrimSpace(owner) == "" || leaseTTL <= 0 {
		return nil, errors.New("settlement processor requires an owner and positive lease TTL")
	}
	return &Processor{store: jobStore, runner: runner, owner: owner, leaseTTL: leaseTTL, now: time.Now}, nil
}

func (p *Processor) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := p.runNext(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Sportmonks settlement processor: %v", err)
		}
		if err := p.runNextGate(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Sportmonks trading-gate processor: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (p *Processor) runNextGate(ctx context.Context) error {
	job, err := p.store.ClaimTradingGateJob(ctx, p.owner, p.now().UTC(), p.leaseTTL)
	if err != nil || job == nil {
		return err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	leaseLost := make(chan error, 1)
	leaseDone := make(chan struct{})
	go p.maintainGateLease(jobCtx, job.ID, cancel, leaseLost, leaseDone)
	_, err = p.runner.CancelProviderWorkingOrders(jobCtx, job.MatchID)
	cancel()
	<-leaseDone
	select {
	case leaseErr := <-leaseLost:
		return fmt.Errorf("trading gate %s lease: %w", job.ID, leaseErr)
	default:
	}
	if err != nil {
		now := p.now().UTC()
		if failErr := p.store.FailTradingGateJob(ctx, job.ID, p.owner, err, now, now.Add(jobRetryDelay(job.Attempts))); failErr != nil {
			return fmt.Errorf("trading gate %s failed: %v; recording failure: %w", job.ID, err, failErr)
		}
		return fmt.Errorf("trading gate %s: %w", job.ID, err)
	}
	if err := p.store.CompleteTradingGateJob(ctx, job.ID, p.owner, p.now().UTC()); err != nil {
		return fmt.Errorf("complete trading gate %s: %w", job.ID, err)
	}
	return nil
}

func (p *Processor) runNext(ctx context.Context) error {
	now := p.now().UTC()
	job, err := p.store.ClaimSettlementJob(ctx, p.owner, now, p.leaseTTL)
	if err != nil || job == nil {
		return err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	leaseLost := make(chan error, 1)
	leaseDone := make(chan struct{})
	go p.maintainLease(jobCtx, job.ID, cancel, leaseLost, leaseDone)
	if job.FormulaVersion != "innings_score_v1" || job.Innings < 1 || job.Innings > 2 || (job.Action != "void" && job.FinalRevision <= 0) {
		err = fmt.Errorf("unsupported settlement contract %s innings %d revision %d", job.FormulaVersion, job.Innings, job.FinalRevision)
	} else if job.Action == "void" {
		err = p.runner.VoidProviderInningsMarket(jobCtx, job.MatchID, job.Innings)
	} else {
		err = p.runner.SettleProviderInnings(jobCtx, job.MatchID, job.Innings, job.FinalRevision)
	}
	cancel()
	<-leaseDone
	select {
	case leaseErr := <-leaseLost:
		return fmt.Errorf("settlement %s lease: %w", job.ID, leaseErr)
	default:
	}
	if err != nil {
		now := p.now().UTC()
		if failErr := p.store.FailSettlementJob(ctx, job.ID, p.owner, err, now, now.Add(jobRetryDelay(job.Attempts))); failErr != nil {
			return fmt.Errorf("settlement %s failed: %v; recording failure: %w", job.ID, err, failErr)
		}
		return fmt.Errorf("settlement %s: %w", job.ID, err)
	}
	if err := p.store.CompleteSettlementJob(ctx, job.ID, p.owner, p.now().UTC()); err != nil {
		return fmt.Errorf("complete settlement %s: %w", job.ID, err)
	}
	return nil
}

func jobRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	shift := min(attempts-1, 6)
	delay := 5 * time.Second * time.Duration(1<<shift)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func (p *Processor) maintainLease(ctx context.Context, jobID string, cancel context.CancelFunc, lost chan<- error, done chan<- struct{}) {
	defer close(done)
	interval := p.leaseTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.store.RenewSettlementJob(ctx, jobID, p.owner, p.now().UTC(), p.leaseTTL); err != nil {
				select {
				case lost <- err:
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (p *Processor) maintainGateLease(ctx context.Context, jobID string, cancel context.CancelFunc, lost chan<- error, done chan<- struct{}) {
	defer close(done)
	interval := p.leaseTTL / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.store.RenewTradingGateJob(ctx, jobID, p.owner, p.now().UTC(), p.leaseTTL); err != nil {
				select {
				case lost <- err:
				default:
				}
				cancel()
				return
			}
		}
	}
}
