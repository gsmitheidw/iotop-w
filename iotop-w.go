// iotop-w  G. Smith, Feb2026.
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ----------------------- VERSION -----------------------
const (
	Version     = "1.03"
	DefaultTopN = 5
	MaxTopN     = 20
	RepoURL     = "https://github.com/gsmitheidw/iotop-w"
	License     = "MIT License"
)

// ----------------------- SETTINGS -----------------------
const (
	historyWidth    = 30
	minInterval     = 100 * time.Millisecond
	maxInterval     = 10 * time.Second
	queueSaturation = 2.0
	maxNameLen      = 16 // truncate long process names
)

// ----------------------- VISUALIZATION MODES -----------------------
const (
	ModeBraille = 0
	ModeBlocks  = 1
)

// ----------------------- SOLARIZED DARK PALETTE -----------------------
var (
	Base03 = "\x1b[38;5;234m"
	Base0  = "\x1b[38;5;250m"
	Green  = "\x1b[38;5;64m"
	Yellow = "\x1b[38;5;136m"
	Orange = "\x1b[38;5;166m"
	Red    = "\x1b[38;5;160m"
	Blue   = "\x1b[38;5;33m"
	Cyan   = "\x1b[38;5;37m"
	Reset  = "\x1b[0m"
)

// ----------------------- BRAILLE SPARKLINE -----------------------
var braille = []rune{
	0x2800, 0x2801, 0x2803, 0x2807,
	0x2817, 0x2837, 0x2877, 0x28F7, 0x28FF,
}

// ----------------------- BLOCK SPARKLINE -----------------------
var blocks = []rune{
	' ', '▁', '▂', '▃', '▄', '▅', '▆', '▇', '█',
}

type Cell struct {
	Level float64
	Color string
}

type Ring struct {
	buf  []Cell
	head int
}

func newRing(n int) Ring { return Ring{buf: make([]Cell, n)} }

func (r *Ring) push(c Cell) {
	r.buf[r.head] = c
	r.head = (r.head + 1) % len(r.buf)
}

func (r Ring) renderBraille() string {
	var b strings.Builder
	for i := 0; i < len(r.buf); i++ {
		c := r.buf[(r.head+i)%len(r.buf)]
		idx := int(c.Level + 0.5)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(braille) {
			idx = len(braille) - 1
		}
		b.WriteString(c.Color)
		b.WriteRune(braille[idx])
		b.WriteString(Reset)
	}
	return b.String()
}

func (r Ring) renderBlocks() string {
	var b strings.Builder
	for i := 0; i < len(r.buf); i++ {
		c := r.buf[(r.head+i)%len(r.buf)]
		idx := int(c.Level + 0.5)
		if idx < 0 {
			idx = 0
		}
		if idx >= len(blocks) {
			idx = len(blocks) - 1
		}
		// Monochrome blocks - use Base0 for all
		b.WriteString(Base0)
		b.WriteRune(blocks[idx])
		b.WriteString(Reset)
	}
	return b.String()
}

func (r Ring) render(mode int) string {
	switch mode {
	case ModeBlocks:
		return r.renderBlocks()
	default:
		return r.renderBraille()
	}
}

// ----------------------- HELPERS -----------------------
func scale(v, max float64) float64 {
	if max <= 0 {
		return 0
	}
	return v * float64(len(braille)-1) / max
}

func scaleBlocks(v, max float64) float64 {
	if max <= 0 {
		return 0
	}
	return v * float64(len(blocks)-1) / max
}

func colorRatio(r float64) string {
	switch {
	case r > 1.0:
		return Red
	case r > 0.75:
		return Orange
	case r > 0.5:
		return Yellow
	case r > 0.25:
		return Green
	default:
		return Base0
	}
}

// formatBytes formats bytes/sec into human-readable form
func formatBytes(bytes float64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%.0f B/s", bytes)
	}
	div, exp := unit, 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB/s", bytes/float64(div), "KMGTPE"[exp])
}

