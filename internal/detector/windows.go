package detector

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	uia "github.com/auuunya/go-element"
	gopsutilnet "github.com/shirou/gopsutil/net"
	"github.com/shirou/gopsutil/process"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type WindowsDetector struct {
	processCache     map[string][]*process.Process
	processCacheTime time.Time
	cacheMutex       sync.RWMutex
	cacheDuration    time.Duration
	rdpPort          uint32
}

var (
	user32 = windows.NewLazyDLL("user32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetClassName             = user32.NewProc("GetClassNameW")
	procGetWindowText            = user32.NewProc("GetWindowTextW")
	procGetWindowTextLength      = user32.NewProc("GetWindowTextLengthW")

	wtsapi32                 = windows.NewLazyDLL("wtsapi32.dll")
	procWTSEnumerateSessions = wtsapi32.NewProc("WTSEnumerateSessionsW")
	procWTSFreeMemory        = wtsapi32.NewProc("WTSFreeMemory")
	procWTSQuerySessionInfo  = wtsapi32.NewProc("WTSQuerySessionInformationW")
)

const (
	wtsActive             uint32 = 0  // WTS_CONNECTSTATE_CLASS
	wtsClientName         uint32 = 10 // WTS_INFO_CLASS
	wtsClientAddress      uint32 = 14 // WTS_INFO_CLASS
	wtsClientProtocolType uint32 = 16 // WTS_INFO_CLASS; 0=Console, 2=RDP
)

// wtsSessionInfo 与 WTS_SESSION_INFOW 内存布局一致（x64: 24 bytes）
type wtsSessionInfo struct {
	SessionId      uint32
	WinStationName uintptr // *uint16，DLL 分配，用 WTSFreeMemory 释放
	State          uint32
}

// RemoteTool 定义远程工具配置
type RemoteTool struct {
	ProcessName             string   // 进程名（可为空，表示不检查进程名）
	WindowClass             string   // 窗口类名（用于检测远程状态）
	WindowTitle             string   // 窗口标题（用于检测远程状态，支持部分匹配）
	CommandLineArgs         []string // 命令行参数特征（用于检测远程状态）
	DetectChildProcess      bool     // 是否检测"会话子进程"：父进程也是同名进程的派生进程（新版 ToDesk 会话激活时派生）
	ChildProcessExcludeArgs []string // 子进程检测时需排除的命令行特征（用于排除常驻的服务/主客户端进程）
	ToolName                string   // 工具显示名称
	TCPConnThreshold        int      // TCP连接数阈值（大于等于此值认为被远程，0表示不检测）
	UDPConnThreshold        int      // UDP连接数阈值（大于此值认为被远程，0表示不检测）
	UseEstablishedOnly      bool     // 是否只统计ESTABLISHED状态的连接（仅对TCP有效）
}

// 远程工具配置列表
var remoteTools = []RemoteTool{
	// ToDesk 兼容两种检测方式：
	//   旧版：主客户端命令行同时带 --localPort= 和 --isVideoSession=true
	//   新版：远程会话激活时，在主客户端下派生一个无参数的 ToDesk.exe 子进程（排除带 --runservice/--localPort 的常驻进程）
	{ProcessName: "todesk.exe", CommandLineArgs: []string{"--localPort=", "--isVideoSession=true"}, DetectChildProcess: true, ChildProcessExcludeArgs: []string{"--runservice", "--localPort", "--isVideoSession", "--hide"}, ToolName: "ToDesk"},
	{ProcessName: "AweSun.exe", CommandLineArgs: []string{"--mod=desktopagent", "--port=", "--agentid=", "--lockscreen="}, ToolName: "向日葵"}, // 向日葵使用命令行参数检测
	{ProcessName: "sunloginclient.exe", ToolName: "向日葵客户端"},                                                  // 占位符，待补充检测方法
	{ProcessName: "GameViewerServer.exe", ToolName: "网易UU远程", TCPConnThreshold: 5, UseEstablishedOnly: true}, // 网易UU远程，基于TCP连接数检测
	{ProcessName: "AskLink.exe", ToolName: "AskLink远程", UDPConnThreshold: 1},                                 // AskLink远程，基于UDP连接数检测
	{ProcessName: "RCClient.exe", ToolName: "远程看看", WindowTitle: "聊天"},                                       // 远程看看，基于窗口标题检测
}

func NewWindowsDetector() *WindowsDetector {
	return &WindowsDetector{
		processCache:  make(map[string][]*process.Process),
		cacheDuration: 3 * time.Second,
		rdpPort:       readRDPPort(),
	}
}

// readRDPPort 从注册表读取 RDP 监听端口，读取失败时返回默认值 3389
func readRDPPort() uint32 {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Terminal Server\WinStations\RDP-Tcp`,
		registry.QUERY_VALUE)
	if err != nil {
		return 3389
	}
	defer k.Close()
	val, _, err := k.GetIntegerValue("PortNumber")
	if err != nil {
		return 3389
	}
	return uint32(val)
}

// findProcessesByName 通过进程名查找进程（优化版本，带缓存）
func (d *WindowsDetector) findProcessesByName(processName string) []*process.Process {
	// 检查缓存
	d.cacheMutex.RLock()
	if time.Since(d.processCacheTime) < d.cacheDuration {
		if cached, ok := d.processCache[processName]; ok {
			d.cacheMutex.RUnlock()
			return cached
		}
	}
	d.cacheMutex.RUnlock()

	// 缓存过期或不存在，重新查询
	var matchedProcesses []*process.Process

	// 创建进程快照
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return matchedProcesses
	}
	defer windows.CloseHandle(snapshot)

	var procEntry windows.ProcessEntry32
	procEntry.Size = uint32(unsafe.Sizeof(procEntry))

	// 获取第一个进程
	err = windows.Process32First(snapshot, &procEntry)
	if err != nil {
		return matchedProcesses
	}

	processNameLower := strings.ToLower(processName)

	// 遍历所有进程
	for {
		// 将进程名从 UTF16 转换为字符串
		exeFile := windows.UTF16ToString(procEntry.ExeFile[:])

		// 比较进程名（不区分大小写）
		if strings.EqualFold(exeFile, processName) || strings.EqualFold(strings.ToLower(exeFile), processNameLower) {
			// 创建 Process 对象
			p, err := process.NewProcess(int32(procEntry.ProcessID))
			if err == nil {
				matchedProcesses = append(matchedProcesses, p)
			}
		}

		// 获取下一个进程
		err = windows.Process32Next(snapshot, &procEntry)
		if err != nil {
			break
		}
	}

	// 更新缓存
	d.cacheMutex.Lock()
	d.processCache[processName] = matchedProcesses
	d.processCacheTime = time.Now()
	d.cacheMutex.Unlock()

	return matchedProcesses
}

// clearCache 清除进程缓存（在检测周期开始时调用）
func (d *WindowsDetector) clearCache() {
	d.cacheMutex.Lock()
	d.processCache = make(map[string][]*process.Process)
	d.cacheMutex.Unlock()
}

// DetectRemoteTools 检测远程工具：进程存在 + 窗口类名存在 = 被远程控制
func (d *WindowsDetector) DetectRemoteTools() ([]Signal, error) {
	var signals []Signal

	// 优化：清除缓存，确保每次检测都是最新的
	// 但在同一次检测周期内，多个工具可以共享缓存
	d.clearCache()

	// 优化：不再获取所有进程，而是使用进程名直接查找
	// 这样可以大幅减少系统调用次数

	// 检查每个远程工具
	for _, tool := range remoteTools {
		// 第一步：收集所有匹配的进程（可能有多个同名进程，或检查所有进程）
		var matchedProcesses []*process.Process

		if tool.ProcessName != "" {
			// 优化：使用进程名直接查找，而不是遍历所有进程
			matchedProcesses = d.findProcessesByName(tool.ProcessName)
		} else {
			// 如果进程名为空，需要检查所有进程（用于检测命令行参数）
			// 这种情况应该避免，因为会导致性能问题
			procs, err := process.Processes()
			if err != nil {
				continue
			}
			matchedProcesses = procs
		}

		// 如果进程不存在，跳过该工具
		if len(matchedProcesses) == 0 {
			continue
		}

		// 第二步：遍历所有匹配的进程，检查远程状态特征
		isRemote := false
		var remoteProcess *process.Process
		var detectionMethod string

		// 优先检查命令行参数（最可靠）
		if len(tool.CommandLineArgs) > 0 {
			for _, p := range matchedProcesses {
				cmdline, err := p.Cmdline()
				if err != nil {
					continue
				}

				// 去掉路径，只检查参数部分
				// 例如："C:\Program Files\ToDesk\ToDesk.exe" --localPort=35600 --isVideoSession=true
				// 提取参数部分：--localPort=35600 --isVideoSession=true
				cmdlineLower := strings.ToLower(cmdline)

				// 如果命令行包含可执行文件名，提取参数部分（去掉路径和可执行文件名）
				// 通用处理：查找 .exe 后面的部分
				if tool.ProcessName != "" {
					processNameLower := strings.ToLower(tool.ProcessName)
					if strings.Contains(cmdlineLower, processNameLower) {
						// 找到进程名后面的部分
						parts := strings.SplitN(cmdlineLower, processNameLower, 2)
						if len(parts) > 1 {
							cmdlineLower = strings.TrimSpace(parts[1])
							// 去掉可能的引号
							cmdlineLower = strings.Trim(cmdlineLower, "\"")
							cmdlineLower = strings.TrimSpace(cmdlineLower)
						}
					}
				}

				// 检查是否包含所有必需的命令行参数
				allArgsFound := true
				for _, arg := range tool.CommandLineArgs {
					if !strings.Contains(cmdlineLower, strings.ToLower(arg)) {
						allArgsFound = false
						break
					}
				}

				if allArgsFound {
					isRemote = true
					remoteProcess = p
					detectionMethod = "命令行参数"
					break
				}
			}
		}

		// 如果命令行参数检测失败，尝试"会话子进程"检测
		// （新版 ToDesk：远程会话激活时会在主客户端下派生一个无参数的同名子进程）
		if !isRemote && tool.DetectChildProcess {
			if child := d.detectChildProcess(matchedProcesses, tool.ChildProcessExcludeArgs); child != nil {
				isRemote = true
				remoteProcess = child
				detectionMethod = "会话子进程"
			}
		}

		// 如果命令行参数检测失败，尝试窗口类名检测
		if !isRemote && tool.WindowClass != "" {
			for _, p := range matchedProcesses {
				hasWindow, err := d.detectWindowClass(int32(p.Pid), tool.WindowClass)
				if err == nil && hasWindow {
					isRemote = true
					remoteProcess = p
					detectionMethod = "窗口类名"
					break
				}
			}
		}

		// 如果窗口类名检测失败，尝试窗口标题检测
		if !isRemote && tool.WindowTitle != "" {
			for _, p := range matchedProcesses {
				hasTitle, err := d.detectWindowTitle(int32(p.Pid), tool.WindowTitle)
				if err == nil && hasTitle {
					isRemote = true
					remoteProcess = p
					detectionMethod = fmt.Sprintf("窗口标题包含:%s", tool.WindowTitle)
					break
				}
			}
		}

		// 如果窗口类名检测失败，尝试TCP连接数检测
		if !isRemote && tool.TCPConnThreshold > 0 {
			for _, p := range matchedProcesses {
				connCount, err := d.getTCPConnectionCount(int32(p.Pid), tool.UseEstablishedOnly)
				if err == nil && connCount >= tool.TCPConnThreshold {
					isRemote = true
					remoteProcess = p
					detectionMethod = fmt.Sprintf("TCP连接数:%d", connCount)
					break
				}
			}
		}

		// 如果TCP连接检测失败，尝试UDP连接数检测
		if !isRemote && tool.UDPConnThreshold > 0 {
			for _, p := range matchedProcesses {
				connCount, err := d.getUDPConnectionCount(int32(p.Pid))
				if err == nil && connCount > tool.UDPConnThreshold {
					isRemote = true
					remoteProcess = p
					detectionMethod = fmt.Sprintf("UDP连接数:%d", connCount)
					break
				}
			}
		}

		// 如果都没有配置，且进程名不为空，进程存在就认为被远程控制
		if !isRemote && tool.ProcessName != "" && tool.WindowClass == "" && tool.WindowTitle == "" && len(tool.CommandLineArgs) == 0 && tool.TCPConnThreshold == 0 && tool.UDPConnThreshold == 0 {
			isRemote = true
			remoteProcess = matchedProcesses[0]
			detectionMethod = "进程存在"
		}

		if isRemote && remoteProcess != nil {
			signalName := tool.ToolName
			if detectionMethod != "" {
				signalName += " (" + detectionMethod + ")"
			}

			signals = append(signals, Signal{
				Type:       "remote_tool",
				Name:       signalName,
				Confidence: 1.0, // 简化：检测到就是1.0
				Source:     fmt.Sprintf("进程:%s PID:%d", tool.ProcessName, remoteProcess.Pid),
				DetectedAt: time.Now(),
			})
		}
	}

	return signals, nil
}

// detectChildProcess 检测是否存在"会话子进程"：父进程也是同名进程的派生进程。
//
// 新版 ToDesk 在远程会话激活时，会在主客户端（带 --localPort 的进程）下派生一个
// 无参数的 ToDesk.exe 子进程；会话结束时该子进程退出。利用这一特征判断是否被远程。
//
// excludeArgs 用于排除常驻进程：服务进程（--runservice）和主客户端（--localPort/--hide）
// 本身的父进程可能也是同名进程，必须根据命令行特征排除，否则空闲时会误报。
// 返回命中的子进程，未命中返回 nil。
func (d *WindowsDetector) detectChildProcess(processes []*process.Process, excludeArgs []string) *process.Process {
	// 收集所有同名进程的 PID 集合，用于判断父进程是否同样是该工具的进程
	pidSet := make(map[int32]bool, len(processes))
	for _, p := range processes {
		pidSet[p.Pid] = true
	}

	for _, p := range processes {
		// 根据命令行特征排除常驻的服务/主客户端进程
		if len(excludeArgs) > 0 {
			cmdline, err := p.Cmdline()
			if err != nil {
				// 读不到命令行时无法确认是否为常驻进程，保守跳过，避免误报
				continue
			}
			cmdlineLower := strings.ToLower(cmdline)
			excluded := false
			for _, arg := range excludeArgs {
				if strings.Contains(cmdlineLower, strings.ToLower(arg)) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
		}

		// 父进程同样是该工具的进程 => 判定为会话子进程
		ppid, err := p.Ppid()
		if err != nil {
			continue
		}
		if pidSet[ppid] {
			return p
		}
	}
	return nil
}

// detectWindowClass 检测指定进程是否有指定窗口类名的窗口
// 先尝试使用 Windows API，如果失败则使用 go-element 库
func (d *WindowsDetector) detectWindowClass(pid int32, className string) (bool, error) {
	// 方法1: 使用 Windows API 直接获取
	found, err := d.detectWindowClassByAPI(pid, className)
	if err == nil && found {
		return true, nil
	}

	// 方法2: 如果 API 方法失败，使用 go-element 库
	/*found, err = d.detectWindowClassByGoElement(pid, className)
	if err == nil && found {
		return true, nil
	}
	*/
	return false, fmt.Errorf("未找到窗口类名 %s (PID: %d)", className, pid)
}

// detectWindowTitle 检测指定进程是否有指定标题的窗口（支持包含匹配）
func (d *WindowsDetector) detectWindowTitle(pid int32, titleSubStr string) (bool, error) {
	var found bool

	enumProc := syscall.NewCallback(func(hwnd syscall.Handle, lParam uintptr) uintptr {
		// 检查当前窗口是否属于目标进程
		var windowPid uint32
		procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&windowPid)))

		if uint32(pid) == windowPid {
			// 获取窗口标题长度
			ret, _, _ := procGetWindowTextLength.Call(uintptr(hwnd))
			if ret > 0 {
				length := int(ret) + 1
				buf := make([]uint16, length)
				// 获取窗口标题
				ret, _, _ := procGetWindowText.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(length))
				if ret > 0 {
					windowTitle := windows.UTF16ToString(buf)
					// 检查标题是否包含目标字符串
					if strings.Contains(windowTitle, titleSubStr) {
						found = true
						return 0 // 停止枚举
					}
				}
			}
		}
		return 1 // 继续枚举
	})

	procEnumWindows.Call(enumProc, 0)
	return found, nil
}

// detectWindowClassByAPI 使用 Windows API 检测窗口类名
func (d *WindowsDetector) detectWindowClassByAPI(pid int32, className string) (bool, error) {
	var found bool

	enumProc := syscall.NewCallback(func(hwnd syscall.Handle, lParam uintptr) uintptr {
		var windowPid uint32
		procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&windowPid)))

		if uint32(pid) == windowPid {
			// 获取窗口类名
			buf := make([]uint16, 256)
			ret, _, _ := procGetClassName.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))

			if ret > 0 {
				windowClass := windows.UTF16ToString(buf)
				if windowClass == className {
					found = true
					return 0 // 停止枚举
				}
			}
		}
		return 1 // 继续枚举
	})

	procEnumWindows.Call(enumProc, 0)
	return found, nil
}

// detectWindowClassByGoElement 使用 go-element 库检测窗口类名
func (d *WindowsDetector) detectWindowClassByGoElement(pid int32, className string) (bool, error) {
	// 初始化 COM
	uia.CoInitialize()
	defer uia.CoUninitialize()

	// 创建 UI Automation 实例
	instance, err := uia.CreateInstance(
		uia.CLSID_CUIAutomation,
		uia.IID_IUIAutomation,
		uia.CLSCTX_INPROC_SERVER,
	)
	if err != nil {
		return false, fmt.Errorf("创建 UI Automation 实例失败: %w", err)
	}

	ppv := uia.NewIUIAutomation(uia.NewIUnKnown(instance))
	defer ppv.Release()

	// 获取根元素（桌面）
	root, err := ppv.GetRootElement()
	if err != nil {
		return false, fmt.Errorf("获取根元素失败: %w", err)
	}
	defer root.Release()

	// 遍历 UI 树
	elems := uia.TraverseUIElementTree(ppv, root)
	if elems == nil {
		return false, fmt.Errorf("遍历 UI 树失败")
	}

	// 搜索匹配的窗口
	found := false
	uia.SearchElem(elems, func(elem *uia.Element) bool {
		if found {
			return false
		}

		// 获取窗口句柄和进程ID
		elem.NativeWindowHandle()
		elem.ProcessId()

		// 检查是否属于目标进程
		if elem.CurrentProcessId == pid && elem.CurrentNativeWindowHandle != 0 {
			// 使用 Windows API 获取窗口类名
			buf := make([]uint16, 256)
			ret, _, _ := procGetClassName.Call(
				elem.CurrentNativeWindowHandle,
				uintptr(unsafe.Pointer(&buf[0])),
				uintptr(len(buf)),
			)

			if ret > 0 {
				windowClass := windows.UTF16ToString(buf)
				if windowClass == className {
					found = true
					return true
				}
			}
		}

		return false
	})

	return found, nil
}

// DetectSessions 检测活跃的 Windows RDP 会话
func (d *WindowsDetector) DetectSessions() ([]Signal, error) {
	var pSessionInfo uintptr
	var sessionCount uint32

	r, _, err := procWTSEnumerateSessions.Call(
		0, 0, 1,
		uintptr(unsafe.Pointer(&pSessionInfo)),
		uintptr(unsafe.Pointer(&sessionCount)),
	)
	if r == 0 {
		return nil, fmt.Errorf("WTSEnumerateSessionsW: %w", err)
	}
	defer procWTSFreeMemory.Call(pSessionInfo)

	var signals []Signal
	infoSize := unsafe.Sizeof(wtsSessionInfo{})

	for i := uint32(0); i < sessionCount; i++ {
		info := (*wtsSessionInfo)(unsafe.Pointer(pSessionInfo + uintptr(i)*infoSize))

		if info.State != wtsActive {
			continue
		}

		stationName := ""
		if info.WinStationName != 0 {
			stationName = windows.UTF16PtrToString((*uint16)(unsafe.Pointer(info.WinStationName)))
		}
		if stationName == "Console" || stationName == "" {
			continue
		}

		// 协议类型 2 = RDP，0 = Console
		proto := d.wtsQueryUint16(info.SessionId, wtsClientProtocolType)
		if proto != 2 {
			continue
		}

		clientName := d.wtsQueryString(info.SessionId, wtsClientName)
		if clientName == "" {
			clientName = "未知客户端"
		}
		clientIP := d.wtsQueryClientIP(info.SessionId)

		displayName := clientName
		if clientIP != "" {
			displayName = clientName + " " + clientIP
		}

		signals = append(signals, Signal{
			Type:       "rdp_session",
			Name:       fmt.Sprintf("Windows RDP (来自: %s)", displayName),
			Confidence: 0.95,
			Source:     fmt.Sprintf("会话ID:%d Station:%s", info.SessionId, stationName),
			DetectedAt: time.Now(),
		})
	}

	return signals, nil
}

// wtsQueryString 查询 WTS 字符串类型信息
func (d *WindowsDetector) wtsQueryString(sessionId uint32, infoClass uint32) string {
	var pBuf uintptr
	var bytesReturned uint32
	r, _, _ := procWTSQuerySessionInfo.Call(
		0,
		uintptr(sessionId),
		uintptr(infoClass),
		uintptr(unsafe.Pointer(&pBuf)),
		uintptr(unsafe.Pointer(&bytesReturned)),
	)
	if r == 0 || pBuf == 0 {
		return ""
	}
	defer procWTSFreeMemory.Call(pBuf)
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(pBuf)))
}

// wtsQueryUint16 查询 WTS uint16 类型信息（如协议类型）
func (d *WindowsDetector) wtsQueryUint16(sessionId uint32, infoClass uint32) uint16 {
	var pBuf uintptr
	var bytesReturned uint32
	r, _, _ := procWTSQuerySessionInfo.Call(
		0,
		uintptr(sessionId),
		uintptr(infoClass),
		uintptr(unsafe.Pointer(&pBuf)),
		uintptr(unsafe.Pointer(&bytesReturned)),
	)
	if r == 0 || pBuf == 0 {
		return 0
	}
	defer procWTSFreeMemory.Call(pBuf)
	return *(*uint16)(unsafe.Pointer(pBuf))
}

// wtsQueryClientIP 查询 RDP 客户端 IP 地址；先走 WTS API，失败则回退到 TCP 连接扫描
func (d *WindowsDetector) wtsQueryClientIP(sessionId uint32) string {
	if ip := d.wtsIPFromAPI(sessionId); ip != "" {
		return ip
	}
	return d.rdpIPFromTCP()
}

// wtsIPFromAPI 通过 WTSQuerySessionInformation 获取客户端 IP
func (d *WindowsDetector) wtsIPFromAPI(sessionId uint32) string {
	var pBuf uintptr
	var bytesReturned uint32
	r, _, _ := procWTSQuerySessionInfo.Call(
		0,
		uintptr(sessionId),
		uintptr(wtsClientAddress),
		uintptr(unsafe.Pointer(&pBuf)),
		uintptr(unsafe.Pointer(&bytesReturned)),
	)
	if r == 0 || pBuf == 0 {
		return ""
	}
	defer procWTSFreeMemory.Call(pBuf)

	type wtsAddr struct {
		Family  uint32
		Address [20]byte
	}
	addr := (*wtsAddr)(unsafe.Pointer(pBuf))

	switch addr.Family {
	case 2: // AF_INET — IPv4 地址在 Address[2..5]
		ip := fmt.Sprintf("%d.%d.%d.%d",
			addr.Address[2], addr.Address[3],
			addr.Address[4], addr.Address[5])
		if ip == "0.0.0.0" {
			// 部分驱动把地址放在 [0..3]
			ip = fmt.Sprintf("%d.%d.%d.%d",
				addr.Address[0], addr.Address[1],
				addr.Address[2], addr.Address[3])
		}
		if ip == "0.0.0.0" {
			return ""
		}
		return ip
	case 23: // AF_INET6
		return fmt.Sprintf("%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x:%02x%02x",
			addr.Address[0], addr.Address[1], addr.Address[2], addr.Address[3],
			addr.Address[4], addr.Address[5], addr.Address[6], addr.Address[7],
			addr.Address[8], addr.Address[9], addr.Address[10], addr.Address[11],
			addr.Address[12], addr.Address[13], addr.Address[14], addr.Address[15])
	}
	return ""
}

// rdpIPFromTCP 从 TCP 连接中找本机 RDP 端口的 ESTABLISHED 对端 IP（兜底方案）
func (d *WindowsDetector) rdpIPFromTCP() string {
	conns, err := gopsutilnet.Connections("tcp")
	if err != nil {
		return ""
	}
	for _, conn := range conns {
		if conn.Laddr.Port == d.rdpPort && conn.Status == "ESTABLISHED" && conn.Raddr.IP != "" {
			return conn.Raddr.IP
		}
	}
	return ""
}

// DetectRDPPorts 检测 RDP 端口（占位符）
func (d *WindowsDetector) DetectRDPPorts() ([]Signal, error) {
	return []Signal{}, nil
}

// getTCPConnectionCount 获取指定进程的TCP连接数
func (d *WindowsDetector) getTCPConnectionCount(pid int32, establishedOnly bool) (int, error) {
	// 使用 gopsutil 获取进程
	p, err := process.NewProcess(pid)
	if err != nil {
		return 0, fmt.Errorf("无法获取进程 %d: %w", pid, err)
	}

	// 获取进程的所有连接
	connections, err := p.Connections()
	if err != nil {
		return 0, fmt.Errorf("无法获取进程 %d 的连接: %w", pid, err)
	}

	// 统计连接数
	count := 0
	for _, conn := range connections {
		// 只统计 TCP 连接
		if conn.Type != syscall.SOCK_STREAM {
			continue
		}

		// 如果只统计 ESTABLISHED 状态的连接
		if establishedOnly {
			if conn.Status == "ESTABLISHED" {
				count++
			}
		} else {
			// 统计所有 TCP 连接
			count++
		}
	}

	return count, nil
}

// getUDPConnectionCount 获取指定进程的UDP连接数（包括UDP和UDPv6）
func (d *WindowsDetector) getUDPConnectionCount(pid int32) (int, error) {
	// 使用 gopsutil 获取进程
	p, err := process.NewProcess(pid)
	if err != nil {
		return 0, fmt.Errorf("无法获取进程 %d: %w", pid, err)
	}

	// 获取进程的所有连接
	connections, err := p.Connections()
	if err != nil {
		return 0, fmt.Errorf("无法获取进程 %d 的连接: %w", pid, err)
	}

	// 统计 UDP 连接数（包括 UDP 和 UDPv6）
	count := 0
	for _, conn := range connections {
		// 统计 UDP 连接 (SOCK_DGRAM)
		if conn.Type == syscall.SOCK_DGRAM {
			count++
		}
	}

	return count, nil
}
