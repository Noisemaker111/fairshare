// Package windows is the part of the Windows API fairshare calls: Go's syscall package plus the
// job-object, process and sync calls it lacks, with golang.org/x/sys/windows's names and
// behavior. It replaces x/sys/windows because that package imports net, whose init starts Winsock
// in every process: 3 ms of each hook call, which never opens a socket.
package windows

import (
	"strconv"
	"syscall"
	"unsafe"
)

type (
	Handle                  = syscall.Handle
	Filetime                = syscall.Filetime
	Overlapped              = syscall.Overlapped
	SecurityAttributes      = syscall.SecurityAttributes
	StartupInfo             = syscall.StartupInfo
	ProcessInformation      = syscall.ProcessInformation
	ByHandleFileInformation = syscall.ByHandleFileInformation
	Win32FileAttributeData  = syscall.Win32FileAttributeData // what os.FileInfo.Sys() returns
	LazyDLL                 = syscall.LazyDLL
	LazyProc                = syscall.LazyProc
)

const (
	InvalidHandle        = ^Handle(0)
	ERROR_ALREADY_EXISTS = syscall.ERROR_ALREADY_EXISTS
	MAX_PATH             = 260
	INFINITE             = 0xFFFFFFFF
	WAIT_OBJECT_0        = 0
	WAIT_TIMEOUT         = 0x102
	MAXIMUM_WAIT_OBJECTS = 64

	DELETE                            = 0x00010000
	SYNCHRONIZE                       = 0x00100000
	GENERIC_READ                      = 0x80000000
	GENERIC_WRITE                     = 0x40000000
	GENERIC_ALL                       = 0x10000000
	EVENT_MODIFY_STATE                = 0x0002
	PROCESS_TERMINATE                 = 0x0001
	PROCESS_SET_QUOTA                 = 0x0100
	PROCESS_CREATE_PROCESS            = 0x0080
	PROCESS_DUP_HANDLE                = 0x0040
	PROCESS_SET_INFORMATION           = 0x0200
	PROCESS_SUSPEND_RESUME            = 0x0800
	PROCESS_QUERY_LIMITED_INFORMATION = 0x1000

	FILE_SHARE_READ              = 0x00000001
	FILE_SHARE_WRITE             = 0x00000002
	FILE_SHARE_DELETE            = 0x00000004
	CREATE_NEW                   = 1
	CREATE_ALWAYS                = 2
	OPEN_EXISTING                = 3
	OPEN_ALWAYS                  = 4
	FILE_ATTRIBUTE_TEMPORARY     = 0x00000100
	FILE_ATTRIBUTE_NORMAL        = 0x00000080
	FILE_ATTRIBUTE_REPARSE_POINT = 0x00000400
	FILE_FLAG_DELETE_ON_CLOSE    = 0x04000000
	FILE_FLAG_BACKUP_SEMANTICS   = 0x02000000
	FILE_FLAG_OPEN_REPARSE_POINT = 0x00200000
	LOCKFILE_FAIL_IMMEDIATELY    = 0x00000001
	LOCKFILE_EXCLUSIVE_LOCK      = 0x00000002

	FILE_NOTIFY_CHANGE_FILE_NAME  = 0x001
	FILE_NOTIFY_CHANGE_SIZE       = 0x008
	FILE_NOTIFY_CHANGE_LAST_WRITE = 0x010

	IDLE_PRIORITY_CLASS         = 0x00000040
	BELOW_NORMAL_PRIORITY_CLASS = 0x00004000
	NORMAL_PRIORITY_CLASS       = 0x00000020
	ABOVE_NORMAL_PRIORITY_CLASS = 0x00008000
	HIGH_PRIORITY_CLASS         = 0x00000080
	DETACHED_PROCESS            = 0x00000008
	CREATE_NEW_PROCESS_GROUP    = 0x00000200
	CREATE_NO_WINDOW            = 0x08000000
	CREATE_SUSPENDED            = 0x00000004

	JOB_OBJECT_LIMIT_PRIORITY_CLASS     = 0x00000020
	JOB_OBJECT_LIMIT_JOB_MEMORY         = 0x00000200
	JOB_OBJECT_LIMIT_BREAKAWAY_OK       = 0x00000800
	JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE  = 0x00002000
	JobObjectBasicAccountingInformation = 1
	JobObjectBasicProcessIdList         = 3
	JobObjectExtendedLimitInformation   = 9

	ProcessBasicInformation = 0
)