// Allowed intervals for stepping
var allowedIntervals = []time.Duration{
	100 * time.Millisecond,
	200 * time.Millisecond,
	300 * time.Millisecond,
	400 * time.Millisecond,
	500 * time.Millisecond,
	600 * time.Millisecond,
	700 * time.Millisecond,
	800 * time.Millisecond,
	900 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// nextInterval strictly steps through allowedIntervals
func nextInterval(curr time.Duration, up bool) time.Duration {
	for i, v := range allowedIntervals {
		if curr == v {
			if up && i < len(allowedIntervals)-1 {
				return allowedIntervals[i+1]
			}
			if !up && i > 0 {
				return allowedIntervals[i-1]
			}
			return curr // at boundary, do nothing
		}
	}
	return curr // do not jump if curr is not exact
}

// disableEcho turns off line input & echo, returns restore function
func disableEcho() func() {
	h, _ := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	var mode uint32
	windows.GetConsoleMode(h, &mode)
	orig := mode
	mode &^= windows.ENABLE_ECHO_INPUT | windows.ENABLE_LINE_INPUT
	windows.SetConsoleMode(h, mode)
	return func() { windows.SetConsoleMode(h, orig) }
}

// ----------------------- WIN32 / PDH -----------------------
var (
	k32 = windows.NewLazySystemDLL("kernel32.dll")
	pdh = windows.NewLazySystemDLL("pdh.dll")

	pGetIOCounters                 = k32.NewProc("GetProcessIoCounters")
	pGetNumberOfConsoleInputEvents = k32.NewProc("GetNumberOfConsoleInputEvents")
	pReadConsoleInputW             = k32.NewProc("ReadConsoleInputW")

	pPdhOpenQuery  = pdh.NewProc("PdhOpenQueryW")
	pPdhAddCounter = pdh.NewProc("PdhAddEnglishCounterW")
	pPdhCollect    = pdh.NewProc("PdhCollectQueryData")
	pPdhGetValue   = pdh.NewProc("PdhGetFormattedCounterValue")
	pPdhCloseQuery = pdh.NewProc("PdhCloseQuery")
)

// ----------------------- CONSOLE INPUT EVENTS -----------------------
const (
	KEY_EVENT = 0x0001
)

type inputRecord struct {
	EventType uint16
	_         [2]byte
	Event     [16]byte
}

type keyEventRecord struct {
	KeyDown         int32
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	Char            uint16
	ControlKeyState uint32
}

// readConsoleKey checks for key presses via console input events
func readConsoleKey() (rune, bool) {
	h, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return 0, false
	}

	var numEvents uint32
	r, _, _ := pGetNumberOfConsoleInputEvents.Call(uintptr(h), uintptr(unsafe.Pointer(&numEvents)))
	if r == 0 || numEvents == 0 {
		return 0, false
	}

	records := make([]inputRecord, 1)
	var read uint32
	r, _, _ = pReadConsoleInputW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&records[0])),
		1,
		uintptr(unsafe.Pointer(&read)),
	)
	if r == 0 || read == 0 {
		return 0, false
	}

	rec := records[0]
	if rec.EventType != KEY_EVENT {
		return 0, false
	}

	// Parse key event
	keyEvent := (*keyEventRecord)(unsafe.Pointer(&rec.Event[0]))
	if keyEvent.KeyDown == 0 {
		return 0, false // only care about key down
	}

	return rune(keyEvent.Char), true
}

// ----------------------- TYPES -----------------------
type ProcHist struct {
	Read, Write Ring
	Max         float64 // now in bytes/sec
	LastSeen    time.Time
}

type ProcIO struct {
	PID   uint32
	Name  string
	Read  uint64
	Write uint64
}

type Snapshot struct {
	Data      map[uint32]ProcIO
	Timestamp time.Time
}

type Rates struct {
	PID   uint32
	Name  string
	Read  float64 // bytes/sec
	Write float64 // bytes/sec
	Total float64 // bytes/sec
}

type DiskQueue struct {
	query   uintptr
	counter uintptr
}

// ----------------------- PROCESS HANDLE CACHE -----------------------
type HandleCache struct {
	handles map[uint32]windows.Handle
}

func newHandleCache() *HandleCache {
	return &HandleCache{handles: make(map[uint32]windows.Handle)}
}

func (hc *HandleCache) get(pid uint32) (windows.Handle, error) {
	if h, exists := hc.handles[pid]; exists {
		return h, nil
	}
	// Try to open the handle
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	hc.handles[pid] = h
	return h, nil
}

func (hc *HandleCache) close(pid uint32) {
	if h, exists := hc.handles[pid]; exists {
		windows.CloseHandle(h)
		delete(hc.handles, pid)
	}
}

func (hc *HandleCache) closeAll() {
	for pid, h := range hc.handles {
		windows.CloseHandle(h)
		delete(hc.handles, pid)
	}
}

func (hc *HandleCache) cleanup(validPIDs map[uint32]bool) {
	for pid, h := range hc.handles {
		if !validPIDs[pid] {
			windows.CloseHandle(h)
			delete(hc.handles, pid)
		}
	}
}

// ----------------------- DISK QUEUE -----------------------
func newDiskQueue() *DiskQueue {
	var q uintptr
	pPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q)))
	path, _ := windows.UTF16PtrFromString(`\PhysicalDisk(_Total)\Avg. Disk Queue Length`)
	var c uintptr
	pPdhAddCounter.Call(q, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&c)))
	pPdhCollect.Call(q)
	return &DiskQueue{query: q, counter: c}
}

