package main

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openPTY allocates a new PTY pair in PURE Go — no cgo openpty (sandboxd is a
// static CGO-free guest ELF). It opens /dev/ptmx, reads the slave number via
// TIOCGPTN, unlocks it via TIOCSPTLCK, then opens /dev/pts/N. master is the host
// side (process output + keyboard input); slave becomes the child's controlling
// terminal. Both are O_NOCTTY so opening them doesn't steal a controlling tty.
func openPTY(rows, cols uint16) (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	// TIOCSPTLCK with 0 unlocks the slave (Linux ptmx handshake).
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, nil, err
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	if err := setWinsize(master, rows, cols); err != nil {
		master.Close()
		slave.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// setWinsize applies a terminal size to the pty master (TIOCSWINSZ). resize =
// Update in the wire; the kernel signals SIGWINCH to the foreground process group
// process group so e.g. `stty size` reflects the new dimensions.
func setWinsize(master *os.File, rows, cols uint16) error {
	return unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: rows, Col: cols,
	})
}
