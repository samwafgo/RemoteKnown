package detector

import (
	"sort"
	"strings"
	"syscall"
	"unsafe"

	gopsutilnet "github.com/shirou/gopsutil/net"
	"github.com/shirou/gopsutil/process"
	"golang.org/x/sys/windows"
)

// ProcSnap 是一次进程快照中单个进程的精简信息（用于前后对比）。
type ProcSnap struct {
	PID  int32  `json:"pid"`
	Name string `json:"name"`
	TCP  int    `json:"tcp"` // ESTABLISHED 状态的 TCP 连接数
	UDP  int    `json:"udp"` // UDP 连接数
}

// Candidate 是快照差集得出的"疑似远程工具"候选，供前端展示与生成规则。
type Candidate struct {
	ProcessName string `json:"processName"`
	ExePath     string `json:"exePath"`
	Cmdline     string `json:"cmdline"`
	IconDataURI string `json:"iconDataURI"`
	ChangeKind  string `json:"changeKind"` // "new"(新进程) | "conn"(连接数突增)
	TCPDelta    int    `json:"tcpDelta"`
	UDPDelta    int    `json:"udpDelta"`
	WindowTitle string `json:"windowTitle"`
	WindowClass string `json:"windowClass"`
}

// noiseProcesses 是快照差集时需要忽略的常见系统/浏览器进程（降噪）。
var noiseProcesses = map[string]bool{
	"explorer.exe": true, "svchost.exe": true, "conhost.exe": true,
	"chrome.exe": true, "msedge.exe": true, "firefox.exe": true,
	"dllhost.exe": true, "backgroundtaskhost.exe": true, "runtimebroker.exe": true,
	"searchhost.exe": true, "textinputhost.exe": true, "wmiprvse.exe": true,
	"taskhostw.exe": true, "sihost.exe": true, "csrss.exe": true, "smss.exe": true,
	"wininit.exe": true, "services.exe": true, "lsass.exe": true, "fontdrvhost.exe": true,
}

// enumerateAllProcesses 通过 Toolhelp32 枚举当前所有进程，返回 pid→进程名。
func enumerateAllProcesses() map[int32]string {
	result := make(map[int32]string)
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return result
	}
	defer windows.CloseHandle(snapshot)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snapshot, &pe); err != nil {
		return result
	}
	for {
		result[int32(pe.ProcessID)] = windows.UTF16ToString(pe.ExeFile[:])
		if err := windows.Process32Next(snapshot, &pe); err != nil {
			break
		}
	}
	return result
}

// SnapshotProcesses 拍一张进程快照：所有进程 + 每个进程的 ESTABLISHED TCP / UDP 连接数。
// 连接数通过一次性全表枚举（Connections("all")）后按 PID 聚合，避免对每个进程单独枚举。
func (d *WindowsDetector) SnapshotProcesses() map[int32]ProcSnap {
	procs := enumerateAllProcesses()
	snap := make(map[int32]ProcSnap, len(procs))
	for pid, name := range procs {
		snap[pid] = ProcSnap{PID: pid, Name: name}
	}

	conns, err := gopsutilnet.Connections("all")
	if err == nil {
		for _, c := range conns {
			if c.Pid == 0 {
				continue
			}
			s, ok := snap[c.Pid]
			if !ok {
				continue
			}
			switch c.Type {
			case syscall.SOCK_STREAM:
				if c.Status == "ESTABLISHED" {
					s.TCP++
				}
			case syscall.SOCK_DGRAM:
				s.UDP++
			}
			snap[c.Pid] = s
		}
	}
	return snap
}

// aggByName 把按 PID 的快照聚合成按进程名（小写）的快照，TCP/UDP 求和。
func aggByName(snap map[int32]ProcSnap) map[string]ProcSnap {
	m := make(map[string]ProcSnap)
	for _, s := range snap {
		key := strings.ToLower(s.Name)
		e := m[key]
		e.Name = s.Name
		e.TCP += s.TCP
		e.UDP += s.UDP
		m[key] = e
	}
	return m
}

