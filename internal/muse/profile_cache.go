package muse

import (
	"container/list"
	"time"
)

type cacheEntry[V any] struct {
	key     [32]byte
	value   V
	expires time.Time
}

// boundedCache is protected by profileEngine.mu; expiry follows last access order.
type boundedCache[V any] struct {
	entries  map[[32]byte]*list.Element
	order    *list.List
	capacity int
	ttl      time.Duration
}

func newCache[V any](capacity int, ttl time.Duration) *boundedCache[V] {
	return &boundedCache[V]{entries: make(map[[32]byte]*list.Element), order: list.New(), capacity: capacity, ttl: ttl}
}

func (c *boundedCache[V]) get(key [32]byte, now time.Time) (V, bool) {
	if element, found := c.entries[key]; found {
		entry := element.Value.(*cacheEntry[V])
		if now.Before(entry.expires) {
			entry.expires = now.Add(c.ttl)
			c.order.MoveToFront(element)
			return entry.value, true
		}
		c.remove(element)
	}
	var empty V
	return empty, false
}

func (c *boundedCache[V]) put(key [32]byte, value V, now time.Time) {
	if element := c.entries[key]; element != nil {
		c.remove(element)
	}
	for element := c.order.Back(); element != nil; element = c.order.Back() {
		if now.Before(element.Value.(*cacheEntry[V]).expires) {
			break
		}
		c.remove(element)
	}
	if c.capacity <= 0 {
		return
	}
	for c.order.Len() >= c.capacity {
		c.remove(c.order.Back())
	}
	entry := &cacheEntry[V]{key: key, value: value, expires: now.Add(c.ttl)}
	c.entries[key] = c.order.PushFront(entry)
}

func (c *boundedCache[V]) remove(element *list.Element) {
	delete(c.entries, element.Value.(*cacheEntry[V]).key)
	c.order.Remove(element)
}
