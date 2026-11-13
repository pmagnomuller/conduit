// Package seats caps how many requests for one provider and model are in
// flight inside this process. A missing cap means unlimited.
package seats

import "sync"

type Lease struct {
	InFlight int `json:"in_flight"`
	Cap      int `json:"cap"`
}

type Table struct {
	mu       sync.Mutex
	caps     map[string]int
	inFlight map[string]int
}

func New(caps map[string]int) *Table {
	copied := map[string]int{}
	for key, cap := range caps {
		if cap < 0 {
			continue
		}
		copied[key] = cap
	}
	return &Table{caps: copied, inFlight: map[string]int{}}
}

// Acquire reserves one seat. The release func is safe to call once.
func (t *Table) Acquire(provider, model string) (release func(), ok bool) {
	if t == nil {
		return func() {}, true
	}
	key := provider + "/" + model
	t.mu.Lock()
	defer t.mu.Unlock()
	cap, limited := t.caps[key]
	if !limited {
		if wild, ok := t.caps[provider+"/*"]; ok {
			cap, limited, key = wild, true, provider+"/*"
		}
	}
	if !limited {
		return func() {}, true
	}
	if t.inFlight[key] >= cap {
		return nil, false
	}
	t.inFlight[key]++
	released := false
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if released {
			return
		}
		released = true
		if t.inFlight[key] > 0 {
			t.inFlight[key]--
		}
	}, true
}

func (t *Table) Snapshot() map[string]Lease {
	if t == nil || len(t.caps) == 0 {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]Lease, len(t.caps))
	for key, cap := range t.caps {
		out[key] = Lease{InFlight: t.inFlight[key], Cap: cap}
	}
	return out
}
