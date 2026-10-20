//go:build windows

package main

// usage_windows.go — how much of the computer Chriscord is using, for
// Settings → About: memory, CPU and GPU, added up over this app and every
// process it started (the WebView2 processes that draw its window, an
// ffmpeg it runs…), measured the way Task Manager measures them:
//
//   - memory: the private working set (Task Manager's "Memory" column);
//   - CPU: share of all the computer's cores since the last reading;
//   - GPU: the "GPU Engine" performance counters Task Manager reads, its
//     busiest engine (3D, video decode, copy…), as Task Manager shows it.
//
// Nothing runs in the background: each GetUsage call takes a reading and
// compares it with the last one, so the page calls it every couple of
// seconds while About is open.

import (
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const usageSupported = true

var (
	modKernel32Usage = syscall.NewLazyDLL("kernel32.dll")
	modPdh           = syscall.NewLazyDLL("pdh.dll")

	pCreateToolhelp32Snapshot = modKernel32Usage.NewProc("CreateToolhelp32Snapshot")
	pProcess32FirstW          = modKernel32Usage.NewProc("Process32FirstW")
	pProcess32NextW           = modKernel32Usage.NewProc("Process32NextW")
	pGetProcessTimes          = modKernel32Usage.NewProc("GetProcessTimes")
	pK32GetProcessMemoryInfo  = modKernel32Usage.NewProc("K32GetProcessMemoryInfo")

	pPdhOpenQueryW                = modPdh.NewProc("PdhOpenQueryW")
	pPdhAddEnglishCounterW        = modPdh.NewProc("PdhAddEnglishCounterW")
	pPdhCollectQueryData          = modPdh.NewProc("PdhCollectQueryData")
	pPdhGetFormattedCounterArrayW = modPdh.NewProc("PdhGetFormattedCounterArrayW")
)

const (
	th32csSnapProcess              = 0x2
	processQueryLimitedInformation = 0x1000
	pdhFmtDouble                   = 0x200
	pdhFmtNoCap100                 = 0x8000
	pdhMoreData                    = 0x800007D2
)

type processEntry32 struct {
	size          uint32
	usage         uint32
	pid           uint32
	defaultHeapID uintptr
	moduleID      uint32
	threads       uint32
	parentPID     uint32
	priClassBase  int32
	flags         uint32
	exeFile       [260]uint16
}

// processMemoryCountersEx2: PROCESS_MEMORY_COUNTERS_EX2 (Windows 10 21H2+);
// older Windows answers to the shorter PROCESS_MEMORY_COUNTERS_EX.
type processMemoryCountersEx2 struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
	privateWorkingSetSize      uintptr
	sharedCommitUsage          uint64
}

// UsagePart is one kind of Chriscord's processes.
type UsagePart struct {
	Name      string `json:"name"`
	Processes int    `json:"processes"`
	Memory    uint64 `json:"memory"` // bytes
}

// Usage is what Chriscord is using right now.
type Usage struct {
	Supported bool        `json:"supported"`
	Memory    uint64      `json:"memory"`    // bytes, all its processes
	CPU       *float64    `json:"cpu"`       // percent of the whole computer; nil on the first reading
	GPU       *float64    `json:"gpu"`       // percent of the busiest GPU engine; nil when unknown
	Parts     []UsagePart `json:"parts"`     // the app, its window (WebView2), anything else it runs
	Processes int         `json:"processes"` // how many in all
}

type usageSampler struct {
	mu       sync.Mutex
	lastAt   time.Time
	lastCPU  map[uint32]uint64 // pid → kernel+user time (100 ns) at the last reading
	gpuQuery uintptr
	gpuCount uintptr
	gpuReady bool // collected once: the next collection gives rates
	gpuOff   bool // the counters aren't there (old Windows, no GPU driver support)
}

var usage usageSampler

// GetUsage is Chriscord's memory, CPU and GPU use now (Settings → About).
func (a *App) GetUsage() Usage { return usage.sample() }

