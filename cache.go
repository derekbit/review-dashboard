package main

import (
	"context"
	"sync"
	"time"
)

type dashboardSnapshot struct {
	GeneratedAt  time.Time
	Repositories []repositoryView
	Warnings     []string
	Error        string
}

type dashboardCache struct {
	org            string
	github         *githubClient
	ttl            time.Duration
	refreshTimeout time.Duration

	mu       sync.RWMutex
	snapshot dashboardSnapshot
	hasData  bool
	inflight chan struct{}
}

func newDashboardCache(org string, github *githubClient, ttl, refreshTimeout time.Duration) *dashboardCache {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	if refreshTimeout <= 0 {
		refreshTimeout = 90 * time.Second
	}

	return &dashboardCache{
		org:            org,
		github:         github,
		ttl:            ttl,
		refreshTimeout: refreshTimeout,
	}
}

func (c *dashboardCache) start(ctx context.Context) {
	go func() {
		c.refresh(context.Background())

		ticker := time.NewTicker(c.ttl)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.refresh(context.Background())
			}
		}
	}()
}

func (c *dashboardCache) get(ctx context.Context) dashboardSnapshot {
	c.mu.Lock()
	if c.hasData && time.Since(c.snapshot.GeneratedAt) < c.ttl {
		snapshot := c.snapshot
		c.mu.Unlock()
		return snapshot
	}

	if c.inflight != nil {
		wait := c.inflight
		snapshot := c.snapshot
		hasData := c.hasData
		c.mu.Unlock()

		if hasData {
			return snapshot
		}

		select {
		case <-ctx.Done():
			return dashboardSnapshot{
				GeneratedAt: time.Now(),
				Error:       ctx.Err().Error(),
			}
		case <-wait:
			c.mu.RLock()
			snapshot = c.snapshot
			c.mu.RUnlock()
			return snapshot
		}
	}

	wait := make(chan struct{})
	c.inflight = wait
	snapshot := c.snapshot
	hasData := c.hasData
	c.mu.Unlock()

	if hasData {
		go c.refreshWithSignal(context.Background(), wait)
		return snapshot
	}

	return c.refreshWithSignal(ctx, wait)
}

func (c *dashboardCache) refresh(ctx context.Context) dashboardSnapshot {
	c.mu.Lock()
	if c.inflight != nil {
		wait := c.inflight
		snapshot := c.snapshot
		hasData := c.hasData
		c.mu.Unlock()
		if hasData {
			return snapshot
		}

		select {
		case <-ctx.Done():
			return dashboardSnapshot{
				GeneratedAt: time.Now(),
				Error:       ctx.Err().Error(),
			}
		case <-wait:
			c.mu.RLock()
			snapshot = c.snapshot
			c.mu.RUnlock()
			return snapshot
		}
	}

	wait := make(chan struct{})
	c.inflight = wait
	c.mu.Unlock()
	return c.refreshWithSignal(ctx, wait)
}

func (c *dashboardCache) refreshWithSignal(parent context.Context, wait chan struct{}) dashboardSnapshot {
	ctx, cancel := context.WithTimeout(parent, c.refreshTimeout)
	defer cancel()

	repositories, warnings, err := c.github.LoadDashboard(ctx, c.org)
	snapshot := dashboardSnapshot{
		GeneratedAt:  time.Now(),
		Repositories: repositories,
		Warnings:     warnings,
	}
	if err != nil {
		snapshot.Error = err.Error()
	}

	c.mu.Lock()
	if len(snapshot.Repositories) > 0 || len(snapshot.Warnings) > 0 || snapshot.Error == "" || !c.hasData {
		c.snapshot = snapshot
		c.hasData = true
	} else {
		snapshot = c.snapshot
	}
	closed := c.inflight
	c.inflight = nil
	c.mu.Unlock()

	if closed != nil {
		close(closed)
	}
	return snapshot
}
