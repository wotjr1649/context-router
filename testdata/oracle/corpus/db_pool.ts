// Connection pool for upstream database and service handles.
//
// Implements the pooling layer described in architecture.md and deployment.md:
// a bounded connection pool with an acquire timeout, a periodic health check on
// idle connections, and a graceful shutdown drain that waits for borrowed
// connections to be returned before closing.

export interface Conn {
  id: number;
  isAlive(): Promise<boolean>;
  close(): Promise<void>;
}

export interface PoolOptions {
  /** Hard cap on live connections; the dominant tuning knob under load. */
  max: number;
  /** How long acquire() waits for a free connection before failing. */
  acquireTimeoutMs: number;
  /** Interval between health check sweeps of idle connections. */
  healthCheckIntervalMs: number;
  /** Factory that opens a fresh upstream connection. */
  open: () => Promise<Conn>;
}

class Deferred<T> {
  promise: Promise<T>;
  resolve!: (v: T) => void;
  reject!: (e: unknown) => void;
  constructor() {
    this.promise = new Promise((res, rej) => {
      this.resolve = res;
      this.reject = rej;
    });
  }
}

/**
 * ConnectionPool hands out live upstream connections and recycles them. It
 * bounds concurrency at `max`, enforces an acquire timeout so a saturated pool
 * surfaces as a fast error rather than a hang, and runs a health check so dead
 * sockets are discarded before a caller ever sees them.
 */
export class ConnectionPool {
  private idle: Conn[] = [];
  private inUse = new Set<Conn>();
  private waiters: Array<{ d: Deferred<Conn>; timer: NodeJS.Timeout }> = [];
  private opening = 0;
  private closed = false;
  private healthTimer?: NodeJS.Timeout;

  constructor(private readonly opts: PoolOptions) {
    this.healthTimer = setInterval(
      () => void this.runHealthCheck(),
      opts.healthCheckIntervalMs,
    );
  }

  private get total(): number {
    return this.idle.length + this.inUse.size + this.opening;
  }

  /**
   * Borrow a connection. Resolves with an idle connection immediately when one
   * is available, opens a new one if under `max`, or waits until a connection
   * is released. Rejects after `acquireTimeoutMs` when the connection pool is
   * exhausted — the caller should map that to a 503.
   */
  async acquire(): Promise<Conn> {
    if (this.closed) throw new Error('pool: acquire after shutdown');

    const existing = this.idle.pop();
    if (existing) {
      this.inUse.add(existing);
      return existing;
    }

    if (this.total < this.opts.max) {
      this.opening++;
      try {
        const conn = await this.opts.open();
        this.inUse.add(conn);
        return conn;
      } finally {
        this.opening--;
      }
    }

    // Pool is at capacity — wait for a release or time out.
    const d = new Deferred<Conn>();
    const timer = setTimeout(() => {
      this.waiters = this.waiters.filter((w) => w.d !== d);
      d.reject(new Error('pool: acquire timeout — connection pool exhausted'));
    }, this.opts.acquireTimeoutMs);
    this.waiters.push({ d, timer });
    return d.promise;
  }

  /** Return a borrowed connection. Hands it to the next waiter if any. */
  release(conn: Conn): void {
    if (!this.inUse.delete(conn)) return;
    const next = this.waiters.shift();
    if (next) {
      clearTimeout(next.timer);
      this.inUse.add(conn);
      next.d.resolve(conn);
      return;
    }
    if (this.closed) {
      void conn.close();
      return;
    }
    this.idle.push(conn);
  }

  /**
   * Health check: probe every idle connection and drop the dead ones so a
   * caller never borrows a closed socket. Runs on a timer; in-use connections
   * are left alone since their owner will observe any failure directly.
   */
  private async runHealthCheck(): Promise<void> {
    const survivors: Conn[] = [];
    for (const conn of this.idle) {
      let alive = false;
      try {
        alive = await conn.isAlive();
      } catch {
        alive = false;
      }
      if (alive) survivors.push(conn);
      else await conn.close().catch(() => {});
    }
    this.idle = survivors;
  }

  /**
   * Graceful shutdown: stop the health check timer, reject any pending waiters,
   * and drain. Idle connections close now; in-use connections close as they
   * are released. Resolves once every borrowed connection has been returned,
   * bounding the deployment's shutdown_grace_seconds drain.
   */
  async shutdown(): Promise<void> {
    this.closed = true;
    if (this.healthTimer) clearInterval(this.healthTimer);
    for (const w of this.waiters) {
      clearTimeout(w.timer);
      w.d.reject(new Error('pool: shutting down'));
    }
    this.waiters = [];
    await Promise.all(this.idle.map((c) => c.close().catch(() => {})));
    this.idle = [];
    // Spin until in-use connections are released by their owners.
    while (this.inUse.size > 0) {
      await new Promise((r) => setTimeout(r, 25));
    }
  }

  /** Snapshot for the pool_connections_* Prometheus metrics. */
  stats(): { inUse: number; idle: number; waiters: number } {
    return { inUse: this.inUse.size, idle: this.idle.length, waiters: this.waiters.length };
  }
}