type procInfo struct {
	pid, parent uint32
	exe         string
}

// ourProcesses: this process and everything it started, and their
// starters' — by parent process id, checking each one started after its
// parent (a parent id can be an old, reused one).
func ourProcesses() []procInfo {
	snap, _, _ := pCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == ^uintptr(0) {
		return nil
	}
	defer syscall.CloseHandle(syscall.Handle(snap))
	var all []procInfo
	var e processEntry32
	e.size = uint32(unsafe.Sizeof(e))
	for ok, _, _ := pProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&e))); ok != 0; ok, _, _ = pProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&e))) {
		all = append(all, procInfo{pid: e.pid, parent: e.parentPID, exe: strings.ToLower(syscall.UTF16ToString(e.exeFile[:]))})
	}
	children := map[uint32][]procInfo{}
	for _, p := range all {
		if p.pid != p.parent {
			children[p.parent] = append(children[p.parent], p)
		}
	}
	self := uint32(syscall.Getpid())
	out := []procInfo{{pid: self, exe: "self"}}
	created := map[uint32]uint64{self: creationTime(self)}
	for i := 0; i < len(out) && len(out) < 256; i++ {
		parent := out[i]
		for _, c := range children[parent.pid] {
			ct := creationTime(c.pid)
			if ct == 0 || ct < created[parent.pid] {
				continue // gone, or older than its "parent": not one of ours
			}
			created[c.pid] = ct
			out = append(out, c)
		}
	}
	return out
}

func openQuery(pid uint32) syscall.Handle {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return 0
	}
	return h
}

func ftU64(f syscall.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

func creationTime(pid uint32) uint64 {
	h := openQuery(pid)
	if h == 0 {
		return 0
	}
	defer syscall.CloseHandle(h)
	var c, x, k, u syscall.Filetime
	if r, _, _ := pGetProcessTimes.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(unsafe.Pointer(&x)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u))); r == 0 {
		return 0
	}
	return ftU64(c)
}

// procUsage: a process's private working set (bytes) and CPU time so far
// (100 ns units).
func procUsage(pid uint32) (mem, cpu uint64, ok bool) {
	h := openQuery(pid)
	if h == 0 {
		return 0, 0, false
	}
	defer syscall.CloseHandle(h)
	var c, x, k, u syscall.Filetime
	if r, _, _ := pGetProcessTimes.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(unsafe.Pointer(&x)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u))); r != 0 {
		cpu = ftU64(k) + ftU64(u)
	}
	var m processMemoryCountersEx2
	m.cb = uint32(unsafe.Sizeof(m))
	if r, _, _ := pK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.cb)); r != 0 {
		mem = uint64(m.privateWorkingSetSize)
	} else {
		// Older Windows: PROCESS_MEMORY_COUNTERS_EX, without the private
		// working set — its private bytes are the nearest.
		m = processMemoryCountersEx2{}
		m.cb = uint32(unsafe.Offsetof(m.privateWorkingSetSize))
		if r, _, _ := pK32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.cb)); r != 0 {
			mem = uint64(m.privateUsage)
		}
	}
	return mem, cpu, true
}

