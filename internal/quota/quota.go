// Package quota provides deterministic admission for provider-use windows.
// It deliberately tracks abstract provider units, not USD: money accounting
// remains in store.AddCost while this package prevents avoidable rate limits.
package quota

import (
	"fmt"
	"time"

	"github.com/djbu/corral/internal/clock"
)

type Class string

const (
	Interactive Class = "interactive"
	Headless    Class = "headless"
)

type Config struct {
	Window             time.Duration
	Limit              int
	InteractiveReserve int
}

// Decision describes an admission result without exposing provider details.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
	Reason     string
}

type entry struct {
	at    time.Time
	class Class
	units int
}

type Controller struct {
	clk         clock.Clock
	cfg         Config
	entries     []entry
	pausedUntil time.Time
}

func New(clk clock.Clock, cfg Config) (*Controller, error) {
	if cfg.Window <= 0 {
		return nil, fmt.Errorf("quota: window must be positive")
	}
	if cfg.Limit <= 0 {
		return nil, fmt.Errorf("quota: limit must be positive")
	}
	if cfg.InteractiveReserve < 0 || cfg.InteractiveReserve >= cfg.Limit {
		return nil, fmt.Errorf("quota: interactive reserve must be >= 0 and less than limit")
	}
	return &Controller{clk: clk, cfg: cfg}, nil
}

func (c *Controller) prune(now time.Time) {
	cutoff := now.Add(-c.cfg.Window)
	i := 0
	for _, e := range c.entries {
		if e.at.After(cutoff) {
			c.entries[i] = e
			i++
		}
	}
	c.entries = c.entries[:i]
}

func (c *Controller) usage(class Class) int {
	total := 0
	for _, e := range c.entries {
		if class == "" || e.class == class {
			total += e.units
		}
	}
	return total
}

// Admit atomically checks and records units. Headless work cannot consume the
// configured interactive reserve; interactive work may use any remaining
// capacity. Calls are serialized by the orchestrator's single scheduling loop.
func (c *Controller) Admit(class Class, units int) Decision {
	now := c.clk.Now()
	c.prune(now)
	if units <= 0 {
		return Decision{Reason: "units must be positive"}
	}
	if class != Interactive && class != Headless {
		return Decision{Reason: "unknown class"}
	}
	if now.Before(c.pausedUntil) {
		return Decision{Reason: "provider rate limit pause", RetryAfter: c.pausedUntil.Sub(now)}
	}
	limit := c.cfg.Limit
	if class == Headless {
		limit -= c.cfg.InteractiveReserve
	}
	if c.usage("")+units > limit {
		return Decision{Reason: "quota window exhausted", RetryAfter: c.nextReset(now)}
	}
	c.entries = append(c.entries, entry{at: now, class: class, units: units})
	return Decision{Allowed: true}
}

// PauseUntil records a provider-provided retry boundary. It only extends an
// existing pause, so an out-of-order weaker signal cannot resume work early.
func (c *Controller) PauseUntil(until time.Time) {
	if until.After(c.pausedUntil) {
		c.pausedUntil = until
	}
}

func (c *Controller) nextReset(now time.Time) time.Duration {
	if len(c.entries) == 0 {
		return 0
	}
	earliest := c.entries[0].at
	for _, e := range c.entries[1:] {
		if e.at.Before(earliest) {
			earliest = e.at
		}
	}
	d := earliest.Add(c.cfg.Window).Sub(now)
	if d < 0 {
		return 0
	}
	return d
}
