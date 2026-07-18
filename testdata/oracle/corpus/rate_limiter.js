// Sliding window rate limiter for the Orbit gateway.
//
// Implements the per-client rate limiter described in architecture.md: a
// sliding window counter that smooths the burst a fixed window would allow at
// the window boundary. A token-bucket limiter is provided as an alternative
// strategy for endpoints that prefer steady-drip admission.

'use strict';

/**
 * SlidingWindowLimiter admits requests using a weighted sliding window.
 *
 * Rather than counting requests in a hard fixed window (which lets a client
 * send `limit` requests at the end of one window and `limit` again at the
 * start of the next), the sliding window blends the previous window's count by
 * how much of it still overlaps the current instant. This is the rate limiter
 * that fronts every protected route.
 */
class SlidingWindowLimiter {
  /**
   * @param {number} limit    max requests per window
   * @param {number} windowMs window length in milliseconds
   * @param {() => number} [now] injectable clock for tests
   */
  constructor(limit, windowMs, now = Date.now) {
    if (limit <= 0) throw new RangeError('limit must be positive');
    if (windowMs <= 0) throw new RangeError('windowMs must be positive');
    this.limit = limit;
    this.windowMs = windowMs;
    this.now = now;
    /** @type {Map<string, {windowStart: number, count: number, prevCount: number}>} */
    this.clients = new Map();
  }

  /**
   * Decide whether a client may proceed. Returns the rate limit decision plus
   * the header values the API reference documents (limit, remaining, reset).
   *
   * @param {string} clientId identity extracted from the JWT token
   * @returns {{allowed: boolean, remaining: number, resetMs: number, retryAfterMs: number}}
   */
  check(clientId) {
    const t = this.now();
    let state = this.clients.get(clientId);
    if (!state) {
      state = { windowStart: t, count: 0, prevCount: 0 };
      this.clients.set(clientId, state);
    }

    // Roll the window forward if we've crossed one or more boundaries.
    const elapsed = t - state.windowStart;
    if (elapsed >= this.windowMs) {
      const windowsPassed = Math.floor(elapsed / this.windowMs);
      state.prevCount = windowsPassed === 1 ? state.count : 0;
      state.count = 0;
      state.windowStart += windowsPassed * this.windowMs;
    }

    // Weighted count: fraction of the previous window still overlapping now,
    // plus the current window's count. This is the sliding window estimate.
    const intoWindow = t - state.windowStart;
    const prevWeight = (this.windowMs - intoWindow) / this.windowMs;
    const estimated = state.prevCount * prevWeight + state.count;

    const resetMs = state.windowStart + this.windowMs - t;
    if (estimated >= this.limit) {
      return { allowed: false, remaining: 0, resetMs, retryAfterMs: resetMs };
    }
    state.count += 1;
    const remaining = Math.max(0, Math.floor(this.limit - estimated - 1));
    return { allowed: true, remaining, resetMs, retryAfterMs: 0 };
  }

  /** Drop idle client state so the map does not grow without bound. */
  sweep(maxIdleMs) {
    const t = this.now();
    for (const [id, state] of this.clients) {
      if (t - state.windowStart > maxIdleMs) this.clients.delete(id);
    }
  }
}

/**
 * TokenBucketLimiter is an alternative rate limiter that refills tokens at a
 * steady rate. It admits bursts up to the bucket capacity and then throttles
 * to the refill rate. Some upstreams prefer this steady drip over the sliding
 * window's sharper cutoff.
 */
class TokenBucketLimiter {
  constructor(capacity, refillPerSec, now = Date.now) {
    this.capacity = capacity;
    this.refillPerSec = refillPerSec;
    this.now = now;
    this.buckets = new Map();
  }

  check(clientId) {
    const t = this.now();
    let b = this.buckets.get(clientId);
    if (!b) {
      b = { tokens: this.capacity, last: t };
      this.buckets.set(clientId, b);
    }
    const refill = ((t - b.last) / 1000) * this.refillPerSec;
    b.tokens = Math.min(this.capacity, b.tokens + refill);
    b.last = t;
    if (b.tokens < 1) {
      const retryAfterMs = ((1 - b.tokens) / this.refillPerSec) * 1000;
      return { allowed: false, remaining: 0, retryAfterMs };
    }
    b.tokens -= 1;
    return { allowed: true, remaining: Math.floor(b.tokens), retryAfterMs: 0 };
  }
}

/**
 * Express-style middleware wrapping a limiter. On rejection it sets the
 * documented rate limit headers and returns 429 with Retry-After.
 */
function rateLimitMiddleware(limiter, clientIdOf) {
  return function (req, res, next) {
    const decision = limiter.check(clientIdOf(req));
    res.setHeader('X-RateLimit-Limit', String(limiter.limit ?? limiter.capacity));
    res.setHeader('X-RateLimit-Remaining', String(decision.remaining));
    if (!decision.allowed) {
      res.setHeader('Retry-After', String(Math.ceil(decision.retryAfterMs / 1000)));
      res.status(429).json({ error: { code: 'rate_limited', message: 'quota exceeded' } });
      return;
    }
    next();
  };
}

module.exports = { SlidingWindowLimiter, TokenBucketLimiter, rateLimitMiddleware };
