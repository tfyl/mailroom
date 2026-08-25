package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Dir stores blobs as files under one directory.
//
// This is the default backend and the only one this build ships, because mailroom is meant to
// run from `docker run` against nothing: a clone that needed an object store to hand back an
// attachment would have traded the problem for a harder one. The Bytes interface is the seam
// where an S3-compatible backend goes, and nothing above it would change — the metadata,
// the signing, the expiry and every authorisation check are already independent of where the
// bytes sit.
//
// The directory lives on the deployment's existing data volume beside the database, so it is
// covered by whatever already backs that up and constrained by the same disk. Files are
// written 0600 into a directory created 0700: the container runs as one uid, and an
// attachment is somebody's mail.
type Dir struct{ root string }

func NewDir(root string) (*Dir, error) {
	if root == "" {
		return nil, errors.New("an attachment directory is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating the attachment directory %s: %w", root, err)
	}
	return &Dir{root: root}, nil
}

// path refuses to turn a key into anything but a direct child of the root. Keys are generated
// ids and can never contain a separator, so this can only ever fire on a programming error —
// which is exactly when a path traversal would otherwise be silent.
func (d *Dir) path(key string) (string, error) {
	if key == "" || strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return "", fmt.Errorf("%w: %q is not a usable blob key", ErrNotFound, key)
	}
	return filepath.Join(d.root, key), nil
}

// Put writes at most limit bytes and refuses anything longer.
//
// It reads one byte past the limit rather than trusting a declared length, so a client that
// lies in Content-Length or sends a chunked body is caught by the copy itself. The write goes
// to a temporary file and is renamed into place, so a key never resolves to a partial file
// and an aborted upload leaves nothing behind.
func (d *Dir) Put(_ context.Context, key string, r io.Reader, limit int64) (int64, error) {
	final, err := d.path(key)
	if err != nil {
		return 0, err
	}

	tmp, err := os.CreateTemp(d.root, ".part-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())

	written, copyErr := io.Copy(tmp, io.LimitReader(r, limit+1))
	closeErr := tmp.Close()
	switch {
	case copyErr != nil:
		return 0, copyErr
	case closeErr != nil:
		return 0, closeErr
	case written > limit:
		return 0, fmt.Errorf("%w: over %d bytes", ErrTooLarge, limit)
	}

	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return 0, err
	}
	return written, nil
}

func (d *Dir) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	path, err := d.path(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

func (d *Dir) Delete(_ context.Context, key string) error {
	path, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
