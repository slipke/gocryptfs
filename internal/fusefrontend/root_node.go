package fusefrontend

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rfjakob/gocryptfs/internal/configfile"
	"github.com/rfjakob/gocryptfs/internal/contentenc"
	"github.com/rfjakob/gocryptfs/internal/inomap"
	"github.com/rfjakob/gocryptfs/internal/nametransform"
	"github.com/rfjakob/gocryptfs/internal/syscallcompat"
	"github.com/rfjakob/gocryptfs/internal/tlog"
)

// RootNode is the root of the filesystem tree of Nodes.
type RootNode struct {
	Node
	// args stores configuration arguments
	args Args
	// dirIVLock: Lock()ed if any "gocryptfs.diriv" file is modified
	// Readers must RLock() it to prevent them from seeing intermediate
	// states
	dirIVLock sync.RWMutex
	// Filename encryption helper
	nameTransform nametransform.NameTransformer
	// Content encryption helper
	contentEnc *contentenc.ContentEnc
	// This lock is used by openWriteOnlyFile() to block concurrent opens while
	// it relaxes the permissions on a file.
	openWriteOnlyLock sync.RWMutex
	// MitigatedCorruptions is used to report data corruption that is internally
	// mitigated by ignoring the corrupt item. For example, when OpenDir() finds
	// a corrupt filename, we still return the other valid filenames.
	// The corruption is logged to syslog to inform the user,	and in addition,
	// the corrupt filename is logged to this channel via
	// reportMitigatedCorruption().
	// "gocryptfs -fsck" reads from the channel to also catch these transparently-
	// mitigated corruptions.
	MitigatedCorruptions chan string
	// IsIdle flag is set to zero each time fs.isFiltered() is called
	// (uint32 so that it can be reset with CompareAndSwapUint32).
	// When -idle was used when mounting, idleMonitor() sets it to 1
	// periodically.
	IsIdle uint32
	// inoMap translates inode numbers from different devices to unique inode
	// numbers.
	inoMap *inomap.InoMap
}

func NewRootNode(args Args, c *contentenc.ContentEnc, n nametransform.NameTransformer) *RootNode {
	// TODO
	return &RootNode{
		args:          args,
		nameTransform: n,
		contentEnc:    c,
		inoMap:        inomap.New(),
	}
}

// mangleOpenFlags is used by Create() and Open() to convert the open flags the user
// wants to the flags we internally use to open the backing file.
// The returned flags always contain O_NOFOLLOW.
func (rn *RootNode) mangleOpenFlags(flags uint32) (newFlags int) {
	newFlags = int(flags)
	// Convert WRONLY to RDWR. We always need read access to do read-modify-write cycles.
	if (newFlags & syscall.O_ACCMODE) == syscall.O_WRONLY {
		newFlags = newFlags ^ os.O_WRONLY | os.O_RDWR
	}
	// We also cannot open the file in append mode, we need to seek back for RMW
	newFlags = newFlags &^ os.O_APPEND
	// O_DIRECT accesses must be aligned in both offset and length. Due to our
	// crypto header, alignment will be off, even if userspace makes aligned
	// accesses. Running xfstests generic/013 on ext4 used to trigger lots of
	// EINVAL errors due to missing alignment. Just fall back to buffered IO.
	newFlags = newFlags &^ syscallcompat.O_DIRECT
	// Create and Open are two separate FUSE operations, so O_CREAT should not
	// be part of the open flags.
	newFlags = newFlags &^ syscall.O_CREAT
	// We always want O_NOFOLLOW to be safe against symlink races
	newFlags |= syscall.O_NOFOLLOW
	return newFlags
}

// reportMitigatedCorruption is used to report a corruption that was transparently
// mitigated and did not return an error to the user. Pass the name of the corrupt
// item (filename for OpenDir(), xattr name for ListXAttr() etc).
// See the MitigatedCorruptions channel for more info.
func (rn *RootNode) reportMitigatedCorruption(item string) {
	if rn.MitigatedCorruptions == nil {
		return
	}
	select {
	case rn.MitigatedCorruptions <- item:
	case <-time.After(1 * time.Second):
		tlog.Warn.Printf("BUG: reportCorruptItem: timeout")
		//debug.PrintStack()
		return
	}
}

// isFiltered - check if plaintext "path" should be forbidden
//
// Prevents name clashes with internal files when file names are not encrypted
func (rn *RootNode) isFiltered(path string) bool {
	atomic.StoreUint32(&rn.IsIdle, 0)

	if !rn.args.PlaintextNames {
		return false
	}
	// gocryptfs.conf in the root directory is forbidden
	if path == configfile.ConfDefaultName {
		tlog.Info.Printf("The name /%s is reserved when -plaintextnames is used\n",
			configfile.ConfDefaultName)
		return true
	}
	// Note: gocryptfs.diriv is NOT forbidden because diriv and plaintextnames
	// are exclusive
	return false
}

// decryptSymlinkTarget: "cData64" is base64-decoded and decrypted
// like file contents (GCM).
// The empty string decrypts to the empty string.
//
// This function does not do any I/O and is hence symlink-safe.
func (rn *RootNode) decryptSymlinkTarget(cData64 string) (string, error) {
	if cData64 == "" {
		return "", nil
	}
	cData, err := rn.nameTransform.B64DecodeString(cData64)
	if err != nil {
		return "", err
	}
	data, err := rn.contentEnc.DecryptBlock([]byte(cData), 0, nil)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Due to RMW, we always need read permissions on the backing file. This is a
// problem if the file permissions do not allow reading (i.e. 0200 permissions).
// This function works around that problem by chmod'ing the file, obtaining a fd,
// and chmod'ing it back.
func (rn *RootNode) openWriteOnlyFile(dirfd int, cName string, newFlags int) (rwFd int, err error) {
	woFd, err := syscallcompat.Openat(dirfd, cName, syscall.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer syscall.Close(woFd)
	var st syscall.Stat_t
	err = syscall.Fstat(woFd, &st)
	if err != nil {
		return
	}
	// The cast to uint32 fixes a build failure on Darwin, where st.Mode is uint16.
	perms := uint32(st.Mode)
	// Verify that we don't have read permissions
	if perms&0400 != 0 {
		tlog.Warn.Printf("openWriteOnlyFile: unexpected permissions %#o, returning EPERM", perms)
		err = syscall.EPERM
		return
	}
	// Upgrade the lock to block other Open()s and downgrade again on return
	rn.openWriteOnlyLock.RUnlock()
	rn.openWriteOnlyLock.Lock()
	defer func() {
		rn.openWriteOnlyLock.Unlock()
		rn.openWriteOnlyLock.RLock()
	}()
	// Relax permissions and revert on return
	err = syscall.Fchmod(woFd, perms|0400)
	if err != nil {
		tlog.Warn.Printf("openWriteOnlyFile: changing permissions failed: %v", err)
		return
	}
	defer func() {
		err2 := syscall.Fchmod(woFd, perms)
		if err2 != nil {
			tlog.Warn.Printf("openWriteOnlyFile: reverting permissions failed: %v", err2)
		}
	}()
	return syscallcompat.Openat(dirfd, cName, newFlags, 0)
}