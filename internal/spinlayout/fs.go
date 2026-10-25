package spinlayout

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// Every path below is opened relative to a directory descriptor with
// openat2, so a path can neither leave its root (RESOLVE_BENEATH) nor follow
// a link (RESOLVE_NO_SYMLINKS, RESOLVE_NO_MAGICLINKS) nor cross into another
// mount (RESOLVE_NO_XDEV, input only).
const (
	resolveIn  = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV
	resolveOut = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS
)

// openDir opens an absolute directory without following a final link.
func openDir(path string) (int, error) {
	return unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// openIn opens a regular file below root for reading.
func openIn(root int, rel string) (*os.File, int64, error) {
	fd, err := unix.Openat2(root, rel, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK,
		Resolve: resolveIn,
	})
	if err != nil {
		return nil, 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, 0, errors.New("not a regular file")
	}
	return os.NewFile(uintptr(fd), rel), st.Size, nil
}

// readIn reads a regular file below root, at most limit bytes.
func readIn(root int, rel string, limit int64) ([]byte, error) {
	f, size, err := openIn(root, rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if size > limit {
		return nil, errTooLarge
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

var errTooLarge = errors.New("larger than the limit")

// outRoot is the converter's output directory: created fresh, written only
// through its descriptor.
type outRoot struct {
	fd   int
	path string
	dirs []string // relative directories created, for the final chmod
}

// newOutRoot creates path, which must not exist, under an existing parent.
func newOutRoot(path string) (*outRoot, error) {
	// Spin runs as another user and must read the tree; seal makes it 0555.
	if err := os.Mkdir(path, 0o755); err != nil { //nolint:gosec // read by Spin's user
		return nil, err
	}
	fd, err := openDir(path)
	if err != nil {
		return nil, err
	}
	return &outRoot{fd: fd, path: path}, nil
}

func (o *outRoot) close() { _ = unix.Close(o.fd) }

// mkdirAll creates the directories of rel (slash-separated, already
// validated) below the root.
func (o *outRoot) mkdirAll(rel string) error {
	cur := ""
	for _, part := range strings.Split(rel, "/") {
		if cur == "" {
			cur = part
		} else {
			cur += "/" + part
		}
		dfd, err := unix.Openat2(o.fd, cur, &unix.OpenHow{
			Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: resolveOut,
		})
		if err == nil {
			_ = unix.Close(dfd)
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return err
		}
		parent, name, pfd := o.fd, cur, -1
		if i := strings.LastIndexByte(cur, '/'); i >= 0 {
			pfd, err = unix.Openat2(o.fd, cur[:i], &unix.OpenHow{
				Flags: unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC, Resolve: resolveOut,
			})
			if err != nil {
				return err
			}
			parent, name = pfd, cur[i+1:]
		}
		err = unix.Mkdirat(parent, name, 0o755)
		if pfd >= 0 {
			_ = unix.Close(pfd)
		}
		if err != nil {
			return err
		}
		o.dirs = append(o.dirs, cur)
	}
	return nil
}

// create creates the regular file rel, which must not exist, with mode
// 0444, and returns it open for writing.
func (o *outRoot) create(rel string) (*os.File, error) {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		if err := o.mkdirAll(rel[:i]); err != nil {
			return nil, err
		}
	}
	fd, err := unix.Openat2(o.fd, rel, &unix.OpenHow{
		Flags:   unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Mode:    0o444,
		Resolve: resolveOut,
	})
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), rel), nil
}

// rename moves one file below the root to another name below it.
func (o *outRoot) rename(from, to string) error {
	return unix.Renameat2(o.fd, from, o.fd, to, unix.RENAME_NOREPLACE)
}

// seal makes every directory 0555 (files are created 0444), deepest first.
func (o *outRoot) seal() error {
	for i := len(o.dirs) - 1; i >= 0; i-- {
		if err := unix.Fchmodat(o.fd, o.dirs[i], 0o555, 0); err != nil {
			return fmt.Errorf("seal %s: %w", o.dirs[i], err)
		}
	}
	return unix.Fchmod(o.fd, 0o555)
}

// remove deletes the output tree, making directories writable first.
func removeTree(path string) {
	_ = os.Chmod(path, 0o700) //nolint:gosec // a directory, made writable only to delete it
	_ = walkChmod(path)
	_ = os.RemoveAll(path)
}

func walkChmod(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			p := path + "/" + e.Name()
			_ = os.Chmod(p, 0o700) //nolint:gosec // a directory, made writable only to delete it
			_ = walkChmod(p)
		}
	}
	return nil
}
