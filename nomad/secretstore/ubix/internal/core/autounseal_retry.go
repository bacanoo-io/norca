package core

import (
	"context"
	"errors"
	"time"
)

// RunAutoUnseal calls [Core.AutoUnseal] until the barrier is unsealed, waiting
// minWait after the first failure and doubling up to maxWait after each further
// one. A KMS, transit vault, or storage backend that is briefly unreachable at
// startup therefore delays the unseal instead of leaving the process sealed until
// something restarts it — every restart costs an unseal, and a replica that stays
// sealed while looking alive is worse than one that is down (ADR D-019, D-021).
//
// It returns nil once the barrier is unsealed, including when an operator
// initializes an auto-unseal vault while it waits (Initialize leaves it
// unsealed). It returns ctx.Err() if ctx ends first, and returns immediately
// with [ErrAutoUnsealNotConfigured] or [ErrNotAutoUnseal], which no amount of
// retrying can fix. Every other error, including [ErrNotInitialized], is retried.
//
// onFailure, if non-nil, is called after each failed attempt with the error and
// the wait before the next attempt. Once unsealed, RunAutoUnseal does not watch
// for a later seal: an operator's sys/seal is left in place, as in Vault.
func (c *Core) RunAutoUnseal(ctx context.Context, minWait, maxWait time.Duration, onFailure func(err error, next time.Duration)) error {
	wait := minWait
	for {
		err := c.AutoUnseal(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrAutoUnsealNotConfigured) || errors.Is(err, ErrNotAutoUnseal) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if onFailure != nil {
			onFailure(err, wait)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait = min(wait*2, maxWait)
	}
}
