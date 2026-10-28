//go:build darwin || linux

package render

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"
)

// WatchEsc calls onEsc for each Esc press on in until stop is called. While
// watching, the terminal is in cbreak mode (no line buffering or echo, Ctrl-C
// still signals); stop restores it. When in is not a terminal it does
// nothing.
func WatchEsc(in *os.File, onEsc func()) (stop func()) {
	fd := int(in.Fd())
	original, err := getTermios(fd)
	if err != nil {
		return func() {}
	}
	cbreak := *original
	cbreak.Lflag &^= syscall.ICANON | syscall.ECHO
	// Return from read after 100ms without input so the loop can see stop.
	cbreak.Cc[syscall.VMIN] = 0
	cbreak.Cc[syscall.VTIME] = 1
	if err := setTermios(fd, &cbreak); err != nil {
		return func() {}
	}

	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(func() { _ = setTermios(fd, original) })
	}

	// Ctrl-C must not leave the shell without echo.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		buffer := make([]byte, 32)
		for {
			select {
			case <-done:
				return
			case sig := <-signals:
				restore()
				signal.Reset(os.Interrupt, syscall.SIGTERM)
				_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
				return
			default:
			}
			n, err := syscall.Read(fd, buffer)
			if err != nil && err != syscall.EINTR && err != syscall.EAGAIN {
				return
			}
			if n > 0 && isLoneEsc(buffer[:n]) {
				onEsc()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		signal.Stop(signals)
		restore()
	}
}

func getTermios(fd int) (*syscall.Termios, error) {
	var termios syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlGetTermios, uintptr(unsafe.Pointer(&termios))); errno != 0 {
		return nil, errno
	}
	return &termios, nil
}

func setTermios(fd int, termios *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlSetTermios, uintptr(unsafe.Pointer(termios))); errno != 0 {
		return errno
	}
	return nil
}
