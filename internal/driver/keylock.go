package driver

import "sync"

// keyLock serializes operations per key (a staging path): NodeStageVolume, NodeUnstageVolume and
// the recovery of one volume must not interleave, while different volumes proceed in parallel.
type keyLock struct {
	mu    sync.Mutex
	locks map[string]*keyLockEntry
}

type keyLockEntry struct {
	sync.Mutex
	refs int
}

// Lock locks key and returns its unlock function.
func (l *keyLock) Lock(key string) (unlock func()) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*keyLockEntry{}
	}
	e, ok := l.locks[key]
	if !ok {
		e = &keyLockEntry{}
		l.locks[key] = e
	}
	e.refs++
	l.mu.Unlock()

	e.Lock()

	return func() {
		e.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}
