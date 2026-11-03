package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ntdll                   = windows.NewLazySystemDLL("ntdll.dll")
	ntSetInformationProcess = ntdll.NewProc("NtSetInformationProcess")
	ntSuspendProcess        = ntdll.NewProc("NtSuspendProcess")
	ntResumeProcess         = ntdll.NewProc("NtResumeProcess")
	kernel32                = windows.NewLazySystemDLL("kernel32.dll")
	setProcessInformation   = kernel32.NewProc("SetProcessInformation")
	globalMemoryStatusEx    = kernel32.NewProc("GlobalMemoryStatusEx")
	isProcessInJobProc      = kernel32.NewProc("IsProcessInJob")
)

const (
	processIoPriority                  = 33 // NtSetInformationProcess class
	processMemoryPriority              = 0  // SetProcessInformation class
	jobObjectAssociateCompletionPort   = 7
	jobObjectCPURateControlInformation = 15
	jobMsgActiveProcessZero            = 4
	jobMsgNewProcess                   = 6
)

// Policy is what every process in a piece of agent work gets. Windows enforces the priority class
// on the whole process tree through the job, so a child cannot raise itself back to normal.
type Policy struct {
	PriorityClass uint32 // windows.IDLE_PRIORITY_CLASS etc.; 0 leaves it alone
	IOPriority    int    // 0 very low, 1 low, 2 normal; -1 leaves it alone
	MemPriority   int    // 1 very low .. 5 normal; -1 leaves it alone
	JobMemoryMB   uint64 // hard commit limit for one piece of work; 0 for none
	// Memory backpressure, off unless asked for (0 turns it off): when free RAM or free commit falls
	// below FreezeBelowMB, the youngest work on the PC freezes (its processes are suspended), one
	// piece at a time, until both are back above ResumeAboveMB. The oldest piece of work never
	// freezes, so work always drains.
	FreezeBelowMB uint64
	ResumeAboveMB uint64
}

// The memory lines --freeze uses. Resume is close to the freeze line: one finished job should thaw
// the next.
const (
	freezeBelowMB = 6 << 10
	resumeAboveMB = 7<<10 + 512
)

// DefaultPolicy is "agent work only uses what the foreground leaves over": it lowers priority and
// never suspends anything. Freezing is opt-in (--freeze): a frozen chat looks hung to its user.
func DefaultPolicy() Policy {
	return Policy{
		PriorityClass: windows.IDLE_PRIORITY_CLASS,
		IOPriority:    0,
		MemPriority:   1,
	}
}

// withFreezing turns on memory backpressure at the given lines.
func (p Policy) withFreezing(below, above uint64) Policy {
	p.FreezeBelowMB, p.ResumeAboveMB = below, above
	return p
}

type jobCPURate struct {
	ControlFlags uint32
	CPURate      uint32
}

const (
	cpuRateEnable      = 0x1
	cpuRateWeightBased = 0x2
	cpuRateHardCap     = 0x4
)

// setCPUWeight makes Windows share the CPU between jobs by weight (1-9) when they compete,
// instead of thread by thread: a job with 40 busy threads then gets no more than a job with 2.
func setCPUWeight(job windows.Handle, weight uint32) error {
	r := jobCPURate{ControlFlags: cpuRateEnable | cpuRateWeightBased, CPURate: weight}
	_, err := windows.SetInformationJobObject(job, jobObjectCPURateControlInformation, uintptr(unsafe.Pointer(&r)), uint32(unsafe.Sizeof(r)))
	return err
}

// setCPUCap holds a job to pct percent of the whole PC's CPU (0 removes the cap).
func setCPUCap(job windows.Handle, pct float64) error {
	r := jobCPURate{}
	if pct > 0 {
		r = jobCPURate{ControlFlags: cpuRateEnable | cpuRateHardCap, CPURate: uint32(max(1, min(10000, pct*100)))}
	}
	_, err := windows.SetInformationJobObject(job, jobObjectCPURateControlInformation, uintptr(unsafe.Pointer(&r)), uint32(unsafe.Sizeof(r)))
	return err
}

