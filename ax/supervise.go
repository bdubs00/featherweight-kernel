package ax

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// StopGracePeriod is how long a supervised command has to exit after SIGTERM
// before Supervise sends SIGKILL. It matches the stock runner.
const StopGracePeriod = 10 * time.Second

// Supervise runs argv as a child in its own process group, with dir as the
// working directory, os.Environ() plus extraEnv as the environment, and the
// standard output and error of fw. It returns when the child exits.
//
// When ctx ends first, Supervise sends SIGTERM to the process group, waits
// StopGracePeriod, sends SIGKILL to what is left, and waits for the child.
//
// The exit code is the exit status of the child, or -1 when a signal killed
// it, like CommandExit.ExitCode of the stock runner. A non-zero exit status is
// not an error: err reports only a failure to start or to wait.
func Supervise(ctx context.Context, argv []string, dir string, extraEnv []string, log *slog.Logger) (exitCode int, err error) {
	if log == nil {
		log = slog.Default()
	}
	if len(argv) == 0 {
		return -1, errors.New("ax: supervise: empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("ax: start %s: %w", argv[0], err)
	}
	pid := cmd.Process.Pid
	log = log.With("pid", pid, "command", argv[0])
	log.Info("ax: started task command")

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-exited:
	case <-ctx.Done():
		waitErr = stopGroup(pid, exited, log)
	}

	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		log.Error("ax: wait for task command", "error", waitErr)
		return code, fmt.Errorf("ax: wait %s: %w", argv[0], waitErr)
	}
	if code == 0 {
		log.Info("ax: task command exited", "exitCode", code)
	} else {
		log.Warn("ax: task command exited", "exitCode", code)
	}
	return code, nil
}

// stopGroup sends SIGTERM to the process group, then SIGKILL after the grace
// period, and returns the result of Wait.
func stopGroup(pid int, exited <-chan error, log *slog.Logger) error {
	log.Info("ax: stopping task command")
	_ = unix.Kill(-pid, unix.SIGTERM)
	timer := time.NewTimer(StopGracePeriod)
	defer timer.Stop()
	select {
	case err := <-exited:
		return err
	case <-timer.C:
		log.Warn("ax: task command did not exit within the grace period; killing", "gracePeriod", StopGracePeriod)
		_ = unix.Kill(-pid, unix.SIGKILL)
		return <-exited
	}
}

const (
	reapInterval = 2 * time.Second
	// reapGrace is how long a zombie child must stay a zombie before the
	// reaper collects it. A child that this process started with os/exec is
	// collected by its own Wait at once. Only an orphan stays a zombie, so the
	// reaper does not take an exit status that a Wait call expects.
	reapGrace = 5 * time.Second
)

// ReapZombies collects orphaned children until ctx ends, when fw is PID 1.
// Otherwise it returns at once.
//
// A plain wait4(-1) loop takes the exit status of every child, including the
// children of os/exec, whose Wait then fails. So on SIGCHLD and every two
// seconds the reaper lists the zombie children in /proc and calls wait4 only
// for a child that has been a zombie for at least five seconds.
func ReapZombies(ctx context.Context) {
	if os.Getpid() != 1 {
		return
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGCHLD)
	defer signal.Stop(sigs)
	tick := time.NewTicker(reapInterval)
	defer tick.Stop()
	r := newReaper(os.Getpid())
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
		case <-tick.C:
		}
		if _, err := r.reap(time.Now()); err != nil {
			slog.Warn("ax: zombie reaper stopped", "error", err)
			return
		}
	}
}

type reaper struct {
	self int
	seen map[int]time.Time
}

func newReaper(self int) *reaper { return &reaper{self: self, seen: map[int]time.Time{}} }

// reap collects the zombie children that were already zombies reapGrace ago,
// records the new ones, and returns the pids that it collected.
func (r *reaper) reap(now time.Time) ([]int, error) {
	zombies, err := zombieChildren(r.self)
	if err != nil {
		return nil, err
	}
	current := make(map[int]bool, len(zombies))
	var reaped []int
	for _, pid := range zombies {
		current[pid] = true
		first, ok := r.seen[pid]
		if !ok {
			r.seen[pid] = now
			continue
		}
		if now.Sub(first) < reapGrace {
			continue
		}
		var ws unix.WaitStatus
		got, err := unix.Wait4(pid, &ws, unix.WNOHANG, nil)
		if err == nil && got == pid {
			reaped = append(reaped, pid)
		}
		delete(r.seen, pid)
		delete(current, pid)
	}
	for pid := range r.seen {
		if !current[pid] {
			delete(r.seen, pid)
		}
	}
	return reaped, nil
}

// zombieChildren lists the children of parent that are zombies.
func zombieChildren(parent int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("ax: list processes: %w", err)
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		state, ppid, ok := parseStat(b)
		if ok && state == 'Z' && ppid == parent {
			out = append(out, pid)
		}
	}
	return out, nil
}

// parseStat returns the state and the parent pid from /proc/<pid>/stat. The
// command name sits in parentheses and can hold spaces and parentheses, so
// the fields start after the last ')'.
func parseStat(b []byte) (state byte, ppid int, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+1 >= len(b) {
		return 0, 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 2 || len(f[0]) != 1 {
		return 0, 0, false
	}
	p, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, false
	}
	return f[0][0], p, true
}
