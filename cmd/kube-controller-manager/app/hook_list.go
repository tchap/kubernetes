package app

import "sync"

// hookList is just a wrapper around []func() that it thread-safe.
type hookList struct {
	hooks []func()
	mu    sync.Mutex
	once  sync.Once
}

// Register appends a hook to the list.
func (hooks *hookList) Register(hook func()) {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.hooks = append(hooks.hooks, hook)
}

// Execute runs the hooks. The hooks are only ever run once, subsequent calls to Execute do nothing.
func (hooks *hookList) Execute() {
	hooks.mu.Lock()
	defer hooks.mu.Unlock()
	hooks.once.Do(func() {
		for _, hook := range hooks.hooks {
			hook()
		}
	})
}
