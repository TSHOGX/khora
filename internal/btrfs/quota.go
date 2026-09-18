package btrfs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// QuotaMode is the observed filesystem capability, not a requested policy.
type QuotaMode uint8

const (
	QuotaUnknown QuotaMode = iota
	NotBtrfs
	QuotaDisabled
	QuotaSimple
	QuotaUnsupported
)

// QuotaStatus reads the kernel quota status item without changing filesystem state.
// Permission and I/O errors remain unknown, never an assertion that quotas are off.
func (c Client) QuotaStatus(ctx context.Context, path string) (QuotaMode, error) {
	if err := ctx.Err(); err != nil {
		return QuotaUnknown, err
	}
	if c.ProbeQuota != nil {
		return c.ProbeQuota(ctx, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return QuotaUnknown, err
	}
	defer f.Close()
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return QuotaUnknown, err
	}
	if st.Type != 0x9123683e {
		return NotBtrfs, nil
	}
	// BTRFS_IOC_TREE_SEARCH: 104-byte key followed by result headers and payloads.
	var args [4096]byte
	native := binary.NativeEndian
	native.PutUint64(args[0:8], 8) // quota tree
	native.PutUint64(args[48:56], ^uint64(0))
	native.PutUint32(args[56:60], 240) // quota status item, object and offset zero
	native.PutUint32(args[60:64], 240)
	native.PutUint32(args[64:68], 1)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(0xd0009411), uintptr(unsafe.Pointer(&args[0])))
	if err := ctx.Err(); err != nil {
		return QuotaUnknown, err
	}
	if errno == unix.ENOENT {
		return QuotaDisabled, nil
	} // no quota tree
	if errno != 0 {
		return QuotaUnknown, fmt.Errorf("read quota status: %w", errno)
	}
	count := native.Uint32(args[64:68])
	if count == 0 {
		return QuotaUnknown, errors.New("quota tree has no status item")
	}
	header := args[104:136]
	if count != 1 || native.Uint64(header[8:16]) != 0 || native.Uint64(header[16:24]) != 0 || native.Uint32(header[24:28]) != 240 {
		return QuotaUnknown, errors.New("unexpected quota status item")
	}
	size := native.Uint32(header[28:32])
	if size < 32 || size > uint32(len(args)-136) {
		return QuotaUnknown, errors.New("invalid quota status size")
	}
	return quotaMode(args[136 : 136+size])
}

func quotaMode(item []byte) (QuotaMode, error) {
	if len(item) < 32 || binary.LittleEndian.Uint64(item[:8]) != 1 {
		return QuotaUnknown, errors.New("invalid quota status version")
	}
	flags := binary.LittleEndian.Uint64(item[16:24])
	if flags & ^uint64(15) != 0 {
		return QuotaUnknown, errors.New("unknown quota status flags")
	}
	if flags&1 == 0 {
		return QuotaDisabled, nil
	}
	if flags&8 == 0 {
		return QuotaUnsupported, nil
	}
	if flags&6 != 0 {
		return QuotaUnknown, errors.New("quota accounting is inconsistent or rescanning")
	}
	return QuotaSimple, nil
}