func (s *usageSampler) sample() Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	procs := ourProcesses()
	out := Usage{Supported: true}
	parts := map[string]*UsagePart{}
	order := []string{"Chriscord", "Window (WebView2)", "Other"}
	for _, n := range order {
		parts[n] = &UsagePart{Name: n}
	}
	cpuNow := map[uint32]uint64{}
	var cpuDelta uint64
	pids := map[uint32]bool{}
	for _, p := range procs {
		mem, cpu, ok := procUsage(p.pid)
		if !ok {
			continue
		}
		pids[p.pid] = true
		kind := "Other"
		switch {
		case p.exe == "self":
			kind = "Chriscord"
		case strings.HasPrefix(p.exe, "msedgewebview2"):
			kind = "Window (WebView2)"
		}
		parts[kind].Processes++
		parts[kind].Memory += mem
		out.Memory += mem
		out.Processes++
		cpuNow[p.pid] = cpu
		// Only what a process used since it was last seen: one seen for
		// the first time counts from the next reading.
		if last, seen := s.lastCPU[p.pid]; seen && cpu >= last {
			cpuDelta += cpu - last
		}
	}
	if s.lastCPU != nil && !s.lastAt.IsZero() {
		wall := now.Sub(s.lastAt)
		if wall > 0 {
			pct := float64(cpuDelta) * 100 / (float64(wall/100) * float64(goruntime.NumCPU()))
			pct = min(max(pct, 0), 100)
			out.CPU = &pct
		}
	}
	s.lastCPU, s.lastAt = cpuNow, now
	out.GPU = s.gpu(pids)
	for _, n := range order {
		if parts[n].Processes > 0 {
			out.Parts = append(out.Parts, *parts[n])
		}
	}
	return out
}

// gpu: the busiest GPU engine's use by these processes, in percent, as
// Task Manager's GPU column (nil until there are two collections, or
// without the counters).
func (s *usageSampler) gpu(pids map[uint32]bool) *float64 {
	if s.gpuOff || unsafe.Sizeof(uintptr(0)) != 8 {
		return nil
	}
	if s.gpuQuery == 0 {
		if err := pPdhOpenQueryW.Find(); err != nil {
			s.gpuOff = true
			return nil
		}
		var q uintptr
		if r, _, _ := pPdhOpenQueryW.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
			s.gpuOff = true
			return nil
		}
		path, _ := syscall.UTF16PtrFromString(`\GPU Engine(*)\Utilization Percentage`)
		var c uintptr
		if r, _, _ := pPdhAddEnglishCounterW.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
			s.gpuOff = true
			return nil
		}
		s.gpuQuery, s.gpuCount = q, c
	}
	if r, _, _ := pPdhCollectQueryData.Call(s.gpuQuery); r != 0 {
		return nil
	}
	if !s.gpuReady {
		s.gpuReady = true // a rate needs two collections
		return nil
	}
	var size, count uint32
	r, _, _ := pPdhGetFormattedCounterArrayW.Call(s.gpuCount, pdhFmtDouble|pdhFmtNoCap100, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if uint32(r) != pdhMoreData || size == 0 {
		zero := 0.0
		return &zero // no engines in use at all
	}
	buf := make([]byte, size)
	if r, _, _ := pPdhGetFormattedCounterArrayW.Call(s.gpuCount, pdhFmtDouble|pdhFmtNoCap100, uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return nil
	}
	// PDH_FMT_COUNTERVALUE_ITEM_W on 64-bit Windows: the name's pointer,
	// then {CStatus, padding, double} — 24 bytes each.
	type item struct {
		name   *uint16
		status uint32
		_      uint32
		value  float64
	}
	items := unsafe.Slice((*item)(unsafe.Pointer(&buf[0])), count)
	engines := map[string]float64{} // one engine (adapter, engine number) → our use of it
	for _, it := range items {
		if it.status != 0 || it.name == nil {
			continue
		}
		name := utf16PtrToString(it.name) // pid_1234_luid_0x…_0x…_phys_0_eng_3_engtype_3D
		pidPart, rest, ok := strings.Cut(strings.TrimPrefix(name, "pid_"), "_")
		if !ok || !strings.HasPrefix(name, "pid_") {
			continue
		}
		pid, err := strconv.ParseUint(pidPart, 10, 32)
		if err != nil || !pids[uint32(pid)] {
			continue
		}
		engine := rest
		if i := strings.Index(rest, "_engtype_"); i >= 0 {
			engine = rest[:i]
		}
		engines[engine] += it.value
	}
	busiest := 0.0
	for _, v := range engines {
		busiest = max(busiest, v)
	}
	busiest = min(busiest, 100)
	return &busiest
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var s []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 {
			break
		}
		s = append(s, c)
		if len(s) > 1024 {
			break
		}
	}
	return syscall.UTF16ToString(s)
}
