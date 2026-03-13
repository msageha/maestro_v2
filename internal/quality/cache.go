package quality

import (
	"container/list"
	"sync"
	"time"
)

// resultCache is a thread-safe LRU cache for evaluation results
type resultCache struct {
	mu       sync.RWMutex
	items    map[string]*list.Element
	lru      *list.List
	maxSize  int
	ttl      time.Duration
}

// cacheItem represents an item in the cache
type cacheItem struct {
	key       string
	value     *EvaluationResult
	expiresAt time.Time
}

// newResultCache creates a new result cache
func newResultCache(maxSize int, ttl time.Duration) *resultCache {
	return &resultCache{
		items:   make(map[string]*list.Element),
		lru:     list.New(),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// Get retrieves a value from the cache
func (c *resultCache) Get(key *cacheKey) *EvaluationResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	keyStr := c.keyToString(key)
	elem, exists := c.items[keyStr]
	if !exists {
		return nil
	}

	item := elem.Value.(*cacheItem)

	// Check if expired and remove stale entry
	if time.Now().After(item.expiresAt) {
		c.removeElement(elem)
		return nil
	}

	// Move to front (most recently used)
	c.lru.MoveToFront(elem)

	// Return a copy to prevent modification
	result := *item.value
	return &result
}

// Set stores a value in the cache
func (c *resultCache) Set(key *cacheKey, value *EvaluationResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	keyStr := c.keyToString(key)

	// Check if already exists
	if elem, exists := c.items[keyStr]; exists {
		// Update existing item
		c.lru.MoveToFront(elem)
		item := elem.Value.(*cacheItem)
		item.value = value
		item.expiresAt = time.Now().Add(c.ttl)
		return
	}

	// Add new item
	item := &cacheItem{
		key:       keyStr,
		value:     value,
		expiresAt: time.Now().Add(c.ttl),
	}

	elem := c.lru.PushFront(item)
	c.items[keyStr] = elem

	// Evict oldest if over capacity
	if c.lru.Len() > c.maxSize {
		c.evictOldest()
	}

	// Clean up expired items periodically
	if c.lru.Len()%100 == 0 {
		c.cleanExpired()
	}
}

// Clear removes all items from the cache
func (c *resultCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.lru = list.New()
}

// evictOldest removes the least recently used item
func (c *resultCache) evictOldest() {
	elem := c.lru.Back()
	if elem != nil {
		c.removeElement(elem)
	}
}

// removeElement removes an element from the cache
func (c *resultCache) removeElement(elem *list.Element) {
	c.lru.Remove(elem)
	item := elem.Value.(*cacheItem)
	delete(c.items, item.key)
}

// cleanExpired removes expired items from the cache
func (c *resultCache) cleanExpired() {
	now := time.Now()
	for elem := c.lru.Back(); elem != nil; {
		prev := elem.Prev()
		item := elem.Value.(*cacheItem)
		if now.After(item.expiresAt) {
			c.removeElement(elem)
		}
		elem = prev
	}
}

// keyToString converts a cache key to a string
func (c *resultCache) keyToString(key *cacheKey) string {
	return key.GateID + ":" + key.GateVersionHash + ":" + key.ContextFingerprint
}

// Size returns the current number of items in the cache
func (c *resultCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lru.Len()
}

// Stats returns cache statistics
func (c *resultCache) Stats() cacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := cacheStats{
		Size:    c.lru.Len(),
		MaxSize: c.maxSize,
	}

	// Count expired items
	now := time.Now()
	for elem := c.lru.Front(); elem != nil; elem = elem.Next() {
		item := elem.Value.(*cacheItem)
		if now.After(item.expiresAt) {
			stats.Expired++
		}
	}

	return stats
}

// cacheStats represents cache statistics
type cacheStats struct {
	Size    int
	MaxSize int
	Expired int
}