//go:build linux

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func (f *Feature) delegate(ctx context.Context, args []string) (int, error) {
	// Linux parent-death signals are tied to the launching OS thread.
	goruntime.LockOSThread()
	defer goruntime.UnlockOSThread()
	command := exec.CommandContext(ctx, f.runtime, args...)
	command.Stdin, command.Stdout, command.Stderr = f.stdin, f.stdout, f.stderr
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	files, err := inheritedFiles()
	if err != nil {
		return 1, err
	}
	command.ExtraFiles = files
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(signals)
	if err := command.Start(); err != nil {
		return 1, fmt.Errorf("start real runtime: %w", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case sig := <-signals:
				_ = command.Process.Signal(sig)
			case <-done:
				return
			}
		}
	}()
	err = command.Wait()
	close(done)
	goruntime.KeepAlive(files)
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 1, fmt.Errorf("wait for real runtime: %w", err)
}

// Preserve only descriptors the supervisor explicitly inherited, including gaps.
func inheritedFiles() ([]*os.File, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno != 0 || flags&syscall.FD_CLOEXEC != 0 {
			continue
		}
		for len(files) <= fd-3 {
			files = append(files, nil)
		}
		files[fd-3] = os.NewFile(uintptr(fd), entry.Name())
	}
	return files, nil
}

// containerd reads error-level JSON from the supplied runc log, not just stderr.
func writeError(args []string, failure error) {
	for index := 0; index < len(args); index++ {
		name, path, inline := strings.Cut(args[index], "=")
		if name != "--log" {
			continue
		}
		if !inline {
			index++
			if index >= len(args) {
				return
			}
			path = args[index]
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return
		}
		defer file.Close()
		_ = json.NewEncoder(file).Encode(map[string]string{"level": "error", "msg": "sunray: " + failure.Error(), "time": time.Now().UTC().Format(time.RFC3339Nano)})
		return
	}
}
