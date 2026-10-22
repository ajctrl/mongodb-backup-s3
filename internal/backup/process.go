package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type commandRunner struct {
	defaultsFile   string
	passphraseFile string
	stderr         io.Writer
	lock           *os.File
}

func (r commandRunner) run(ctx context.Context, stdout io.Writer, name string, args ...string) error {
	return r.runInput(ctx, nil, stdout, name, args...)
}

func (r commandRunner) runInput(ctx context.Context, stdin io.Reader, stdout io.Writer, name string, args ...string) error {
	cmd, closeFiles, err := r.command(ctx, name, args...)
	if err != nil {
		return err
	}
	defer closeFiles()
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	// Streaming stdout may still be draining after the producer exits.
	cmd.WaitDelay = 0
	return commandError(ctx, name, cmd.Run())
}

func (r commandRunner) command(ctx context.Context, name string, args ...string) (*exec.Cmd, func(), error) {
	if name == "mongorestore" || name == "mongodump" {
		args = append([]string{"--config=" + r.defaultsFile}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = r.stderr
	cmd.Env = childEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if r.lock != nil {
		// The dump/encryption child keeps the lock if the parent is killed abruptly.
		cmd.ExtraFiles = []*os.File{r.lock}
	}
	closeFiles := func() {}
	if name == "gpg" {
		// Each invocation gets an independent offset, including repeated DB dumps.
		file, err := os.Open(r.passphraseFile)
		if err != nil {
			return nil, nil, fmt.Errorf("open GPG passphrase file: %w", err)
		}
		fd := 3 + len(cmd.ExtraFiles)
		cmd.ExtraFiles = append(cmd.ExtraFiles, file)
		cmd.Args = append([]string{cmd.Args[0], "--passphrase-fd", fmt.Sprint(fd)}, args...)
		closeFiles = func() { file.Close() }
	}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd, closeFiles, nil
}

func commandError(ctx context.Context, name string, err error) error {
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Keep command arguments and configuration out of error messages.
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func childEnvironment() []string {
	var result []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "MONGODB_") || key == "PASSPHRASE" {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func acquireLock(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open backup lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("Could not acquire backup lock; another backup may already be running: %w", err)
	}
	// Close releases the lock. Never unlink it or explicitly unlock the shared
	// file description: any surviving child must retain its inherited lock.
	return file, nil
}
