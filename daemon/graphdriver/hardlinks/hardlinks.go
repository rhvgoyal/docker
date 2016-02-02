// +build linux

package hardlinks

import (
	"fmt"
	"github.com/Sirupsen/logrus"
	"io/ioutil"
	"os"
	"path"
	"sync"
	"syscall"

	"github.com/docker/docker/daemon/graphdriver"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/idtools"

	"github.com/docker/docker/pkg/mount"
	"github.com/opencontainers/runc/libcontainer/label"
)

// This is a small wrapper over the NaiveDiffWriter that lets us have a custom
// implementation of ApplyDiff()

var (
	// ErrApplyDiffFallback is returned to indicate that a normal ApplyDiff is applied as a fallback from Naive diff writer.
	ErrApplyDiffFallback = fmt.Errorf("Fall back to normal ApplyDiff")
)

// ApplyDiffProtoDriver wraps the ProtoDriver by extending the interface with ApplyDiff method.
type ApplyDiffProtoDriver interface {
	graphdriver.ProtoDriver
	// ApplyDiff writes the diff to the archive for the given id and parent id.
	// It returns the size in bytes written if successful, an error ErrApplyDiffFallback is returned otherwise.
	ApplyDiff(id, parent string, diff archive.Reader) (size int64, err error)
}

type naiveDiffDriverWithApply struct {
	graphdriver.Driver
	applyDiff ApplyDiffProtoDriver
}

// NaiveDiffDriverWithApply returns a NaiveDiff driver with custom ApplyDiff.
func NaiveDiffDriverWithApply(driver ApplyDiffProtoDriver, uidMaps, gidMaps []idtools.IDMap) graphdriver.Driver {
	return &naiveDiffDriverWithApply{
		Driver:    graphdriver.NewNaiveDiffDriver(driver, uidMaps, gidMaps),
		applyDiff: driver,
	}
}

// ApplyDiff creates a diff layer with either the NaiveDiffDriver or with a fallback.
func (d *naiveDiffDriverWithApply) ApplyDiff(id, parent string, diff archive.Reader) (int64, error) {
	b, err := d.applyDiff.ApplyDiff(id, parent, diff)
	if err == ErrApplyDiffFallback {
		return d.Driver.ApplyDiff(id, parent, diff)
	}
	return b, err
}

// ActiveMount contains information about the count, path and whether is mounted or not.
// This information is part of the Driver, that contains list of active mounts.
type ActiveMount struct {
	count   int
	path    string
	mounted bool
}

// Driver contains information about the home directory and the list of active mounts that are created using this driver.
type Driver struct {
	home       string
	sync.Mutex // Protects concurrent modification to active
	active     map[string]*ActiveMount
	uidMaps    []idtools.IDMap
	gidMaps    []idtools.IDMap
}

var backingFs = "<unknown>"

func init() {
	graphdriver.Register("hardlinks", Init)
}

func Init(home string, options []string, uidMaps, gidMaps []idtools.IDMap) (graphdriver.Driver, error) {
	rootUID, rootGID, err := idtools.GetRootUIDGID(uidMaps, gidMaps)
	if err != nil {
		return nil, err
	}

	// Create the driver home dir
	if err := idtools.MkdirAllAs(home, 0700, rootUID, rootGID); err != nil && !os.IsExist(err) {
		return nil, err
	}

	d := &Driver{
		home:    home,
		active:  make(map[string]*ActiveMount),
		uidMaps: uidMaps,
		gidMaps: gidMaps,
	}

	return NaiveDiffDriverWithApply(d, uidMaps, gidMaps), nil
}

func (d *Driver) String() string {
	return "hardlinks"
}

// Status returns current driver information in a two dimensional string array.
// Output contains "Backing Filesystem" used in this implementation.
func (d *Driver) Status() [][2]string {
	return [][2]string{
		{"Backing Filesystem", backingFs},
	}
}

func (d *Driver) GetMetadata(id string) (map[string]string, error) {
	return nil, nil
}

// Cleanup simply returns nil and do not change the existing filesystem.
// This is required to satisfy the graphdriver.Driver interface.
func (d *Driver) Cleanup() error {
	return nil
}

func (d *Driver) Create(id, parent, mountLabel string) (retErr error) {
	dir := d.dir(id)

	rootUID, rootGID, err := idtools.GetRootUIDGID(d.uidMaps, d.gidMaps)
	if err != nil {
		return err
	}
	if err := idtools.MkdirAllAs(path.Dir(dir), 0700, rootUID, rootGID); err != nil {
		return err
	}
	if err := idtools.MkdirAs(dir, 0700, rootUID, rootGID); err != nil {
		return err
	}

	defer func() {
		// Clean up on failure
		if retErr != nil {
			os.RemoveAll(dir)
		}
	}()

	// If no parent has been specified, just create a root dir. Used
	// for images.
	if parent == "" {
		if err := idtools.MkdirAs(path.Join(dir, "root"), 0755, rootUID, rootGID); err != nil {
			return err
		}

		return nil
	}

	// This should be called for container.
	parentDir := d.dir(parent)
	// Ensure parent exists
	if _, err := os.Lstat(parentDir); err != nil {
		return err
	}

	parentDirRoot := path.Join(parentDir, "root")
	if _, err := os.Lstat(parentDirRoot); err != nil {
		return err
	}

	// Create root and mnt directories. mnt will be used for bind
	// mounting root.
	if err := idtools.MkdirAs(path.Join(dir, "root"), 0755, rootUID, rootGID); err != nil {
		return err
	}

	if err := idtools.MkdirAs(path.Join(dir, "mnt"), 0755, rootUID, rootGID); err != nil {
		return err
	}
	childDirRoot := path.Join(dir, "root")

	// Copy contents of parent with hardlinks created as appropriate.
	return copyDir(parentDirRoot, childDirRoot, copyHardlink)
}

