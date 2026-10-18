package oidc

import (
	"container/list"
	"sync"
	"time"

	"github.com/bdubs00/featherweight-kernel/fw"
)

// entry is one cached verdict. A verified token has a principal and lasts
// until the token expires. A rejected token has a reason and lasts for a
// short time.
type entry struct {
	principal *fw.Principal
	reason    string
	until     time.Time
}

// item is what the list holds: the key of the entry, so eviction from the
// back of the list finds the map entry.
type item struct {
	key [32]byte
	entry
}

// cache keeps verdicts by the SHA-256 of the token, bounded to max
// entries. The least recently used entry goes first. It is safe for
// concurrent use.
type cache struct {
	mu      sync.Mutex
	max     int
	entries map[[32]byte]*list.Element
	order   *list.List // front is the most recently used
}

func newCache(max int) *cache {
	return &cache{max: max, entries: map[[32]byte]*list.Element{}, order: list.New()}
}

// get returns the entry of a key when it is present and not past its time.
func (c *cache) get(key [32]byte, now time.Time) (entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return entry{}, false
	}
	it := el.Value.(*item)
	if !now.Before(it.until) {
		c.order.Remove(el)
		delete(c.entries, key)
		return entry{}, false
	}
	c.order.MoveToFront(el)
	return it.entry, true
}

// put stores an entry and evicts the least recently used one when the
// cache is full.
func (c *cache) put(key [32]byte, e entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*item).entry = e
		c.order.MoveToFront(el)
		return
	}
	for c.max > 0 && c.order.Len() >= c.max {
		back := c.order.Back()
		c.order.Remove(back)
		delete(c.entries, back.Value.(*item).key)
	}
	c.entries[key] = c.order.PushFront(&item{key: key, entry: e})
}

// len returns the number of entries, expired ones included.
func (c *cache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// clonePrincipal copies a principal so a caller cannot change the cached one.
func clonePrincipal(p *fw.Principal) *fw.Principal {
	if p == nil {
		return nil
	}
	out := *p
	if p.Groups != nil {
		out.Groups = append([]string(nil), p.Groups...)
	}
	if p.Scopes != nil {
		out.Scopes = append([]string(nil), p.Scopes...)
	}
	return &out
}