type jobAssociateCompletionPort struct {
	CompletionKey  uintptr
	CompletionPort windows.Handle
}

// Unit is one piece of agent work: a job and everything in it.
type Unit struct {
	ID      uintptr
	Job     windows.Handle
	Skip    uint32 // a process in the job never to freeze (fairshare itself, for run)
	Started time.Time
	Frozen     bool
	reg        windows.Handle
	regName    string
	frozenMark windows.Handle
	frozenPIDs []uint32
}

// Governor owns the units of this process: it tunes every process that enters one of their jobs,
// and freezes and thaws them under memory pressure together with every other fairshare on the PC.
type Governor struct {
	policy   Policy
	port     windows.Handle
	mu       sync.Mutex
	units    map[uintptr]*Unit
	nextID   uintptr
	OnEmpty  func(*Unit) // called when the last process in a unit's job has exited
	Tuned    atomic.Int64
	TuneErrs atomic.Int64
	Freezes  atomic.Int64
	sensor   *WakeSensor
	// FrozenFile, when set, always lists the processes this governor holds frozen (see thawGuard).
	FrozenFile string
}

// NewGovernor starts governing. With a sensor, work also freezes while the foreground is held up,
// not only while memory is short; the sensor must run at normal priority, so only a governor
// outside any agent job (adopt) can have one.
func NewGovernor(p Policy, sensor *WakeSensor) (*Governor, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("create job port: %w", err)
	}
	g := &Governor{policy: p, port: port, units: map[uintptr]*Unit{}, sensor: sensor}
	go g.watch()
	if p.FreezeBelowMB > 0 || sensor != nil {
		os.MkdirAll(runsDir(), 0o755)
		go g.backpressure()
	}
	return g, nil
}

// NewUnit creates a job under the policy. killOnClose makes Windows kill everything in it when
// this process ends; adopt leaves it off so work it adopted survives fairshare stopping. A name
// lets other programs (fairshare throttle) open the job; "" leaves it unnamed.
func (g *Governor) NewUnit(killOnClose bool, name string) (*Unit, error) {
	var namePtr *uint16
	if name != "" {
		namePtr, _ = windows.UTF16PtrFromString(name)
	}
	job, err := windows.CreateJobObject(nil, namePtr)
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	if killOnClose {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	if g.policy.PriorityClass != 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PRIORITY_CLASS
		info.BasicLimitInformation.PriorityClass = g.policy.PriorityClass
	}
	if g.policy.JobMemoryMB > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(g.policy.JobMemoryMB << 20)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("set job limits: %w", err)
	}
	g.mu.Lock()
	g.nextID++
	u := &Unit{ID: g.nextID, Job: job, Started: time.Now()}
	g.units[u.ID] = u
	g.mu.Unlock()
	assoc := jobAssociateCompletionPort{CompletionKey: u.ID, CompletionPort: g.port}
	if _, err := windows.SetInformationJobObject(job, jobObjectAssociateCompletionPort, uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc))); err != nil {
		g.Close(u)
		return nil, fmt.Errorf("watch job: %w", err)
	}
	if g.policy.FreezeBelowMB > 0 || g.sensor != nil {
		if err := g.register(u); err != nil {
			g.Close(u)
			return nil, err
		}
	}
	return u, nil
}

// Close forgets a unit: it is thawed first, so nothing is ever left frozen.
func (g *Governor) Close(u *Unit) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.units[u.ID]; !ok {
		return
	}
	g.setFrozen(u, false)
	delete(g.units, u.ID)
	if u.reg != 0 {
		windows.CloseHandle(u.reg)
	}
	windows.CloseHandle(u.Job)
}

func (g *Governor) watch() {
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		if err := windows.GetQueuedCompletionStatus(g.port, &msg, &key, &ov, windows.INFINITE); err != nil {
			continue
		}
		switch msg {
		case jobMsgNewProcess:
			g.tune(uint32(uintptr(unsafe.Pointer(ov))))
		case jobMsgActiveProcessZero:
			g.mu.Lock()
			u := g.units[key]
			g.mu.Unlock()
			if u != nil && g.OnEmpty != nil {
				g.OnEmpty(u)
			}
		}
	}
}

