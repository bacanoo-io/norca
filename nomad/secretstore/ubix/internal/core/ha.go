package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// haLockName is the one HA lock a vault's replicas contend for.
const haLockName = "active"

// ErrHANotConfigured is returned by [Core.RunHA] when HA is off.
var ErrHANotConfigured = errors.New("core: HA is not configured")

// HAConfig enables active/standby high availability (ADR D-021,
// docs/design/ha-active-standby.md): replicas sharing Backend all unseal, and
// the one holding its lock is active.
type HAConfig struct {
	Backend     storage.HABackend
	HolderID    string // unique per replica process
	Advertise   string // this replica's API URL, as clients reach it
	ClusterAddr string // this replica's cluster URL, where standbys forward to
	Lock        storage.LockOptions
	// StepDownHoldOff is how long a replica that stepped down on request waits
	// before contending for the lock again, so another replica takes over
	// instead of it winning straight back. Default 10s.
	StepDownHoldOff time.Duration
}

// DefaultStepDownHoldOff is used when HAConfig.StepDownHoldOff is zero.
const DefaultStepDownHoldOff = 10 * time.Second

// LeaderInfo describes the active replica.
type LeaderInfo struct {
	HolderID    string
	APIAddr     string
	ClusterAddr string
	IsSelf      bool
}

// haAdvert is what a replica writes into the lock's advertise field.
type haAdvert struct {
	API     string `json:"api"`
	Cluster string `json:"cluster,omitempty"`
}

// WithHA enables active/standby HA. The core's storage must be Backend (or a
// wrapper of it), so the lock fences the core's own writes.
func WithHA(cfg HAConfig) Option {
	return func(c *Core) { c.ha.cfg = cfg }
}

// ha is the core's active/standby state. Its zero value means HA is off.
type ha struct {
	cfg         HAConfig
	advert      string       // JSON haAdvert for the lock row
	lock        storage.Lock // this replica's handle on the HA lock
	active      atomic.Bool
	activeSince atomic.Int64 // unix nanos when this replica last became active
	stepDown    chan struct{}

	mu           sync.Mutex
	activeHooks  []func()
	standbyHooks []func()
}

func (h *ha) init() {
	if h.cfg.Backend == nil {
		return
	}
	if h.cfg.StepDownHoldOff <= 0 {
		h.cfg.StepDownHoldOff = DefaultStepDownHoldOff
	}
	adv, _ := json.Marshal(haAdvert{API: h.cfg.Advertise, Cluster: h.cfg.ClusterAddr})
	h.advert = string(adv)
	h.stepDown = make(chan struct{}, 1)
	h.lock = h.cfg.Backend.HALock(haLockName, h.cfg.HolderID, h.advert, h.cfg.Lock)
}

func (h *ha) enabled() bool { return h.lock != nil }

// acquireForInit takes the HA lock for the duration of an Initialize, through a
// handle of its own so the RunHA loop cannot mistake it for its acquisition.
func (h *ha) acquireForInit(ctx context.Context) (release func(), err error) {
	l := h.cfg.Backend.HALock(haLockName, h.cfg.HolderID+"/init", h.advert, h.cfg.Lock)
	if _, err := l.Acquire(ctx); err != nil {
		return nil, fmt.Errorf("core: acquire HA lock to initialize: %w", err)
	}
	return func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.Release(rctx)
	}, nil
}

// HAEnabled reports whether this core runs in active/standby mode.
func (c *Core) HAEnabled() bool { return c.ha.enabled() }

// Active reports whether this replica should serve requests: unsealed and, with
// HA, holding the lock. Without HA, unsealed is enough.
func (c *Core) Active() bool {
	if c.barrier.Sealed() {
		return false
	}
	return !c.ha.enabled() || c.ha.active.Load()
}

// Standby reports whether this replica is unsealed but not active (HA only).
func (c *Core) Standby() bool {
	return c.ha.enabled() && !c.barrier.Sealed() && !c.ha.active.Load()
}

// Leader reports the active replica. A zero LeaderInfo means none holds the
// lock right now (an election is in progress, or every replica is sealed).
func (c *Core) Leader(ctx context.Context) (LeaderInfo, error) {
	if !c.ha.enabled() {
		return LeaderInfo{}, ErrHANotConfigured
	}
	h, err := c.ha.lock.Holder(ctx)
	if err != nil || h.ID == "" {
		return LeaderInfo{}, err
	}
	info := LeaderInfo{HolderID: h.ID, IsSelf: h.ID == c.ha.cfg.HolderID}
	var adv haAdvert
	if json.Unmarshal([]byte(h.Advertise), &adv) == nil {
		info.APIAddr, info.ClusterAddr = adv.API, adv.Cluster
	} else {
		info.APIAddr = h.Advertise // a bare URL
	}
	return info, nil
}

// ActiveSince reports when this replica last became active, or the zero time
// if it is not active.
func (c *Core) ActiveSince() time.Time {
	if !c.Active() || !c.ha.enabled() {
		return time.Time{}
	}
	return time.Unix(0, c.ha.activeSince.Load()).UTC()
}

// ErrNotActive is returned by [Core.StepDown] on a replica that is not active.
var ErrNotActive = errors.New("core: this replica is not the active one")

