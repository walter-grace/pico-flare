// Package agentfs provides a per-agent file system backed by R2.
// Keys: agents/{agentID}/workspace/{path}
package agentfs

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/bigneek/picoflare/pkg/storage"
)

// FS is an R2-backed file system scoped to a single agent.
type FS struct {
	r2     *storage.R2Client
	bucket string
	prefix string // agents/{agentID}/workspace/
}

// New creates a per-agent file system. agentID is typically chatID (e.g. "chat-123456").
func New(r2 *storage.R2Client, bucket, agentID string) *FS {
	if agentID == "" {
		agentID = "default"
	}
	return &FS{
		r2:     r2,
		bucket: bucket,
		prefix: fmt.Sprintf("agents/%s/workspace/", agentID),
	}
}

func (f *FS) key(p string) string {
	// Normalize: no leading slash, clean path
	p = strings.TrimPrefix(path.Clean(p), "/")
	if p == "." {
		p = ""
	}
	return f.prefix + p
}

// ReadFile reads a file from the agent's workspace.
func (f *FS) ReadFile(ctx context.Context, filePath string) ([]byte, error) {
	if f.r2 == nil {
		return nil, fmt.Errorf("agentfs: no R2 client")
	}
	return f.r2.DownloadObject(ctx, f.bucket, f.key(filePath))
}

// WriteFile writes a file to the agent's workspace.
func (f *FS) WriteFile(ctx context.Context, filePath string, data []byte) error {
	if f.r2 == nil {
		return fmt.Errorf("agentfs: no R2 client")
	}
	return f.r2.UploadObject(ctx, f.bucket, f.key(filePath), data)
}

// ListDir lists files and directories under the given path.
// Returns entries as "name" or "name/" for directories.
func (f *FS) ListDir(ctx context.Context, dirPath string) ([]string, error) {
	if f.r2 == nil {
		return nil, fmt.Errorf("agentfs: no R2 client")
	}
	prefix := f.key(dirPath)
	if prefix != f.prefix {
		prefix += "/"
	}
	keys, err := f.r2.ListObjects(ctx, f.bucket, prefix, 500)
	if err != nil {
		return nil, err
	}
	// Dedupe: for keys like agents/x/workspace/a/b/c, we want immediate children
	seen := make(map[string]bool)
	var result []string
	for _, k := range keys {
		rel := strings.TrimPrefix(k, prefix)
		if rel == "" {
			continue
		}
		parts := strings.SplitN(rel, "/", 2)
		name := parts[0]
		if len(parts) > 1 {
			name += "/" // directory
		}
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	return result, nil
}

// DeleteFile deletes a file from the agent's workspace.
func (f *FS) DeleteFile(ctx context.Context, filePath string) error {
	if f.r2 == nil {
		return fmt.Errorf("agentfs: no R2 client")
	}
	return f.r2.DeleteObject(ctx, f.bucket, f.key(filePath))
}

// Exists returns true if the path exists.
func (f *FS) Exists(ctx context.Context, filePath string) (bool, error) {
	if f.r2 == nil {
		return false, nil
	}
	return f.r2.ObjectExists(ctx, f.bucket, f.key(filePath))
}
