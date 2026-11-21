// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package rig is the engine of Rig, the package manager of Norca.
//
// A host program builds an [Engine] with [New] and gives it, as options, the
// adapters it has: the cluster, the release API, the secret store, its
// storage and the decoder of job specifications (package host), and a
// fetcher of modules (package source). The rig program is the first host;
// the Norca server embeds the engine later through the same options
// (design, section 12).
//
// The engine keeps no global state, reads no environment variables and no
// files of its own, and reaches the network and the cluster only through
// the adapters it is given (design 12.4).
package rig

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/bacanoo-io/norca/rigger/host"
	"github.com/bacanoo-io/norca/rigger/oplog"
	"github.com/bacanoo-io/norca/rigger/source"
)

// ErrInvalidOption is matched (with errors.Is) by the error of [New] when an
// option is invalid.
var ErrInvalidOption = errors.New("rig: invalid option")

// Engine loads, evaluates, plans and applies modules and stacks. It is safe
// for concurrent use.
//
// An engine needs only the adapters of what it is asked to do: evaluating a
// module needs a job decoder, planning a stack needs the cluster and the
// release API as well.
type Engine struct {
	cluster  host.Cluster
	jobs     host.JobDecoder
	decoder  host.ResourceDecoder
	releases host.Releases
	secrets  host.Secrets
	store    host.Store
	fetcher  source.Fetcher
	clock    host.Clock
	logger   *slog.Logger
	limits   Limits
}

// Option configures an [Engine]. An option returns an error when its
// argument is invalid; [New] then fails.
type Option func(*Engine) error

// New returns an engine configured by the options.
//
// Without options the engine logs to the handler of slog.Default(), reads
// the time of the operating system and uses [DefaultLimits]. New returns an error matching
// [ErrInvalidOption] when an option is nil or invalid.
func New(opts ...Option) (*Engine, error) {
	e := &Engine{
		clock:  host.SystemClock{},
		logger: slog.New(oplog.NewHandler(slog.Default().Handler())),
		limits: DefaultLimits(),
	}

	for i, opt := range opts {
		if opt == nil {
			return nil, fmt.Errorf("%w: option %d is nil", ErrInvalidOption, i)
		}

		if err := opt(e); err != nil {
			return nil, err
		}
	}

	return e, nil
}

// WithCluster gives the engine the facts of the cluster.
func WithCluster(c host.Cluster) Option {
	return func(e *Engine) error {
		if c == nil {
			return invalid("cluster")
		}

		e.cluster = c

		return nil
	}
}

// WithJobDecoder gives the engine the decoder of job specifications.
func WithJobDecoder(d host.JobDecoder) Option {
	return func(e *Engine) error {
		if d == nil {
			return invalid("job decoder")
		}

		e.jobs = d

		return nil
	}
}

// WithResourceDecoder gives the engine the decoder of the kinds whose
// specification is a file format of the host (host.ResourceDecoder).
func WithResourceDecoder(d host.ResourceDecoder) Option {
	return func(e *Engine) error {
		if d == nil {
			return invalid("resource decoder")
		}

		e.decoder = d

		return nil
	}
}

// WithReleases gives the engine the release API of the cluster.
func WithReleases(r host.Releases) Option {
	return func(e *Engine) error {
		if r == nil {
			return invalid("releases")
		}

		e.releases = r

		return nil
	}
}

// WithSecrets gives the engine the secret store of the cluster.
func WithSecrets(s host.Secrets) Option {
	return func(e *Engine) error {
		if s == nil {
			return invalid("secrets")
		}

		e.secrets = s

		return nil
	}
}

// WithStore gives the engine the storage of the host.
func WithStore(s host.Store) Option {
	return func(e *Engine) error {
		if s == nil {
			return invalid("store")
		}

		e.store = s

		return nil
	}
}

// WithFetcher gives the engine the host's own fetcher of modules. The engine
// asks it first and uses its built-in sources for what it does not have.
func WithFetcher(f source.Fetcher) Option {
	return func(e *Engine) error {
		if f == nil {
			return invalid("fetcher")
		}

		e.fetcher = f

		return nil
	}
}

// WithClock replaces the clock of the operating system, for tests.
func WithClock(c host.Clock) Option {
	return func(e *Engine) error {
		if c == nil {
			return invalid("clock")
		}

		e.clock = c

		return nil
	}
}

// WithLogger sets the logger of the engine. The engine logs with constant
// messages and puts what varies into attributes; it never logs a sensitive
// value, and it returns errors instead of logging them.
//
// The engine wraps the handler of l with [oplog.NewHandler]: its log is a
// tree of operations, and every line carries the IDs of the operation it
// belongs to.
func WithLogger(l *slog.Logger) Option {
	return func(e *Engine) error {
		if l == nil {
			return invalid("logger")
		}

		e.logger = slog.New(oplog.NewHandler(l.Handler()))

		return nil
	}
}

// WithLimits replaces [DefaultLimits]. Every limit must be positive.
func WithLimits(l Limits) Option {
	return func(e *Engine) error {
		if err := l.validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidOption, err)
		}

		e.limits = l

		return nil
	}
}

func invalid(what string) error {
	return fmt.Errorf("%w: nil %s", ErrInvalidOption, what)
}
