package quality

import (
	"container/list"
	"sync"
	"time"
)

// ResultCache is a thread-safe LRU cache for evaluation results
type ResultCache struct {
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

// NewResultCache creates a new result cache
func NewResultCache(maxSize int, ttl time.Duration) *ResultCache {
	return &ResultCache{
		items:   make(map[string]*list.Element),
		lru:     list.New(),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// Get retrieves a value from the cache
func (c *ResultCache) Get(key *CacheKey) *EvaluationResult {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keyStr := c.keyToString(key)
	elem, exists := c.items[keyStr]
	if !exists {
		return nil
	}

	item := elem.Value.(*cacheItem)

	// Check if expired
	if time.Now().After(item.expiresAt) {
		// Item is expired, but we don't remove it here (would need write lock)
		return nil
	}

	// Move to front (most recently used)
	// This requires a write lock, so we skip it in Get for performance
	// The LRU order will be updated on Set

	// Return a copy to prevent modification
	result := *item.value
	return &result
}

// Set stores a value in the cache
func (c *ResultCache) Set(key *CacheKey, value *EvaluationResult) {
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
func (c *ResultCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.items = make(map[string]*list.Element)
	c.lru = list.New()
}

// evictOldest removes the least recently used item
func (c *ResultCache) evictOldest() {
	elem := c.lru.Back()
	if elem != nil {
		c.removeElement(elem)
	}
}

// removeElement removes an element from the cache
func (c *ResultCache) removeElement(elem *list.Element) {
	c.lru.Remove(elem)
	item := elem.Value.(*cacheItem)
	delete(c.items, item.key)
}

// cleanExpired removes expired items from the cache
func (c *ResultCache) cleanExpired() {
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
func (c *ResultCache) keyToString(key *CacheKey) string {
	return key.GateID + ":" + key.GateVersionHash + ":" + key.ContextFingerprint
}

// Size returns the current number of items in the cache
func (c *ResultCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lru.Len()
}

// Stats returns cache statistics
func (c *ResultCache) Stats() CacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	stats := CacheStats{
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

// CacheStats represents cache statistics
type CacheStats struct {
	Size    int
	MaxSize int
	Expired int
}