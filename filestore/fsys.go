package filestore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// The store follows no link. Every path it opens is opened through an
// [os.Root] on its directory, so nothing it opens resolves outside the
// directory whatever a link in it says, and every file and scope
// directory is checked with Lstat as well, so a link inside the
// directory is not followed either: a link where an entry file would be
// is not an entry, a scope directory that is a link holds no entries
// and takes no write, and a journal, cursor or lock that is a link is
// refused. A committed project/x.md that points at a key elsewhere on
// the machine is never read, rendered or journaled. A write replaces
// the file it writes by a rename, which replaces a link rather than
// writing through it.

// errNotRegular is a path that is a link, a directory or anything but
// a regular file where the store expects one.
var errNotRegular = errors.New("not a regular file")

// root opens the store's directory as a root. Each operation opens its
// own and closes it, so a store holds no descriptor between calls.
func (s *Store) root() (*os.Root, error) {
	r, err := os.OpenRoot(s.dir)
	if err != nil {
		return nil, fmt.Errorf("filestore: open %s: %w", s.dir, err)
	}
	return r, nil
}

// scopeIsDir reports whether scope's directory in r exists, and refuses
// one that is a link, or a Windows junction, which reads as irregular:
// the store does not follow it, and says so rather than read the scope
// as empty, which would have Reconcile tombstone what is behind it.
func scopeIsDir(r *os.Root, scope string) (bool, error) {
	info, err := r.Lstat(scope)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("filestore: %s: %w", scope, err)
	case info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
		return false, linkedScope(scope)
	}
	return info.IsDir(), nil
}

func linkedScope(scope string) error {
	return fmt.Errorf("filestore: scope directory %s is a link, which the store does not follow; replace it with a directory: %w", scope, errNotRegular)
}

// isDir reports whether name in r is a directory and not a link to
// one; a name that does not exist is not.
func isDir(r *os.Root, name string) (bool, error) {
	info, err := r.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("filestore: %s: %w", name, err)
	}
	return info.IsDir(), nil
}

// openRegular opens name in r with flag, and refuses it, with
// [errNotRegular], unless name is a regular file and not a link: the
// name's own Lstat must be the file opened, so a link swapped in between
// the check and the open is caught too.
func openRegular(r *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
	if info, err := r.Lstat(name); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("filestore: %s: %w", name, errNotRegular)
	}
	f, err := r.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	opened, ferr := f.Stat()
	named, lerr := r.Lstat(name)
	if ferr != nil || lerr != nil || !named.Mode().IsRegular() || !os.SameFile(opened, named) {
		f.Close()
		return nil, fmt.Errorf("filestore: %s: %w", name, errNotRegular)
	}
	return f, nil
}

// readRegular reads name in r whole, with the file's information, as
// [openRegular] allows it.
func readRegular(r *os.Root, name string) ([]byte, fs.FileInfo, error) {
	f, err := openRegular(r, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

// writeAtomic writes data to a temporary file beside name, syncs it,
// and renames it into place, so a reader sees the old file or the new
// one and never a partial write. The rename replaces whatever is at
// name, a link included, without following it. The directory name is
// in is created when missing and refused when it is a link.
func (s *Store) writeAtomic(name string, data []byte) error {
	r, err := s.root()
	if err != nil {
		return err
	}
	defer r.Close()
	dir := filepath.Dir(name)
	if dir != "." {
		if err := r.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("filestore: create %s: %w", dir, err)
		}
		if ok, err := isDir(r, dir); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("filestore: %s is not a directory of the store's: it is a link or a file", dir)
		}
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("filestore: temporary name: %w", err)
	}
	tmp := filepath.Join(dir, ".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("filestore: create temporary file in %s: %w", dir, err)
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = r.Rename(tmp, name)
	}
	if werr != nil {
		r.Remove(tmp)
		return fmt.Errorf("filestore: write %s: %w", name, werr)
	}
	if d, err := r.Open(dir); err == nil {
		// Make the rename durable where the platform allows a
		// directory to be synced; where it does not, the file is.
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// readFile reads name in the store's root as [readRegular] allows it.
func (s *Store) readFile(name string) ([]byte, error) {
	r, err := s.root()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, _, err := readRegular(r, name)
	return data, err
}

// openFile opens name in the store's root as [openRegular] allows it.
// The file stays open after the root is closed.
func (s *Store) openFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	r, err := s.root()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return openRegular(r, name, flag, perm)
}

// readNoFollow reads the file at path whole, and refuses it, with
// [errNotRegular], unless path is a regular file and not a link. It is
// for the lock and its takeover claims, which are named by path.
func readNoFollow(path string) ([]byte, error) {
	if info, err := os.Lstat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("filestore: %s: %w", path, errNotRegular)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, ferr := f.Stat()
	named, lerr := os.Lstat(path)
	if ferr != nil || lerr != nil || !named.Mode().IsRegular() || !os.SameFile(opened, named) {
		return nil, fmt.Errorf("filestore: %s: %w", path, errNotRegular)
	}
	return io.ReadAll(f)
}
