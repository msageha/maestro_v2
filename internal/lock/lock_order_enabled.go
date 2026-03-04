//go:build lockorder

package lock

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// lockOrderMode controls the behaviour of the debug order checker.
type lockOrderMode uint8

const (
	lockOrderOff   lockOrderMode = iota // tracking disabled
	lockOrderWarn                       // log violations
	lockOrderPanic                      // panic on violation (for tests)
)

// heldLock records a single lock acquisition for a goroutine.
type heldLock struct {
	key   string
	level int
}

// debugOrderChecker tracks per-goroutine lock acquisitions and verifies
// that locks are acquired in the canonical order:
//
//	queue:* (level 1) → state:* (level 2) → result:* (level 3)
//
// It is compiled in only when the "lockorder" build tag is set.
// The runtime behaviour is controlled by the MAESTRO_LOCKORDER environment
// variable: off (default) | warn | panic.
type debugOrderChecker struct {
	mu   sync.Mutex
	held map[uint64][]heldLock // goroutine ID → currently held locks
	mode lockOrderMode
}

func newOrderChecker() orderChecker {
	return &debugOrderChecker{
		held: make(map[uint64][]heldLock),
		mode: parseLockOrderMode(os.Getenv("MAESTRO_LOCKORDER")),
	}
}

func parseLockOrderMode(v string) lockOrderMode {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "warn", "1", "true":
		return lockOrderWarn
	case "panic", "strict", "2":
		return lockOrderPanic
	default:
		return lockOrderOff
	}
}

// LevelForKey returns the canonical lock level for a key prefix.
// Keys that do not match a known prefix return (0, false) and are
// excluded from order checking.
func LevelForKey(key string) (int, bool) {
	switch {
	case strings.HasPrefix(key, "queue:"):
		return 1, true
	case strings.HasPrefix(key, "state:"):
		return 2, true
	case strings.HasPrefix(key, "result:"):
		return 3, true
	default:
		return 0, false
	}
}

func (c *debugOrderChecker) BeforeLock(key string) {
	if c.mode == lockOrderOff {
		return
	}
	level, ok := LevelForKey(key)
	if !ok {
		return
	}
	gid := currentGID()

	c.mu.Lock()
	held := c.held[gid]
	maxLevel, maxKey := 0, ""
	for _, h := range held {
		if h.level > maxLevel {
			maxLevel, maxKey = h.level, h.key
		}
	}
	c.mu.Unlock()

	if maxLevel > level {
		msg := fmt.Sprintf(
			`lock order violation: acquiring %q (level %d) while holding %q (level %d); expected queue:* → state:* → result:*`,
			key, level, maxKey, maxLevel,
		)
		c.violate(msg)
	}
}

func (c *debugOrderChecker) AfterLock(key string) {
	if c.mode == lockOrderOff {
		return
	}
	level, ok := LevelForKey(key)
	if !ok {
		return
	}
	gid := currentGID()

	c.mu.Lock()
	c.held[gid] = append(c.held[gid], heldLock{key: key, level: level})
	c.mu.Unlock()
}

func (c *debugOrderChecker) BeforeUnlock(key string) {
	if c.mode == lockOrderOff {
		return
	}
	if _, ok := LevelForKey(key); !ok {
		return
	}
	gid := currentGID()

	c.mu.Lock()
	defer c.mu.Unlock()

	locks := c.held[gid]
	for i := len(locks) - 1; i >= 0; i-- {
		if locks[i].key == key {
			locks = append(locks[:i], locks[i+1:]...)
			break
		}
	}
	if len(locks) == 0 {
		delete(c.held, gid)
	} else {
		c.held[gid] = locks
	}
}

func (c *debugOrderChecker) violate(msg string) {
	if c.mode == lockOrderWarn {
		log.Printf("LOCKORDER: %s", msg)
		return
	}
	panic(msg)
}

// currentGID extracts the goroutine ID from a runtime.Stack snapshot.
// This is a diagnostic-only helper and its behaviour depends on the Go
// runtime's stack trace format (stable since Go 1.0, but not a public API).
func currentGID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false) // "goroutine 123 [running]:\n..."
	s := strings.TrimPrefix(string(buf[:n]), "goroutine ")
	i := strings.IndexByte(s, ' ')
	if i <= 0 {
		return 0
	}
	id, _ := strconv.ParseUint(s[:i], 10, 64)
	return id
}
