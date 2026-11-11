//go:build linux && (amd64 || arm64)

package media

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

func init() { toolMainHook = fakeTool }

// fakeTool is the test binary playing a media tool. Each command is one
// behaviour the sandbox must handle.
func fakeTool(args []string) {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(70)
	}
	switch args[0] {
	case "environ":
		for _, e := range os.Environ() {
			fmt.Println("env", e)
		}
		for _, a := range os.Args[1:] {
			fmt.Println("arg", a)
		}
	case "sockets":
		_, err1 := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		_, err2 := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		fmt.Println("socket", errnoOf(err1))
		fmt.Println("socketpair", errnoOf(err2))
	case "syscalls":
		// Everything a tool does besides networking: files, pipes, memory,
		// threads and child processes.
		var pipe [2]int
		check(syscall.Pipe(pipe[:]))
		f, err := os.Open("/proc/self/status")
		check(err)
		_ = f.Close()
		_, err = syscall.Mmap(-1, 0, 1<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
		check(err)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() { time.Sleep(time.Millisecond) })
		}
		wg.Wait()
		check(exec.Command(self, "--tool", "noop").Run())
		fmt.Println("ok")
	case "limits":
		b, err := os.ReadFile("/proc/self/limits")
		check(err)
		_, err = os.Stdout.Write(b)
		check(err)
	case "fds":
		// fds: list every open descriptor and what it refers to.
		entries, err := os.ReadDir("/proc/self/fd")
		check(err)
		for _, e := range entries {
			target, err := os.Readlink("/proc/self/fd/" + e.Name())
			if err != nil {
				target = err.Error()
			}
			fmt.Println("fd", e.Name(), "->", target)
		}
	case "exit":
		n, _ := strconv.Atoi(args[1])
		os.Exit(n)
	case "noise":
		// noise <bytes>: write a first line, filler and a last line to
		// standard error, then fail.
		n, _ := strconv.Atoi(args[1])
		fmt.Fprintln(os.Stderr, "FIRST-LINE")
		for written := 0; written < n; written += 100 {
			fmt.Fprintln(os.Stderr, strings.Repeat("x", 99))
		}
		fmt.Fprintln(os.Stderr, "LAST-LINE")
		os.Exit(4)
	case "kill-self":
		n, _ := strconv.Atoi(args[1])
		check(syscall.Kill(os.Getpid(), syscall.Signal(n)))
		time.Sleep(time.Minute)
	case "kill-parent":
		// kill-parent <pidfile>: SIGKILL the helper, which cannot clean up.
		check(os.WriteFile(args[1], fmt.Appendf(nil, "%d", os.Getpid()), 0o600))
		check(syscall.Kill(os.Getppid(), syscall.SIGKILL))
		time.Sleep(time.Minute)
	case "tsync":
		// A thread that exists before the filter is installed must be
		// filtered too.
		other := make(chan chan int)
		started := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			close(started)
			reply := <-other
			_, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			reply <- errnoOf(err)
		}()
		<-started
		runtime.LockOSThread()
		const prSetNoNewPrivs = 38
		_, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0)
		if e != 0 {
			check(e)
		}
		check(installFilter())
		reply := make(chan int)
		other <- reply
		fmt.Println("socket", <-reply)
	case "sleep":
		time.Sleep(time.Minute)
	case "spawn":
		// spawn <marker> <ready> [hold]: start a grandchild in its own
		// session, then exit, or with "hold" keep running.
		child := exec.Command(self, "--tool", "sleep", args[1])
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		check(child.Start())
		check(os.WriteFile(args[2], fmt.Appendf(nil, "%d %d", child.Process.Pid, os.Getpid()), 0o600))
		if len(args) > 3 {
			time.Sleep(time.Minute)
		}
	case "alloc":
		// alloc <MiB> <busy ms>: touch memory and spend CPU.
		mib, _ := strconv.Atoi(args[1])
		busy, _ := strconv.Atoi(args[2])
		b := make([]byte, mib<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		// Spin on the process's own CPU time: a wall-clock deadline gives
		// less CPU than asked when other tests share the cores.
		n := 0
		for cpuTime() < time.Duration(busy)*time.Millisecond {
			n++
		}
		fmt.Println(len(b), n > 0)
	}
}

// cpuTime is the user plus system time the process has used so far.
func cpuTime() time.Duration {
	var ru syscall.Rusage
	check(syscall.Getrusage(syscall.RUSAGE_SELF, &ru))
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(71)
	}
}

func errnoOf(err error) int {
	if err == nil {
		return 0
	}
	return int(err.(syscall.Errno))
}
