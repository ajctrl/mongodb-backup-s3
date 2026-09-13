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
	"time"
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
	cmd.WaitDelay = 5 * time.Second
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

type pipelineCommand struct {
	name string
	args []string
}

// pipeline waits for every process, including a producer that closes stdout
// before reporting an error. Only its caller may signal EOF to the uploader.
func (r commandRunner) pipeline(ctx context.Context, stdout io.Writer, specs ...pipelineCommand) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmds := make([]*exec.Cmd, len(specs))
	var pipes []*os.File
	closePipes := func() {
		for _, pipe := range pipes {
			pipe.Close()
		}
		pipes = nil
	}
	defer closePipes()
	for i, spec := range specs {
		cmd, closeFiles, err := r.command(ctx, spec.name, spec.args...)
		if err != nil {
			return err
		}
		defer closeFiles()
		cmds[i] = cmd
		// A slow upload may still be draining stdout after the child exits.
		// Cancellation closes the upload pipe; do not truncate it after WaitDelay.
		cmds[i].WaitDelay = 0
		if i > 0 {
			reader, writer, err := os.Pipe()
			if err != nil {
				return err
			}
			pipes = append(pipes, reader, writer)
			cmds[i-1].Stdout, cmds[i].Stdin = writer, reader
		}
	}
	cmds[len(cmds)-1].Stdout = stdout
	var started []int
	var result error
	// Start consumers first; OS pipes give backpressure without buffering dumps.
	for i := len(cmds) - 1; i >= 0; i-- {
		if err := cmds[i].Start(); err != nil {
			result = commandError(ctx, specs[i].name, err)
			cancel()
			break
		}
		started = append(started, i)
	}
	// Only children retain these descriptors, so early exits propagate EOF/EPIPE.
	closePipes()
	done := make(chan error, len(started))
	for _, i := range started {
		go func() { done <- commandError(ctx, specs[i].name, cmds[i].Wait()) }()
	}
	for range started {
		if err := <-done; err != nil {
			result = errors.Join(result, err)
			cancel()
		}
	}
	return result
}

func withoutEnv(environ []string, name string) []string {
	result := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, name+"=") {
			result = append(result, entry)
		}
	}
	return result
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