func (d *DiskQueue) read() float64 {
	pPdhCollect.Call(d.query)
	var val struct {
		CStatus     uint32
		DoubleValue float64
	}
	pPdhGetValue.Call(d.counter, 0x00000200, 0, uintptr(unsafe.Pointer(&val)))
	return val.DoubleValue
}

func (d *DiskQueue) close() {
	pPdhCloseQuery.Call(d.query)
}

// ----------------------- DISK BAR -----------------------
func renderDiskBar(depth float64, width int, maxQueue float64) string {
	level := depth / maxQueue
	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}

	filled := int(level * float64(width))
	bar := make([]string, width)
	for i := range bar {
		if i < filled {
			bar[i] = Red + "█" + Reset
		} else {
			bar[i] = Base03 + "█" + Reset
		}
	}
	
	// Clean bar only - no confusing numbers
	return fmt.Sprintf("%sDisk Pressure:%s %s", Blue, Reset, strings.Join(bar, ""))
}

// ----------------------- PROCESS SNAPSHOT -----------------------
func snapshotIO(cache *HandleCache) Snapshot {
	out := make(map[uint32]ProcIO)
	snap, _ := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if windows.Process32First(snap, &pe) != nil {
		return Snapshot{Data: out, Timestamp: time.Now()}
	}

	validPIDs := make(map[uint32]bool)
	for {
		validPIDs[pe.ProcessID] = true

		h, err := cache.get(pe.ProcessID)
		if err == nil {
			var io windows.IO_COUNTERS
			if r, _, _ := pGetIOCounters.Call(uintptr(h), uintptr(unsafe.Pointer(&io))); r != 0 {
				out[pe.ProcessID] = ProcIO{
					PID:   pe.ProcessID,
					Name:  windows.UTF16ToString(pe.ExeFile[:]),
					Read:  io.ReadTransferCount,
					Write: io.WriteTransferCount,
				}
			}
		}

		if windows.Process32Next(snap, &pe) != nil {
			break
		}
	}

	// Clean up handles for PIDs that no longer exist
	cache.cleanup(validPIDs)

	return Snapshot{Data: out, Timestamp: time.Now()}
}

