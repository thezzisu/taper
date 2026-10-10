package media

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// Linux sg's synchronous SG_IO wait returns ERESTARTSYS when interrupted.
// SA_RESTART can consequently submit the same tape READ/WRITE twice, even though
// the first command reached the drive. Go installs SA_RESTART handlers and uses
// SIGURG for preemption. Block asynchronous signals on this OS thread for the
// command; deliver pending signals after completion. Do not block synchronous
// fault signals needed by the runtime. Never retry an ambiguous tape command.
func protectTapeIO() (release func(), err error) {
	runtime.LockOSThread()
	var blocked, previous unix.Sigset_t
	for sig := 1; sig <= 64; sig++ {
		switch unix.Signal(sig) {
		case unix.SIGILL, unix.SIGTRAP, unix.SIGABRT, unix.SIGBUS, unix.SIGFPE, unix.SIGSEGV, unix.SIGSYS, unix.SIGKILL, unix.SIGSTOP:
			continue
		}
		blocked.Val[(sig-1)/64] |= uint64(1) << uint((sig-1)%64)
	}
	if err = unix.PthreadSigmask(unix.SIG_BLOCK, &blocked, &previous); err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	return func() {
		// The thread cannot be returned to the Go pool with the wrong mask.
		if e := unix.PthreadSigmask(unix.SIG_SETMASK, &previous, nil); e != nil {
			panic(e)
		}
		runtime.UnlockOSThread()
	}, nil
}
