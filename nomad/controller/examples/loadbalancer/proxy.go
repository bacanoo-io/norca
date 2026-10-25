// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// proxy is the TCP load balancer run by the "nomad" provider. It forwards
// every connection to the next target (round robin), trying the following
// targets if dialing fails. The targets are read from a file with one
// "host:port" per line, rendered by a template of the proxy job; the file is
// re-read when it changes.
type proxy struct {
	targetsFile string
	logger      *slog.Logger
	dialTimeout time.Duration

	mu      sync.RWMutex
	targets []string
	content []byte
	next    atomic.Uint64
}

func (p *proxy) loadTargets() {
	b, err := os.ReadFile(p.targetsFile)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.logger.Warn("reading targets failed", "error", err)
		}
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if bytes.Equal(b, p.content) && p.content != nil {
		return
	}
	var targets []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			targets = append(targets, line)
		}
	}
	p.content, p.targets = b, targets
	p.logger.Info("targets updated", "targets", strings.Join(targets, ","))
}

func (p *proxy) watchTargets(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		p.loadTargets()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *proxy) pick() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.targets)
	if n == 0 {
		return nil
	}
	start := int(p.next.Add(1) % uint64(n))
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, p.targets[(start+i)%n])
	}
	return out
}

func (p *proxy) handle(conn net.Conn) {
	defer conn.Close()
	var upstream net.Conn
	for _, target := range p.pick() {
		c, err := net.DialTimeout("tcp", target, p.dialTimeout)
		if err == nil {
			upstream = c
			break
		}
		p.logger.Debug("dialing target failed", "target", target, "error", err)
	}
	if upstream == nil {
		p.logger.Warn("no reachable target", "client", conn.RemoteAddr().String())
		return
	}
	defer upstream.Close()

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(upstream, conn)
	go pipe(conn, upstream)
	<-done
	<-done
}

// serve accepts connections until ctx is canceled.
func (p *proxy) serve(ctx context.Context, ln net.Listener) error {
	go p.watchTargets(ctx)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go p.handle(conn)
	}
}