// tune applies the parts of the policy a job cannot enforce by itself: disk and memory priority.
func (g *Governor) tune(pid uint32) {
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		g.TuneErrs.Add(1)
		return
	}
	defer windows.CloseHandle(h)
	ok := true
	if g.policy.IOPriority >= 0 {
		v := uint32(g.policy.IOPriority)
		if st, _, _ := ntSetInformationProcess.Call(uintptr(h), processIoPriority, uintptr(unsafe.Pointer(&v)), 4); st != 0 {
			ok = false
		}
	}
	if g.policy.MemPriority >= 0 {
		v := uint32(g.policy.MemPriority)
		if res, _, _ := setProcessInformation.Call(uintptr(h), processMemoryPriority, uintptr(unsafe.Pointer(&v)), 4); res == 0 {
			ok = false
		}
	}
	if ok {
		g.Tuned.Add(1)
	} else {
		g.TuneErrs.Add(1)
	}
}

func runsDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "fairshare", "units")
}

// register leaves a file named by the unit's start time, open without delete sharing and deleted
// by Windows when its handle closes (also when this process dies). So the folder always lists
// exactly the live units on the PC, oldest first.
func (g *Governor) register(u *Unit) error {
	u.regName = fmt.Sprintf("%020d-%d-%d", u.Started.UnixNano(), windows.GetCurrentProcessId(), u.ID)
	p, _ := windows.UTF16PtrFromString(filepath.Join(runsDir(), u.regName))
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE|windows.DELETE, windows.FILE_SHARE_READ, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
	if err != nil {
		return fmt.Errorf("register unit: %w", err)
	}
	u.reg = h
	return nil
}

const frozenSuffix = ".frozen"

// unitsOnPC reads the registry: live units oldest first, and which of them are frozen.
func unitsOnPC() (live []string, frozen map[string]bool) {
	entries, _ := os.ReadDir(runsDir()) // sorted by name, and names start with the start time
	frozen = map[string]bool{}
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), frozenSuffix); ok {
			frozen[name] = true
		} else {
			live = append(live, e.Name())
		}
	}
	return live, frozen
}

func (g *Governor) setFrozen(u *Unit, freeze bool) {
	if freeze == u.Frozen {
		return
	}
	if freeze {
		p, _ := windows.UTF16PtrFromString(filepath.Join(runsDir(), u.regName+frozenSuffix))
		h, err := windows.CreateFile(p, windows.GENERIC_WRITE|windows.DELETE, windows.FILE_SHARE_READ, nil, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_TEMPORARY|windows.FILE_FLAG_DELETE_ON_CLOSE, 0)
		if err == nil {
			u.frozenMark = h
		}
		u.frozenPIDs = freezeJob(u.Job, true, u.Skip)
		g.Freezes.Add(1)
	} else {
		freezeJob(u.Job, false, u.Skip)
		u.frozenPIDs = nil
		if u.frozenMark != 0 {
			windows.CloseHandle(u.frozenMark)
			u.frozenMark = 0
		}
	}
	u.Frozen = freeze
	if g.FrozenFile != "" {
		g.writeFrozen()
	}
}

// writeFrozen records which processes this governor holds frozen, so the thaw guard can let them
// go if this process dies without thawing them. Called with g.mu held.
func (g *Governor) writeFrozen() {
	var b strings.Builder
	for _, u := range g.units {
		for _, pid := range u.frozenPIDs {
			fmt.Fprintf(&b, "%d %d\n", pid, creationTime(pid))
		}
	}
	tmp := g.FrozenFile + ".tmp"
	if os.WriteFile(tmp, []byte(b.String()), 0o644) == nil {
		os.Rename(tmp, g.FrozenFile)
	}
}

// freezeMemStatus is the memory reading backpressure acts on (tests make memory look short).
var freezeMemStatus = MemStatus

