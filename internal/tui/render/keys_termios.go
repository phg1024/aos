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
	restore, err := enterTermMode(fd, func(mode *syscall.Termios) {
		mode.Lflag &^= syscall.ICANON | syscall.ECHO
		// Return from read after 100ms without input so the loop can see stop.
		mode.Cc[syscall.VMIN] = 0
		mode.Cc[syscall.VTIME] = 1
	})
	if err != nil {
		return func() {}
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		buffer := make([]byte, 32)
		for {
			select {
			case <-done:
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
		restore()
	}
}

// enterTermMode switches fd to a changed copy of its current mode and returns
// a function that restores the original. Until then, SIGINT or SIGTERM
// restores the terminal before the signal ends the process, so Ctrl-C never
// leaves the shell without echo.
func enterTermMode(fd int, change func(*syscall.Termios)) (restore func(), err error) {
	original, err := getTermios(fd)
	if err != nil {
		return nil, err
	}
	mode := *original
	change(&mode)
	if err := setTermios(fd, &mode); err != nil {
		return nil, err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-signals:
			_ = setTermios(fd, original)
			signal.Reset(os.Interrupt, syscall.SIGTERM)
			_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			signal.Stop(signals)
			_ = setTermios(fd, original)
		})
	}, nil
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
