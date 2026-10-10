package media

import (
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTapeIOBlocksSignalsAndRestoresThread(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var before, during, after unix.Sigset_t
	if e := unix.PthreadSigmask(unix.SIG_BLOCK, nil, &before); e != nil {
		t.Fatal(e)
	}
	func() {
		release, e := protectTapeIO()
		if e != nil {
			t.Fatal(e)
		}
		defer release()
		if e = unix.PthreadSigmask(unix.SIG_BLOCK, nil, &during); e != nil {
			t.Fatal(e)
		}
		for _, sig := range []unix.Signal{unix.SIGURG, unix.SIGINT, unix.SIGTERM, unix.SIGPROF, unix.SIGCHLD, unix.SIGUSR1} {
			if during.Val[(int(sig)-1)/64]&(uint64(1)<<uint((int(sig)-1)%64)) == 0 {
				t.Fatalf("%v can restart SG_IO", sig)
			}
		}
		if during.Val[0]&(uint64(1)<<uint(unix.SIGSEGV-1)) != before.Val[0]&(uint64(1)<<uint(unix.SIGSEGV-1)) {
			t.Fatal("runtime fault handling mask changed")
		}
	}()
	if e := unix.PthreadSigmask(unix.SIG_BLOCK, nil, &after); e != nil {
		t.Fatal(e)
	}
	if before != after {
		t.Fatal("signal mask leaked to the Go thread pool")
	}
}