type IO_COUNTERS struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type JOBOBJECT_BASIC_LIMIT_INFORMATION struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type JOBOBJECT_EXTENDED_LIMIT_INFORMATION struct {
	BasicLimitInformation JOBOBJECT_BASIC_LIMIT_INFORMATION
	IoInfo                IO_COUNTERS
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type PROCESS_BASIC_INFORMATION struct {
	ExitStatus                   uint32
	PebBaseAddress               uintptr
	AffinityMask                 uintptr
	BasePriority                 int32
	UniqueProcessId              uintptr
	InheritedFromUniqueProcessId uintptr
}

// NTStatus is a failed native call's status.
type NTStatus uint32

func (s NTStatus) Error() string { return "NTSTATUS 0x" + strconv.FormatUint(uint64(s), 16) }

// The calls Go's syscall package has, with the same signatures as x/sys/windows.
var (
	CloseHandle                = syscall.CloseHandle
	OpenProcess                = syscall.OpenProcess
	GetProcessTimes            = syscall.GetProcessTimes
	WaitForSingleObject        = syscall.WaitForSingleObject
	TerminateProcess           = syscall.TerminateProcess
	GetExitCodeProcess         = syscall.GetExitCodeProcess
	CreateProcess              = syscall.CreateProcess
	GetFileInformationByHandle = syscall.GetFileInformationByHandle
	UTF16PtrFromString         = syscall.UTF16PtrFromString
	StringToUTF16Ptr           = syscall.StringToUTF16Ptr
	UTF16ToString              = syscall.UTF16ToString
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll") // a known DLL: Windows only ever loads System32's
	ntdll    = syscall.NewLazyDLL("ntdll.dll")    // likewise, and already in every process

	procGetSystemDirectoryW          = kernel32.NewProc("GetSystemDirectoryW")
	procGetCurrentProcessId          = kernel32.NewProc("GetCurrentProcessId")
	procCreateFileW                  = kernel32.NewProc("CreateFileW")
	procCreateJobObjectW             = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject     = kernel32.NewProc("AssignProcessToJobObject")
	procSetInformationJobObject      = kernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObject    = kernel32.NewProc("QueryInformationJobObject")
	procCreateIoCompletionPort       = kernel32.NewProc("CreateIoCompletionPort")
	procGetQueuedCompletionStatus    = kernel32.NewProc("GetQueuedCompletionStatus")
	procOpenThread                   = kernel32.NewProc("OpenThread")
	procCreateMutexW                 = kernel32.NewProc("CreateMutexW")
	procOpenMutexW                   = kernel32.NewProc("OpenMutexW")
	procCreateEventW                 = kernel32.NewProc("CreateEventW")
	procOpenEventW                   = kernel32.NewProc("OpenEventW")
	procSetEvent                     = kernel32.NewProc("SetEvent")
	procLockFileEx                   = kernel32.NewProc("LockFileEx")
	procFindFirstChangeNotificationW = kernel32.NewProc("FindFirstChangeNotificationW")
	procFindNextChangeNotification   = kernel32.NewProc("FindNextChangeNotification")
	procFindCloseChangeNotification  = kernel32.NewProc("FindCloseChangeNotification")
	procSetPriorityClass             = kernel32.NewProc("SetPriorityClass")
	procGetPriorityClass             = kernel32.NewProc("GetPriorityClass")
	procGetShortPathNameW            = kernel32.NewProc("GetShortPathNameW")
	procQueryFullProcessImageNameW   = kernel32.NewProc("QueryFullProcessImageNameW")
	procNtQueryInformationProcess    = ntdll.NewProc("NtQueryInformationProcess")
	procWaitForMultipleObjects       = kernel32.NewProc("WaitForMultipleObjects")
	procProcessIdToSessionId         = kernel32.NewProc("ProcessIdToSessionId")

	// user32 loads only on a desktop call (run --normal), never in the hook.
	user32             = syscall.NewLazyDLL("user32.dll") // a known DLL, like kernel32
	procCreateDesktopW = user32.NewProc("CreateDesktopW")
)

// NewLazySystemDLL loads name from System32 only, never from the current or program folder.
func NewLazySystemDLL(name string) *LazyDLL {
	buf := make([]uint16, MAX_PATH)
	n, _, _ := procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.NewLazyDLL(UTF16ToString(buf[:n]) + `\` + name)
}

// CurrentProcess is the pseudo handle every process has for itself.
func CurrentProcess() Handle { return Handle(^uintptr(0)) }

func GetCurrentProcessId() uint32 {
	r, _, _ := procGetCurrentProcessId.Call()
	return uint32(r)
}

func errno(e error) error {
	if e, ok := e.(syscall.Errno); ok && e != 0 {
		return e
	}
	return syscall.EINVAL
}

func b(v bool) uintptr {
	if v {
		return 1
	}
	return 0
}

// ok calls a function that returns a nonzero value on success.
func ok(p *LazyProc, args ...uintptr) (uintptr, error) {
	r, _, e := p.Call(args...)
	if r == 0 {
		return 0, errno(e)
	}
	return r, nil
}

// handle calls a function that returns a handle, and fails with NULL (or with
// INVALID_HANDLE_VALUE when invalid is set).
func handle(p *LazyProc, invalid bool, args ...uintptr) (Handle, error) {
	r, _, e := p.Call(args...)
	if h := Handle(r); h == 0 || invalid && h == InvalidHandle {
		return h, errno(e)
	}
	return Handle(r), nil
}

// CreateFile fails only when no handle comes back: unlike syscall.CreateFile, opening a file
// that already exists with OPEN_ALWAYS or CREATE_ALWAYS is not an error.
func CreateFile(name *uint16, access, mode uint32, sa *SecurityAttributes, createmode, attrs uint32, templatefile Handle) (Handle, error) {
	return handle(procCreateFileW, true, uintptr(unsafe.Pointer(name)), uintptr(access), uintptr(mode), uintptr(unsafe.Pointer(sa)), uintptr(createmode), uintptr(attrs), uintptr(templatefile))
}

func CreateJobObject(sa *SecurityAttributes, name *uint16) (Handle, error) {
	return handle(procCreateJobObjectW, false, uintptr(unsafe.Pointer(sa)), uintptr(unsafe.Pointer(name)))
}

func AssignProcessToJobObject(job, process Handle) error {
	_, err := ok(procAssignProcessToJobObject, uintptr(job), uintptr(process))
	return err
}

func SetInformationJobObject(job Handle, class uint32, info uintptr, size uint32) (int, error) {
	r, err := ok(procSetInformationJobObject, uintptr(job), uintptr(class), info, uintptr(size))
	return int(r), err
}

func QueryInformationJobObject(job Handle, class int32, info uintptr, size uint32, retlen *uint32) error {
	_, err := ok(procQueryInformationJobObject, uintptr(job), uintptr(class), info, uintptr(size), uintptr(unsafe.Pointer(retlen)))
	return err
}

func CreateIoCompletionPort(file, port Handle, key uintptr, threads uint32) (Handle, error) {
	return handle(procCreateIoCompletionPort, false, uintptr(file), uintptr(port), key, uintptr(threads))
}

func GetQueuedCompletionStatus(port Handle, qty *uint32, key *uintptr, ov **Overlapped, timeout uint32) error {
	_, err := ok(procGetQueuedCompletionStatus, uintptr(port), uintptr(unsafe.Pointer(qty)), uintptr(unsafe.Pointer(key)), uintptr(unsafe.Pointer(ov)), uintptr(timeout))
	return err
}

func OpenThread(access uint32, inherit bool, tid uint32) (Handle, error) {
	return handle(procOpenThread, false, uintptr(access), b(inherit), uintptr(tid))
}

// CreateMutex returns the handle and ERROR_ALREADY_EXISTS when the named mutex was there already.
func CreateMutex(sa *SecurityAttributes, initialOwner bool, name *uint16) (Handle, error) {
	r, _, e := procCreateMutexW.Call(uintptr(unsafe.Pointer(sa)), b(initialOwner), uintptr(unsafe.Pointer(name)))
	if r == 0 || e == ERROR_ALREADY_EXISTS {
		return Handle(r), errno(e)
	}
	return Handle(r), nil
}

func OpenMutex(access uint32, inherit bool, name *uint16) (Handle, error) {
	return handle(procOpenMutexW, false, uintptr(access), b(inherit), uintptr(unsafe.Pointer(name)))
}

// CreateEvent returns the handle and ERROR_ALREADY_EXISTS when the named event was there already.
func CreateEvent(sa *SecurityAttributes, manualReset, initialState uint32, name *uint16) (Handle, error) {
	r, _, e := procCreateEventW.Call(uintptr(unsafe.Pointer(sa)), uintptr(manualReset), uintptr(initialState), uintptr(unsafe.Pointer(name)))
	if r == 0 || e == ERROR_ALREADY_EXISTS {
		return Handle(r), errno(e)
	}
	return Handle(r), nil
}

func OpenEvent(access uint32, inherit bool, name *uint16) (Handle, error) {
	return handle(procOpenEventW, false, uintptr(access), b(inherit), uintptr(unsafe.Pointer(name)))
}

func SetEvent(event Handle) error {
	_, err := ok(procSetEvent, uintptr(event))
	return err
}

func LockFileEx(file Handle, flags, reserved, bytesLow, bytesHigh uint32, ov *Overlapped) error {
	_, err := ok(procLockFileEx, uintptr(file), uintptr(flags), uintptr(reserved), uintptr(bytesLow), uintptr(bytesHigh), uintptr(unsafe.Pointer(ov)))
	return err
}

func FindFirstChangeNotification(path string, subtree bool, filter uint32) (Handle, error) {
	p, err := UTF16PtrFromString(path)
	if err != nil {
		return InvalidHandle, err
	}
	return handle(procFindFirstChangeNotificationW, true, uintptr(unsafe.Pointer(p)), b(subtree), uintptr(filter))
}

func FindNextChangeNotification(h Handle) error {
	_, err := ok(procFindNextChangeNotification, uintptr(h))
	return err
}

func FindCloseChangeNotification(h Handle) error {
	_, err := ok(procFindCloseChangeNotification, uintptr(h))
	return err
}

func SetPriorityClass(process Handle, class uint32) error {
	_, err := ok(procSetPriorityClass, uintptr(process), uintptr(class))
	return err
}

func GetPriorityClass(process Handle) (uint32, error) {
	r, err := ok(procGetPriorityClass, uintptr(process))
	return uint32(r), err
}

func GetShortPathName(long, short *uint16, size uint32) (uint32, error) {
	r, err := ok(procGetShortPathNameW, uintptr(unsafe.Pointer(long)), uintptr(unsafe.Pointer(short)), uintptr(size))
	return uint32(r), err
}

func QueryFullProcessImageName(process Handle, flags uint32, name *uint16, size *uint32) error {
	_, err := ok(procQueryFullProcessImageNameW, uintptr(process), uintptr(flags), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(size)))
	return err
}

func NtQueryInformationProcess(process Handle, class int32, info unsafe.Pointer, size uint32, retlen *uint32) error {
	r, _, _ := procNtQueryInformationProcess.Call(uintptr(process), uintptr(class), uintptr(info), uintptr(size), uintptr(unsafe.Pointer(retlen)))
	if r != 0 {
		return NTStatus(r)
	}
	return nil
}

// WaitForMultipleObjects waits for one (or, with waitAll, every) handle, up to MAXIMUM_WAIT_OBJECTS.
func WaitForMultipleObjects(handles []Handle, waitAll bool, ms uint32) (uint32, error) {
	if len(handles) == 0 || len(handles) > MAXIMUM_WAIT_OBJECTS {
		return 0, syscall.EINVAL
	}
	r, _, e := procWaitForMultipleObjects.Call(uintptr(len(handles)), uintptr(unsafe.Pointer(&handles[0])), b(waitAll), uintptr(ms))
	if uint32(r) == 0xFFFFFFFF {
		return 0, errno(e)
	}
	return uint32(r), nil
}

func ProcessIdToSessionId(pid uint32, session *uint32) error {
	_, err := ok(procProcessIdToSessionId, uintptr(pid), uintptr(unsafe.Pointer(session)))
	return err
}

// CreateDesktop makes (or opens, if the name exists) a desktop on the calling process's window
// station. Nothing on it is ever shown on the screen, and its windows cannot take the screen's focus.
// It lasts while this handle or a thread on it does.
func CreateDesktop(name string) (Handle, error) {
	p, err := UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	return handle(procCreateDesktopW, false, uintptr(unsafe.Pointer(p)), 0, 0, 0, GENERIC_ALL, 0)
}

// unicodeString is Windows' UNICODE_STRING.
type unicodeString struct {
	Length, MaximumLength uint16
	Buffer                *uint16
}

// desktopInfo is this process's RTL_USER_PROCESS_PARAMETERS.DesktopInfo ("WinSta0\Default"): the
// desktop CreateProcess gives a child whose STARTUPINFO names none (Go's exec never names one).
// The offsets are the 64-bit PEB's (ProcessParameters at 0x20, DesktopInfo at 0xC0), stable since
// Windows XP x64.
func desktopInfo() (*unicodeString, error) {
	var pbi PROCESS_BASIC_INFORMATION
	var n uint32
	if err := NtQueryInformationProcess(CurrentProcess(), 0, unsafe.Pointer(&pbi), uint32(unsafe.Sizeof(pbi)), &n); err != nil {
		return nil, err
	}
	paramsAt := pbi.PebBaseAddress + 0x20
	params := **(**uintptr)(unsafe.Pointer(&paramsAt))
	infoAt := params + 0xC0
	info := *(**unicodeString)(unsafe.Pointer(&infoAt))
	if info.Length%2 != 0 || info.Length > info.MaximumLength || info.Length > 1024 || info.Length > 0 && info.Buffer == nil {
		return nil, syscall.EINVAL
	}
	return info, nil
}

// ChildDesktop reads the desktop ("WinSta0\Default") a process this one starts would be given.
func ChildDesktop() (string, error) {
	info, err := desktopInfo()
	if err != nil || info.Length == 0 {
		return "", err
	}
	return UTF16ToString(unsafe.Slice(info.Buffer, info.Length/2)), nil
}

// WithChildDesktop runs start with the desktop of processes this one starts set to desktop
// ("WinSta0\name"), then sets it back. Run nothing else that starts processes meanwhile.
func WithChildDesktop(desktop string, start func() error) error {
	info, err := desktopInfo()
	if err != nil {
		return err
	}
	s, err := syscall.UTF16FromString(desktop)
	if err != nil {
		return err
	}
	saved := *info
	*info = unicodeString{Length: uint16(2 * (len(s) - 1)), MaximumLength: uint16(2 * len(s)), Buffer: &s[0]}
	defer func() { *info = saved }()
	return start()
}