// backpressure acts for the whole PC one step at a time: while memory is short, the youngest
// running unit anywhere freezes; once memory is back, the oldest frozen unit thaws. The oldest
// unit never freezes, so work always drains. Each fairshare process only ever touches its own
// units, and only when the step is theirs. Freezing instead of killing means memory pressure
// slows work down rather than losing it.
func (g *Governor) backpressure() {
	calmTicks := 0
	for {
		time.Sleep(200 * time.Millisecond)
		short, plenty := false, true
		if g.policy.FreezeBelowMB > 0 {
			phys, commit, _ := freezeMemStatus()
			low := min(phys, commit)
			short, plenty = low < g.policy.FreezeBelowMB, low > g.policy.ResumeAboveMB
		}
		if g.sensor != nil {
			lagging, calm := g.sensor.State()
			if calm {
				calmTicks++
			} else {
				calmTicks = 0
			}
			// freeze fast while the foreground is held up; thaw slowly, one per second of calm
			short = short || lagging
			plenty = plenty && calmTicks >= 5
			if plenty {
				calmTicks = 0
			}
		}
		g.mu.Lock()
		frozenHere := false
		mine := map[string]*Unit{}
		for _, u := range g.units {
			mine[u.regName] = u
			frozenHere = frozenHere || u.Frozen
		}
		if !short && !frozenHere {
			g.mu.Unlock()
			continue
		}
		live, frozen := unitsOnPC()
		if len(live) > 0 {
			if u := mine[live[0]]; u != nil {
				g.setFrozen(u, false) // the oldest always runs
			}
		}
		switch {
		case short:
			for _, name := range slices.Backward(live[min(1, len(live)):]) {
				if !frozen[name] {
					if u := mine[name]; u != nil {
						g.setFrozen(u, true)
					}
					break
				}
			}
		case plenty:
			for _, name := range live {
				if frozen[name] {
					if u := mine[name]; u != nil {
						g.setFrozen(u, false)
					}
					break
				}
			}
		}
		g.mu.Unlock()
	}
}

// jobPIDs lists the processes in job right now, including those in jobs nested under it.
func jobPIDs(job windows.Handle) []uint32 {
	var list struct {
		Assigned uint32
		Listed   uint32
		IDs      [8192]uintptr
	}
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicProcessIdList, uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil); err != nil {
		if os.Getenv("FAIRSHARE_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "  list job processes:", err)
		}
		return nil
	}
	out := make([]uint32, 0, list.Listed)
	for _, id := range list.IDs[:list.Listed] {
		out = append(out, uint32(id))
	}
	return out
}

// jobCPUSeconds is the user+kernel CPU time everything in job has used, which load does not
// inflate the way it inflates wall time.
func jobCPUSeconds(job windows.Handle) float64 {
	var acct struct {
		TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
		TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
	}
	if err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil); err != nil {
		return 0
	}
	return float64(acct.TotalUserTime+acct.TotalKernelTime) / 1e7
}

// descendants lists every process below root by parent link. A parent link is only followed when
// the child started after the parent, so a reused process ID never pulls in a stranger.
func descendants(procs map[uint32]procInfo, root uint32) []uint32 {
	children := map[uint32][]uint32{}
	for pid, p := range procs {
		if pid != p.ppid {
			children[p.ppid] = append(children[p.ppid], pid)
		}
	}
	var out []uint32
	queue := []uint32{root}
	created := map[uint32]uint64{root: procs[root].created}
	if created[root] == 0 {
		created[root] = creationTime(root)
	}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, c := range children[parent] {
			t := procs[c].created
			if t == 0 || t < created[parent] {
				continue
			}
			created[c] = t
			out = append(out, c)
			queue = append(queue, c)
		}
	}
	return out
}