// DiffSnapshots 比较基线与当前快照，得出疑似远程工具候选：
//   - 新进程：进程名在 after 出现、baseline 没有
//   - 连接突增：两次都在，但 TCP 或 UDP 连接数增长（覆盖"进程常驻、仅连接数涨"的工具，如网易UU）
//
// 候选以进程名为锚点，代表 PID 取 after 中连接数最多的同名进程，用于补全 exe/命令行/图标/窗口信息。
func (d *WindowsDetector) DiffSnapshots(baseline, after map[int32]ProcSnap) []Candidate {
	bName := aggByName(baseline)
	aName := aggByName(after)

	// 每个进程名在 after 里的代表 PID（连接数最多者优先，连接数相同取任意一个）
	repPID := make(map[string]int32)
	repScore := make(map[string]int)
	for _, s := range after {
		key := strings.ToLower(s.Name)
		score := s.TCP + s.UDP
		if pid, ok := repPID[key]; !ok || score > repScore[key] || pid == 0 {
			repPID[key] = s.PID
			repScore[key] = score
		}
	}

	var candidates []Candidate
	for key, a := range aName {
		if noiseProcesses[key] {
			continue
		}
		b, existed := bName[key]
		var kind string
		tcpDelta, udpDelta := a.TCP-b.TCP, a.UDP-b.UDP
		switch {
		case !existed:
			kind = "new"
			tcpDelta, udpDelta = a.TCP, a.UDP
		case tcpDelta >= 1 || udpDelta >= 1:
			kind = "conn"
		default:
			continue
		}

		c := Candidate{
			ProcessName: a.Name,
			ChangeKind:  kind,
			TCPDelta:    tcpDelta,
			UDPDelta:    udpDelta,
		}
		if pid, ok := repPID[key]; ok {
			d.enrichCandidate(&c, pid)
		}
		candidates = append(candidates, c)
	}

	// 排序：有网络活动的优先，其次新进程优先
	sort.SliceStable(candidates, func(i, j int) bool {
		ai, aj := candidates[i].TCPDelta+candidates[i].UDPDelta, candidates[j].TCPDelta+candidates[j].UDPDelta
		if ai != aj {
			return ai > aj
		}
		return candidates[i].ChangeKind == "new" && candidates[j].ChangeKind != "new"
	})
	return candidates
}

// enrichCandidate 为候选补全 exe 路径、命令行、图标与顶层窗口信息。
func (d *WindowsDetector) enrichCandidate(c *Candidate, pid int32) {
	if p, err := process.NewProcess(pid); err == nil {
		if exe, err := p.Exe(); err == nil {
			c.ExePath = exe
		}
		if cmd, err := p.Cmdline(); err == nil {
			c.Cmdline = cmd
		}
	}
	if c.ExePath != "" {
		c.IconDataURI = ExtractIconDataURI(c.ExePath)
	}
	c.WindowClass, c.WindowTitle = d.getTopWindowInfo(pid)
}

// getTopWindowInfo 返回指定进程第一个有标题的顶层窗口的类名与标题（用于预填窗口检测维度）。
func (d *WindowsDetector) getTopWindowInfo(pid int32) (className, title string) {
	enumProc := syscall.NewCallback(func(hwnd syscall.Handle, lParam uintptr) uintptr {
		var wpid uint32
		procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wpid)))
		if uint32(pid) != wpid {
			return 1
		}
		ret, _, _ := procGetWindowTextLength.Call(uintptr(hwnd))
		if ret <= 0 {
			return 1
		}
		length := int(ret) + 1
		buf := make([]uint16, length)
		r2, _, _ := procGetWindowText.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(length))
		if r2 <= 0 {
			return 1
		}
		title = windows.UTF16ToString(buf)
		cbuf := make([]uint16, 256)
		procGetClassName.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cbuf[0])), uintptr(len(cbuf)))
		className = windows.UTF16ToString(cbuf)
		return 0 // 找到即停止枚举
	})
	procEnumWindows.Call(enumProc, 0)
	return
}
