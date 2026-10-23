package recorder

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Dir is an Uploader that stores objects as files under Root, keeping the S3 layout. The local
// command uses it when no bucket is given.
type Dir struct {
	Root string
}

func (d *Dir) Put(_ context.Context, key, _ string, body io.ReadSeeker) error {
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return fmt.Errorf("recorder: invalid key %q", key)
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	path := filepath.Join(d.Root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	return writeFileAtomic(path, data)
}

// Path is where Put stored key.
func (d *Dir) Path(key string) string { return filepath.Join(d.Root, filepath.FromSlash(key)) }
