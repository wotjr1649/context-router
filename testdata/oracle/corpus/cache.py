"""In-process response cache for the Orbit gateway.

Implements a bounded LRU cache with per-entry TTL expiry and single-flight
coalescing. This mirrors the behavior documented in caching.md: LRU cache
eviction under capacity pressure, lazy cache TTL expiry on read, and explicit
invalidation driven by upstream write events.
"""

from __future__ import annotations

import threading
import time
from collections import OrderedDict
from dataclasses import dataclass
from typing import Callable, Optional


@dataclass
class Entry:
    value: bytes
    # Absolute expiry timestamp (monotonic seconds). An entry is expired once
    # time.monotonic() passes this value; TTL expiry is evaluated lazily.
    expires_at: float
    stored_at: float


class LRUCache:
    """A thread-safe bounded LRU cache with TTL.

    Capacity is a hard bound on the number of entries. When the cache is full a
    new write evicts the least recently used entry first. This is the LRU cache
    eviction path. Independently, each entry has a cache TTL; an entry that has
    outlived its TTL is dropped lazily the next time it is read.
    """

    def __init__(self, max_entries: int, default_ttl_seconds: float) -> None:
        if max_entries <= 0:
            raise ValueError("max_entries must be positive")
        self._max = max_entries
        self._default_ttl = default_ttl_seconds
        self._data: "OrderedDict[str, Entry]" = OrderedDict()
        self._lock = threading.Lock()
        self._locks: dict[str, threading.Lock] = {}
        self.hits = 0
        self.misses = 0
        self.evictions_capacity = 0
        self.evictions_ttl = 0

    def get(self, key: str) -> Optional[bytes]:
        """Return the cached value or None. Expired entries are evicted on read.

        This is the lazy cache TTL path: rather than sweeping the whole cache on
        a timer we check expiry when a key is actually read, which keeps the hot
        path cheap and lets cold entries fall out under LRU eviction instead.
        """
        with self._lock:
            entry = self._data.get(key)
            if entry is None:
                self.misses += 1
                return None
            if entry.expires_at <= time.monotonic():
                # TTL expiry: drop the stale entry and report a miss.
                del self._data[key]
                self.evictions_ttl += 1
                self.misses += 1
                return None
            # Move to most-recently-used position.
            self._data.move_to_end(key)
            self.hits += 1
            return entry.value

    def set(self, key: str, value: bytes, ttl_seconds: Optional[float] = None) -> None:
        """Insert or replace an entry, applying LRU cache eviction if full."""
        ttl = self._default_ttl if ttl_seconds is None else ttl_seconds
        now = time.monotonic()
        entry = Entry(value=value, expires_at=now + ttl, stored_at=now)
        with self._lock:
            if key in self._data:
                self._data.move_to_end(key)
            self._data[key] = entry
            # Capacity eviction: drop least-recently-used until within bound.
            while len(self._data) > self._max:
                self._data.popitem(last=False)
                self.evictions_capacity += 1

    def invalidate_prefix(self, prefix: str) -> int:
        """Explicitly evict every entry whose key starts with prefix.

        Called from the upstream write-event subscriber. Cache invalidation is
        best-effort: a dropped event just means the stale entry ages out under
        its TTL instead of being removed immediately.
        """
        with self._lock:
            victims = [k for k in self._data if k.startswith(prefix)]
            for k in victims:
                del self._data[k]
            return len(victims)

    def _entry_lock(self, key: str) -> threading.Lock:
        with self._lock:
            lk = self._locks.get(key)
            if lk is None:
                lk = threading.Lock()
                self._locks[key] = lk
            return lk

    def get_or_fill(self, key: str, fill: Callable[[], tuple[bytes, float]]) -> bytes:
        """Return the cached value, or fill it under single-flight coalescing.

        The first miss for a key takes the per-key lock and calls ``fill``;
        concurrent misses for the same key wait on that lock and then observe
        the freshly cached value instead of each calling the upstream. This is
        the cache stampede protection described in caching.md.
        """
        hit = self.get(key)
        if hit is not None:
            return hit
        lock = self._entry_lock(key)
        with lock:
            # Re-check under the lock — another caller may have filled it.
            hit = self.get(key)
            if hit is not None:
                return hit
            value, ttl = fill()
            self.set(key, value, ttl)
            return value

    def stats(self) -> dict[str, float]:
        """Snapshot for the Prometheus cache metrics exporter."""
        with self._lock:
            total = self.hits + self.misses
            hit_ratio = (self.hits / total) if total else 0.0
            return {
                "entries": len(self._data),
                "hit_ratio": hit_ratio,
                "evictions_capacity": self.evictions_capacity,
                "evictions_ttl": self.evictions_ttl,
            }