// ----------------------- MAIN -----------------------
func main() {
	top := DefaultTopN
	help := false
	showVersion := false
	showInfo := false
	visualMode := ModeBraille // default to braille

	for i := 1; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--help" || os.Args[i] == "-h":
			help = true
		case os.Args[i] == "--version" || os.Args[i] == "-v":
			showVersion = true
		case os.Args[i] == "--info" || os.Args[i] == "-i":
			showInfo = true
		case os.Args[i] == "--blocks" || os.Args[i] == "-b":
			visualMode = ModeBlocks
		case os.Args[i] == "--top":
			if i+1 < len(os.Args) {
				n, err := strconv.Atoi(os.Args[i+1])
				if err == nil && n > 0 {
					if n > MaxTopN {
						top = MaxTopN
					} else {
						top = n
					}
				}
				i++
			}
		case strings.HasPrefix(os.Args[i], "--top="):
			n, err := strconv.Atoi(strings.TrimPrefix(os.Args[i], "--top="))
			if err == nil && n > 0 {
				if n > MaxTopN {
					top = MaxTopN
				} else {
					top = n
				}
			}
		}
	}

	if help {
		fmt.Printf(`iotop-w %s
Usage: iotop-w [options]

Options:
  --help, -h       Show this help message
  --version, -v    Show version
  --info, -i       Show repo info and license
  --top <number>   Show top <number> processes (max %d)
  --blocks, -b     Use block-style visualization (default: braille)
`, Version, MaxTopN)
		return
	}

	if showVersion {
		fmt.Println("iotop-w version", Version)
		return
	}

	if showInfo {
		fmt.Printf("iotop-w repo: %s\nLicense: %s\n", RepoURL, License)
		return
	}

	h, _ := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	var mode uint32
	windows.GetConsoleMode(h, &mode)
	windows.SetConsoleMode(h, mode|0x0004)

	restore := disableEcho()
	defer func() {
		restore()
		windows.FlushConsoleInputBuffer(windows.STD_INPUT_HANDLE)
		fmt.Print("\x1b[0m\x1b[H\x1b[J\x1b[?25h") // reset colors, clear screen & show cursor
	}()
	fmt.Print("\x1b[?25l")

	disk := newDiskQueue()
	defer disk.close()

	cache := newHandleCache()
	defer cache.closeAll()

	interval := 1 * time.Second // start at usable default
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prevSnap := snapshotIO(cache)
	procs := map[uint32]*ProcHist{}

	for {
		select {
		case <-ticker.C:
			fmt.Print("\x1b[H\x1b[J")
			fmt.Printf("%s〘iotop-w〙 %s %s\n\n", Blue, Version, Reset)

			q := disk.read()
			fmt.Println(renderDiskBar(q, 30, queueSaturation))
			fmt.Println()
			
			currSnap := snapshotIO(cache)
			elapsed := currSnap.Timestamp.Sub(prevSnap.Timestamp).Seconds()

			if elapsed <= 0 {
				elapsed = 0.001 // prevent division by zero
			}

			var rates []Rates
			for pid, now := range currSnap.Data {
				old, ok := prevSnap.Data[pid]
				if !ok {
					continue
				}
				rDelta := float64(now.Read - old.Read)
				wDelta := float64(now.Write - old.Write)
				if rDelta+wDelta == 0 {
					continue
				}

				// Calculate actual rates in bytes/sec
				rRate := rDelta / elapsed
				wRate := wDelta / elapsed

				rates = append(rates, Rates{
					PID:   pid,
					Name:  now.Name,
					Read:  rRate,
					Write: wRate,
					Total: rRate + wRate,
				})
			}

			sort.Slice(rates, func(i, j int) bool { return rates[i].Total > rates[j].Total })

			fmt.Printf("%s%-5s │ %-*s │ %-*s │ %-*s%s\n",
				Blue, "PID", maxNameLen, "Name", historyWidth, "Read", historyWidth, "Write", Reset)

			now := time.Now()
			for i := 0; i < top && i < len(rates); i++ {
				r := rates[i]
				h := procs[r.PID]
				if h == nil {
					h = &ProcHist{
						Read:     newRing(historyWidth),
						Write:    newRing(historyWidth),
						Max:      1.0,
						LastSeen: now,
					}
					procs[r.PID] = h
				}
				h.LastSeen = now

				// Update max with proper bytes/sec rates
				if r.Total > h.Max {
					h.Max = r.Total
				} else {
					h.Max = h.Max * 0.95
				}

				// Scale appropriately based on mode
				var readScale, writeScale float64
				if visualMode == ModeBlocks {
					readScale = scaleBlocks(r.Read, h.Max)
					writeScale = scaleBlocks(r.Write, h.Max)
				} else {
					readScale = scale(r.Read, h.Max)
					writeScale = scale(r.Write, h.Max)
				}

				h.Read.push(Cell{readScale, colorRatio(r.Read / h.Max)})
				h.Write.push(Cell{writeScale, colorRatio(r.Write / h.Max)})

				nameRunes := []rune(r.Name)
				if len(nameRunes) > maxNameLen {
					nameRunes = nameRunes[:maxNameLen-1]
					nameRunes = append(nameRunes, '…')
				}
				displayName := string(nameRunes)

				fmt.Printf("%-5d │ %-*s │ %-*s │ %-*s\n",
					r.PID, maxNameLen, displayName, historyWidth, 
					h.Read.render(visualMode), historyWidth, h.Write.render(visualMode))
			}

			// Clean up old process history
			for pid, h := range procs {
				if now.Sub(h.LastSeen) > 30*time.Second {
					delete(procs, pid)
				}
			}

			prevSnap = currSnap

			// Display interval nicely
			var intervalStr string
			if interval < time.Second {
				intervalStr = fmt.Sprintf("%dms", interval.Milliseconds())
			} else {
				intervalStr = fmt.Sprintf("%.0fs", interval.Seconds())
			}

			// Bubble tea style controls bar - properly aligned
			fmt.Printf("\n%s╭────────────────────────────────────────────────────╮%s\n", Base0, Reset)
			fmt.Printf("%s│%s Interval: %s%-5s%s%s│%s %s+/-%s %sAdjust%s %s│%s %ss%s %sStyle%s %s│%s %sq%s %sQuit%s     %s│%s\n",
				Base0, Reset,
				Blue, intervalStr, Reset,
				Base0, Reset,
				Blue, Reset, Base0, Reset,
				Base0, Reset,
				Blue, Reset, Base0, Reset,
				Base0, Reset,
				Blue, Reset, Base0, Reset,
				Base0, Reset)
			fmt.Printf("%s╰────────────────────────────────────────────────────╯%s\n", Base0, Reset)

		default:
			// Check for console input events
			if ch, ok := readConsoleKey(); ok {
				ch = rune(strings.ToLower(string(ch))[0])
				switch ch {
				case 'q':
					fmt.Print("\x1b[0m\x1b[H\x1b[J\x1b[?25h") // reset colors, clear, show cursor
					return
				case '+', '=':
					interval = nextInterval(interval, true)
					ticker.Reset(interval)
				case '-', '_':
					interval = nextInterval(interval, false)
					ticker.Reset(interval)
				case 's':
					// Toggle visualization mode
					if visualMode == ModeBraille {
						visualMode = ModeBlocks
					} else {
						visualMode = ModeBraille
					}
				}
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
}