// StepDown asks the active replica to give up the lock, so another replica
// takes over (sys/step-down). It returns once the request is queued; the
// replica then waits StepDownHoldOff before contending again.
func (c *Core) StepDown() error {
	if !c.ha.enabled() {
		return ErrHANotConfigured
	}
	if !c.ha.active.Load() {
		return ErrNotActive
	}
	select {
	case c.ha.stepDown <- struct{}{}:
	default: // one already pending
	}
	return nil
}

// OnActive registers fn to run each time this replica becomes active, before
// it serves requests as active.
func (c *Core) OnActive(fn func()) {
	c.ha.mu.Lock()
	defer c.ha.mu.Unlock()
	c.ha.activeHooks = append(c.ha.activeHooks, fn)
}

// OnStandby registers fn to run each time this replica stops being active
// (lost the lock, sealed, or shutting down), before the lock is released.
func (c *Core) OnStandby(fn func()) {
	c.ha.mu.Lock()
	defer c.ha.mu.Unlock()
	c.ha.standbyHooks = append(c.ha.standbyHooks, fn)
}

func (c *Core) runHooks(active bool) {
	c.ha.mu.Lock()
	hooks := slices.Clone(c.ha.standbyHooks)
	if active {
		hooks = slices.Clone(c.ha.activeHooks)
	}
	c.ha.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// notifyLocked wakes everyone waiting on stateChanged. The caller holds c.mu.
func (c *Core) notifyLocked() {
	close(c.stateCh)
	c.stateCh = make(chan struct{})
}

// stateChanged returns a channel closed at the next seal or unseal.
func (c *Core) stateChanged() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stateCh
}

// waitUnsealed blocks until the barrier is unsealed or ctx ends.
func (c *Core) waitUnsealed(ctx context.Context) error {
	for {
		changed := c.stateChanged()
		if !c.barrier.Sealed() {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RunHA runs this replica's side of the election until ctx ends: whenever the
// vault is unsealed it waits for the HA lock, becomes active on getting it, and
// steps down — releasing the lock so a standby takes over at once — when the
// lock is lost, the vault is sealed, or ctx ends. logf reports transitions.
// It returns nil when ctx ends, after stepping down.
func (c *Core) RunHA(ctx context.Context, logf func(format string, args ...any)) error {
	if !c.ha.enabled() {
		return ErrHANotConfigured
	}
	for {
		if err := c.waitUnsealed(ctx); err != nil {
			return nil
		}
		changed := c.stateChanged()
		if c.barrier.Sealed() {
			continue
		}

		// Wait for the lock, giving up if the vault is sealed meanwhile.
		acqCtx, cancel := context.WithCancel(ctx)
		go func(sealed <-chan struct{}) {
			select {
			case <-sealed:
				cancel()
			case <-acqCtx.Done():
			}
		}(changed)
		lost, err := c.ha.lock.Acquire(acqCtx)
		if err != nil {
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if !errors.Is(err, context.Canceled) {
				logf("WARNING: HA lock: %v", err)
				if ctxSleep(ctx, time.Second) != nil {
					return nil
				}
			}
			continue
		}

		changed = c.stateChanged()
		if c.barrier.Sealed() { // sealed while acquiring
			c.releaseHA(logf)
			cancel()
			continue
		}
		// Drop a step-down request left over from an earlier term, before any
		// new one can be accepted (StepDown requires active).
		select {
		case <-c.ha.stepDown:
		default:
		}
		// Hooks first: they prepare what an active replica needs (fresh caches,
		// the cluster CA) before it answers anything as active.
		c.runHooks(true)
		c.ha.activeSince.Store(time.Now().UnixNano())
		c.ha.active.Store(true)
		logf("HA: active (%s)", c.ha.cfg.HolderID)

		var reason string
		holdOff := false
		select {
		case <-lost:
			reason = "lost the HA lock"
		case <-changed:
			reason = "sealed"
		case <-ctx.Done():
			reason = "shutting down"
		case <-c.ha.stepDown:
			reason = "stepped down on request"
			holdOff = true
		}
		c.ha.active.Store(false)
		c.runHooks(false)
		logf("HA: standby (%s)", reason)
		c.releaseHA(logf)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if holdOff && c.holdOff(ctx) != nil {
			return nil
		}
	}
}

// holdOff keeps a replica that stepped down on request out of the election for
// StepDownHoldOff, so another replica takes over — but only while another can:
// if the lock stays free for two retry intervals (the others had their chance,
// or have gone away since), it ends early, since waiting any longer would leave
// the vault with no active replica at all.
func (c *Core) holdOff(ctx context.Context) error {
	retry := c.ha.cfg.Lock.RetryInterval
	if retry <= 0 {
		retry = storage.DefaultLockRetryInterval
	}
	grace := 2 * retry
	deadline := time.Now().Add(c.ha.cfg.StepDownHoldOff)
	var freeSince time.Time
	for time.Now().Before(deadline) {
		if err := ctxSleep(ctx, retry/2); err != nil {
			return err
		}
		h, err := c.ha.lock.Holder(ctx)
		switch {
		case err != nil || h.ID != "":
			freeSince = time.Time{}
		case freeSince.IsZero():
			freeSince = time.Now()
		case time.Since(freeSince) >= grace:
			return nil
		}
	}
	return nil
}

func (c *Core) releaseHA(logf func(string, ...any)) {
	rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ha.lock.Release(rctx); err != nil {
		logf("WARNING: HA lock release: %v (a standby takes over when the lease expires)", err)
	}
}

func ctxSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