func (d *Driver) dir(id string) string {
	return path.Join(d.home, id)
}

// Remove cleans the directories that are created for this id.
func (d *Driver) Remove(id string) error {
	if err := os.RemoveAll(d.dir(id)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Get creates and mounts the required file system for the given id and returns the mount path.
func (d *Driver) Get(id string, mountLabel string) (string, error) {
	// Protect the d.active from concurrent access
	d.Lock()
	defer d.Unlock()

	mntinfo := d.active[id]
	if mntinfo != nil {
		mntinfo.count++
		return mntinfo.path, nil
	}

	mntinfo = &ActiveMount{count: 1}

	dir := d.dir(id)
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("links: Directory %s is not present", dir)
	}

	rootDir := path.Join(dir, "root")
	if _, err := os.Stat(rootDir); err != nil {
		return "", fmt.Errorf("links: Directory %s is not present", rootDir)
	}

	mntDir := path.Join(dir, "mnt")
	if _, err := os.Stat(mntDir); err != nil {
		// mnt dir does not exist. It should be image id. Just return
		// root dir.
		mntinfo.path = rootDir
		d.active[id] = mntinfo
		return mntinfo.path, nil
	}

	// Mount root on mnt with option -ro for top level container.
	// Hack: docker writes some files on -init layer using setupInitLayer()
	// So if id has suffix -init, mount without read only. setupInitLayer
	// does not overwrite existing files instead first removes existing
	// files. So this should overwrite the existing file and not distrub
	// the underlying image layer.
	mountOpts := "bind"
	mountOpts = label.FormatMountLabel(mountOpts, mountLabel)

	if err := mount.Mount(rootDir, mntDir, "bind", mountOpts); err != nil {
		return "", fmt.Errorf("links: Failed to bind mount %s on %s: %v", rootDir, mntDir, err)
	}

	mntinfo.path = mntDir
	mntinfo.mounted = true
	d.active[id] = mntinfo
	return mntinfo.path, nil
}

// Put unmounts the mount path created for the give id.
func (d *Driver) Put(id string) error {
	// Protect the d.active from concurrent access
	d.Lock()
	defer d.Unlock()

	mntinfo := d.active[id]
	if mntinfo == nil {
		logrus.Debugf("hardlinks: Put on a non-mounted device %s", id)
		// but it might be still here
		if d.Exists(id) {
			mntDir := path.Join(d.dir(id), "mnt")
			if d.Exists(mntDir) {
				err := syscall.Unmount(mntDir, 0)
				if err != nil {
					logrus.Debugf("hardlinks: Failed to unmount %s: %v", mntDir, err)
				}
			}
		}
		return nil
	}

	mntinfo.count--
	if mntinfo.count > 0 {
		return nil
	}

	defer delete(d.active, id)
	if mntinfo.mounted {
		err := syscall.Unmount(mntinfo.path, 0)
		if err != nil {
			logrus.Debugf("hardlinks: Failed to unmount %s: %v", mntinfo.path, err)
		}
		return err
	}
	return nil
}

// ApplyDiff applies the new layer on top of the root, if parent does not exist with will return a ErrApplyDiffFallback error.
func (d *Driver) ApplyDiff(id string, parent string, diff archive.Reader) (size int64, err error) {
	dir := d.dir(id)

	if parent == "" {
		return 0, ErrApplyDiffFallback
	}

	parentRootDir := path.Join(d.dir(parent), "root")
	if _, err := os.Stat(parentRootDir); err != nil {
		return 0, ErrApplyDiffFallback
	}

	// We now know there is a parent, and it has a "root" directory containing
	// the full root filesystem. We can just hardlink it and apply the
	// layer. This relies on two things:
	// 1) ApplyDiff is only run once on a clean (no writes to upper layer) container
	// 2) ApplyDiff doesn't do any in-place writes to files (would break hardlinks)
	// These are all currently true and are not expected to break

	tmpRootDir, err := ioutil.TempDir(dir, "tmproot")
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmpRootDir)
		} else {
			os.RemoveAll(path.Join(dir, "mnt"))
		}
	}()

	if err = copyDir(parentRootDir, tmpRootDir, copyHardlink); err != nil {
		return 0, err
	}

	options := &archive.TarOptions{UIDMaps: d.uidMaps, GIDMaps: d.gidMaps}
	if size, err = chrootarchive.ApplyUncompressedLayer(tmpRootDir, diff, options); err != nil {
		return 0, err
	}

	rootDir := path.Join(dir, "root")
	if err := os.Rename(tmpRootDir, rootDir); err != nil {
		return 0, err
	}

	return
}

// Exists checks to see if the id is already mounted.
func (d *Driver) Exists(id string) bool {
	_, err := os.Stat(d.dir(id))
	return err == nil
}
