package runner

import (
	"context"
	"time"
)

// A workspace writer can outlive a task deadline. Do not park the claimed
// execution indefinitely in RWMutex.RLock after its authority is canceled.
func (d *Daemon) waitTaskWorkspaceGate(ctx context.Context, key string) (func(), error) {
	gate := d.workspaceGate(key)
	for {
		if err := ctx.Err(); err != nil {
			return func() {}, err
		}
		if gate.TryRLock() {
			return onceRelease(gate.RUnlock), nil
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return func() {}, ctx.Err()
		case <-timer.C:
		}
	}
}
