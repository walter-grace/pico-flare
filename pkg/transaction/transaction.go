// Package transaction provides transactional R2 writes with rollback on failure.
package transaction

import (
	"context"
	"sync"

	"github.com/bigneek/picoflare/pkg/storage"
)

// Txn tracks R2 writes and can revert them on Rollback.
type Txn struct {
	r2     *storage.R2Client
	bucket string
	prefix string // e.g. agents/{agentID}/

	// undo: key -> original content (nil = didn't exist)
	undo   map[string][]byte
	mu     sync.Mutex
	active bool
}

// New creates a transaction scoped to the given prefix.
func New(r2 *storage.R2Client, bucket, prefix string) *Txn {
	return &Txn{
		r2:     r2,
		bucket: bucket,
		prefix: prefix,
		undo:   make(map[string][]byte),
		active: true,
	}
}

// Put records a write. On Rollback, the original value is restored.
func (t *Txn) Put(ctx context.Context, key string, data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active {
		return nil
	}
	fullKey := t.prefix + key
	if _, ok := t.undo[fullKey]; !ok {
		orig, _ := t.r2.DownloadObject(ctx, t.bucket, fullKey)
		t.undo[fullKey] = orig // nil if didn't exist
	}
	return t.r2.UploadObject(ctx, t.bucket, fullKey, data)
}

// Rollback reverts all writes in this transaction.
func (t *Txn) Rollback(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.active {
		return nil
	}
	t.active = false
	for fullKey, orig := range t.undo {
		if orig == nil {
			_ = t.r2.DeleteObject(ctx, t.bucket, fullKey)
		} else {
			_ = t.r2.UploadObject(ctx, t.bucket, fullKey, orig)
		}
	}
	return nil
}

// Commit marks the transaction complete (no rollback needed).
func (t *Txn) Commit() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active = false
	t.undo = nil
}
