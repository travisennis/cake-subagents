//go:build unix

package subagent

import (
	"os"
	"os/signal"
	"syscall"
)

func flock(file *os.File, nonBlocking bool) error {
	operation := syscall.LOCK_EX
	if nonBlocking {
		operation |= syscall.LOCK_NB
	}
	return syscall.Flock(int(file.Fd()), operation)
}

func signalGroup(pid int, signal syscall.Signal) {
	if pid > 0 {
		_ = syscall.Kill(-pid, signal)
	}
}

func ignoreHangup() {
	signal.Ignore(syscall.SIGHUP)
}

// closeOnExec keeps an inherited lock descriptor in this process while hiding
// it from children. Go's exec does not close unrelated descriptors for us, and
// the supervisor's flock leases must never leak into the Cake child: a killed
// supervisor would otherwise look alive until that child exits.
func closeOnExec(fd int) {
	syscall.CloseOnExec(fd)
}