// pullIntoJobs puts every process below root that is in none of the set's jobs into one of them.
// Windows starts some programs outside their parent's job: Microsoft Store apps such as the Store
// build of PowerShell break away by default, so without this their work is neither governed,
// counted nor stopped. Any that refuse even an empty job are returned, so callers can still count
// and stop them by parent link.
func pullIntoJobs(s *jobSet, root uint32, tried map[uint32]bool) (pulled, sided int, outside []uint32, why error) {
	for _, pid := range descendants(snapshot(), root) {
		if tried[pid] {
			outside = append(outside, pid)
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		if !s.has(h) {
			switch side, err := s.place(h); {
			case err != nil:
				tried[pid] = true
				outside = append(outside, pid)
				why = err
			case side != 0:
				sided++
			default:
				pulled++
			}
		}
		windows.CloseHandle(h)
	}
	return pulled, sided, outside, why
}

// JobStats is everything Windows counts for a job, over its whole life.
type JobStats struct {
	CPUSeconds      float64
	Processes       uint32 // every process that was ever in it
	ReadMB, WriteMB float64
	AllIOMB         float64 // reads, writes and other I/O (network included)
	PeakMB          float64 // most memory the whole job had committed at once
}

func jobStats(job windows.Handle) JobStats {
	var acct struct {
		TotalUserTime, TotalKernelTime, ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
		TotalPageFaultCount, TotalProcesses, ActiveProcesses, TotalTerminatedProcesses     uint32
		IO                                                                                 windows.IO_COUNTERS
	}
	const jobObjectBasicAndIoAccountingInformation = 8
	var s JobStats
	if windows.QueryInformationJobObject(job, jobObjectBasicAndIoAccountingInformation, uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil) == nil {
		s.CPUSeconds = float64(acct.TotalUserTime+acct.TotalKernelTime) / 1e7
		s.Processes = acct.TotalProcesses
		s.ReadMB, s.WriteMB = float64(acct.IO.ReadTransferCount)/(1<<20), float64(acct.IO.WriteTransferCount)/(1<<20)
		s.AllIOMB = s.ReadMB + s.WriteMB + float64(acct.IO.OtherTransferCount)/(1<<20)
	}
	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if windows.QueryInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil) == nil {
		s.PeakMB = float64(ext.PeakJobMemoryUsed) / (1 << 20)
	}
	return s
}

// freezeJob suspends (or resumes) every process in job except skip, and returns the ones it did.
func freezeJob(job windows.Handle, freeze bool, skip uint32) []uint32 {
	call := ntResumeProcess
	if freeze {
		call = ntSuspendProcess
	}
	var done []uint32
	for _, pid := range jobPIDs(job) {
		if pid == skip {
			continue
		}
		if setSuspended(pid, call) {
			done = append(done, pid)
		}
	}
	return done
}

func setSuspended(pid uint32, call interface{ Call(...uintptr) (uintptr, uintptr, error) }) bool {
	h, err := windows.OpenProcess(windows.PROCESS_SUSPEND_RESUME, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	st, _, _ := call.Call(uintptr(h))
	return st == 0
}

// thawGuard waits for process owner to end, then resumes every process listed in frozenFile that
// is still the same process (same creation time). It runs as its own small process, so work that
// adopt froze is never left frozen if adopt is killed.
func thawGuard(owner uint32, frozenFile string) int {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, owner)
	if err == nil {
		windows.WaitForSingleObject(h, windows.INFINITE)
		windows.CloseHandle(h)
	}
	b, err := os.ReadFile(frozenFile)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		var pid uint32
		var created uint64
		if n, _ := fmt.Sscan(line, &pid, &created); n == 2 && creationTime(pid) == created {
			setSuspended(pid, ntResumeProcess)
		}
	}
	os.Remove(frozenFile)
	return 0
}

func inJob(process, job windows.Handle) bool {
	var member int32
	isProcessInJobProc.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&member)))
	return member != 0
}

// MemStatus is free physical memory and free commit (what can still be allocated before the PC
// runs out of RAM plus page file), in MB.
func MemStatus() (availPhysMB, availCommitMB, loadPct uint64) {
	var m struct {
		Length               uint32
		MemoryLoad           uint32
		TotalPhys, AvailPhys uint64
		TotalPageFile        uint64
		AvailPageFile        uint64
		TotalVirtual         uint64
		AvailVirtual         uint64
		AvailExtendedVirtual uint64
	}
	m.Length = uint32(unsafe.Sizeof(m))
	globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	return m.AvailPhys >> 20, m.AvailPageFile >> 20, uint64(m.MemoryLoad)
}
