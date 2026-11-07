// fairshare runs AI agents' heavy work (tests, builds, typechecks, browser checks) so it only
// uses what the foreground leaves over, slows down evenly instead of freezing the PC, and is
// computed once when many sessions run the same check on the same code.
package main

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/Noisemaker111/fairshare/internal/windows"
)

const usage = `fairshare: agent work that yields to you.

  fairshare run [flags] -- command args...   run a command (and everything it starts) as agent work
  fairshare run --cache -- bun run test      ...and run it once for every session on the same code
  fairshare adopt                            make every chat's work fair (T3 Code sessions of any provider, terminal agents)
  fairshare throttle [<pid> <percent|off>]   list the chats adopt holds, or cap one chat's CPU
  fairshare probe [--secs 10]                measure how a game-like foreground app is doing now
  fairshare bench [flags] [-- run flags]     N heavy synthetic jobs against the probe
  fairshare bench lanes [flags]              does sharing by chat stop a 40-thread chat starving a 2-thread one
  fairshare ceiling [flags]                  how many idle processes / spawns the PC takes
  fairshare du <folders...>                  real disk use, counting hardlinked files once
  fairshare inspect                          print the priorities Windows gave this process
  fairshare key [-- command]                 print the cache key for this folder and command

The thread shell (every agent tool call goes through it):
  fairshare install [--uninstall] [--hooks-only]  hook in Claude Code, Codex and OpenCode; adopt always on
  fairshare hook claude|codex|opencode       one PreToolUse payload on stdin -> pass, rewrite or send back
  fairshare exec <bash|pwsh> <b64> <ms>      run a long test/build/install with a summary and early stops
  fairshare slot <command> [args...]         measure a command; one on the heavy list first waits for a slot (2 per PC)
  fairshare lease boat [--label <what>]      hold one boat machine lease until stdin closes (BOAT_MAX_MACHINES, 2 per PC)
  fairshare lease boat -- <command>          run a boat command holding one lease
  fairshare await <file> [maxS] [quietS]     wait for a Claude background task's output to finish
  fairshare report [--hours 24]              what every chat ran, waited on and wasted, from the event log

Run "fairshare <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var code int
	switch os.Args[1] {
	case "run":
		code = cmdRun(args)
	case "probe":
		code = cmdProbe(args)
	case "bench":
		if len(args) > 0 && args[0] == "lanes" {
			code = benchLanes(args[1:])
		} else {
			code = cmdBench(args)
		}
	case "throttle":
		code = cmdThrottle(args)
	case "spin": // busy work for bench lanes; not for people
		code = cmdSpin(args)
	case "ceiling":
		code = cmdCeiling(args)
	case "adopt":
		code = cmdAdopt(args)
	case "du":
		code = cmdDu(args)
	case "thaw-guard": // started by adopt; not for people
		var owner uint32
		if len(args) == 2 {
			fmt.Sscan(args[0], &owner)
			code = thawGuard(owner, args[1])
		}
	case "hook":
		code = cmdHook(args)
	case "exec":
		code = cmdExec(args)
	case "slot":
		code = cmdSlot(args)
	case "lease":
		code = cmdLease(args)
	case "await":
		code = cmdAwait(args)
	case "install":
		code = cmdInstall(args)
	case "report":
		code = cmdReport(args)
	case "decide": // the rules alone, for checks: NDJSON calls on stdin -> NDJSON decisions
		code = cmdDecide()
	case "inspect":
		code = cmdInspect()
	case "key":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		key, err := CacheKey(".", args, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		} else {
			fmt.Println(key)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		code = 2
	}
	os.Exit(code)
}

var (
	ntQueryInformationProcess = ntdll.NewProc("NtQueryInformationProcess")
	getProcessInformation     = kernel32.NewProc("GetProcessInformation")
)

func cmdInspect() int {
	pid := windows.GetCurrentProcessId()
	var io, mem uint32
	ntQueryInformationProcess.Call(uintptr(windows.CurrentProcess()), processIoPriority, uintptr(unsafe.Pointer(&io)), 4, 0)
	getProcessInformation.Call(uintptr(windows.CurrentProcess()), processMemoryPriority, uintptr(unsafe.Pointer(&mem)), 4)
	desktop, _ := windows.ChildDesktop()
	fmt.Printf("pid=%d priority=%s io=%d mem=%d desktop=%s\n", pid, priorityName(pid), io, mem, desktop)
	var procMask, sysMask uintptr
	kernel32.NewProc("GetProcessAffinityMask").Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&procMask)), uintptr(unsafe.Pointer(&sysMask)))
	fmt.Printf("affinity=%#x of %#x\n", procMask, sysMask)
	if !inJob(windows.CurrentProcess(), 0) {
		fmt.Println("job: none")
		return 0
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	var rate jobCPURate
	windows.QueryInformationJobObject(0, jobObjectCPURateControlInformation, uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate)), nil)
	fmt.Printf("job: limits=%#x affinity=%#x priority=%d cpu_rate_flags=%#x cpu_rate=%d active_process_limit=%d job_memory_limit_mb=%d\n",
		info.BasicLimitInformation.LimitFlags, info.BasicLimitInformation.Affinity, info.BasicLimitInformation.PriorityClass,
		rate.ControlFlags, rate.CPURate, info.BasicLimitInformation.ActiveProcessLimit, info.JobMemoryLimit>>20)
	return 0
}
