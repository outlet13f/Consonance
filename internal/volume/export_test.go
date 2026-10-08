package volume

import "context"

// AcquireForTest takes the operation lock the way an operation does.
func (v *Volume) AcquireForTest(ctx context.Context) (release func(), err error) {
	return v.acquire(ctx)
}

// CopyEndedForTest records a backup-mode copy of source that ended now.
func (v *Volume) CopyEndedForTest(source string) { v.copyEnded(source) }
