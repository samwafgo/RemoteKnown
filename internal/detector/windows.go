package detector

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	uia "github.com/auuunya/go-element"
	"github.com/shirou/gopsutil/process"
	"golang.org/x/sys/windows"
)

type WindowsDetector struct {
	// 进程缓存，避免频繁查询
	processCache     map[string][]*process.Process
	processCacheTime time.Time
	cacheMutex       sync.RWMutex
	cacheDuration    time.Duration
}

var (
	user32 = windows.NewLazyDLL("user32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetClassName             = user32.NewProc("GetClassNameW")
	procGetWindowText            = user32.NewProc("GetWindowTextW")
	procGetWindowTextLength      = user32.NewProc("GetWindowTextLengthW")
)

// RemoteTool 定义远程工具配置
type RemoteTool struct {
	ProcessName        string   // 进程名（可为空，表示不检查进程名）
	WindowClass        string   // 窗口类名（用于检测远程状态）
	WindowTitle        string   // 窗口标题（用于检测远程状态，支持部分匹配）
	CommandLineArgs    []string // 命令行参数特征（用于检测远程状态）
	ToolName           string   // 工具显示名称
	TCPConnThreshold   int      // TCP连接数阈值（大于等于此值认为被远程，0表示不检测）
	UDPConnThreshold   int      // UDP连接数阈值（大于此值认为被远程，0表示不检测）
	UseEstablishedOnly bool     // 是否只统计ESTABLISHED状态的连接（仅对TCP有效）
}

// 远程工具配置列表
var remoteTools = []RemoteTool{
	{ProcessName: "todesk.exe", CommandLineArgs: []string{"--localPort=", "--isVideoSession=true"}, ToolName: "ToDesk"},
	{ProcessName: "AweSun.exe", CommandLineArgs: []string{"--mod=desktopagent", "--port=", "--agentid=", "--lockscreen="}, ToolName: "向日葵"}, // 向日葵使用命令行参数检测
	{ProcessName: "sunloginclient.exe", ToolName: "向日葵客户端"},                                                                                 // 占位符，待补充检测方法
	{ProcessName: "GameViewerServer.exe", ToolName: "网易UU远程", TCPConnThreshold: 5, UseEstablishedOnly: true},                                // 网易UU远程，基于TCP连接数检测
	{ProcessName: "AskLink.exe", ToolName: "AskLink远程", UDPConnThreshold: 1},                                                                // AskLink远程，基于UDP连接数检测
	{ProcessName: "RCClient.exe", ToolName: "远程看看", WindowTitle: "聊天"},                                                                      // 远程看看，基于窗口标题检测
}

func NewWindowsDetector() *WindowsDetector {
	return &WindowsDetector{
		processCache:  make(map[string][]*process.Process),
		cacheDuration: 3 * time.Second, // 缓存3秒，减少重复查询
	}
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

// DetectSessions 检测系统会话（占位符）
func (d *WindowsDetector) DetectSessions() ([]Signal, error) {
	return []Signal{}, nil
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
