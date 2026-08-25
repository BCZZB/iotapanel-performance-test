// SPDX-License-Identifier: Apache-2.0
// App：路由、一键跑分调度（异步 + 进度）、历史持久化、系统信息采集。
package main

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ---------- 结果模型 ----------

type CombineResult struct {
	Cpu   float64            `json:"cpu"`
	Mem   float64            `json:"mem"`
	Disk  float64            `json:"disk"`
	Total float64            `json:"total"`
}

type BenchRun struct {
	ID        string                     `json:"id"`
	StartedAt time.Time                  `json:"started_at"`
	Finished  bool                       `json:"finished"`
	Error     string                     `json:"error,omitempty"`
	Progress  int                        `json:"progress"` // 0-100
	Stage     string                     `json:"stage"`
	Hardware  HardwareInfo               `json:"hardware"`
	Cpu       map[string]float64         `json:"cpu,omitempty"`
	Mem       map[string]float64         `json:"mem,omitempty"`
	Disk      map[string]float64         `json:"disk,omitempty"`
	Combine   CombineResult             `json:"combine,omitempty"`
}

// HardwareInfo 硬件信息快照。
type HardwareInfo struct {
	CPU        string `json:"cpu"`
	Core       int    `json:"cores"`
	MemTotalMB int    `json:"mem_total_mb"`
	Arch       string `json:"arch"`
	GoVersion  string `json:"go_version"`
	Hostname   string `json:"hostname"`
}

// ---------- App ----------

type App struct {
	pluginName string
	dataDir    string
	mu         sync.Mutex
	current    *BenchRun
	cancel     chan struct{}
	cancelOnce sync.Once
}

func NewApp(home, pluginName string) (*App, error) {
	dataDir := filepath.Join(home, "data", pluginName)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return &App{
		pluginName: pluginName,
		dataDir:    dataDir,
	}, nil
}

func (a *App) cancelRun() {
	a.cancelOnce.Do(func() {
		if a.cancel != nil {
			close(a.cancel)
		}
	})
}

func (a *App) historyFile() string { return filepath.Join(a.dataDir, "history.json") }

// ---------- 路由 ----------

func (a *App) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /api/status", a.handleStatus)
	mux.HandleFunc("POST /api/bench", a.handleBench)
	mux.HandleFunc("POST /api/cancel", a.handleCancel)
	mux.HandleFunc("GET /api/history", a.handleHistory)
	mux.HandleFunc("GET /api/meta", a.handleMeta)

	// 静态资源（web/ 下非 index）
	sub, err := fs.Sub(webFS, "web")
	if err == nil {
		static := http.StripPrefix("", http.FileServer(http.FS(sub)))
		mux.Handle("/assets/", static)
	}
	return a.secure(mux)
}

func (a *App) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func jsonOK(w http.ResponseWriter, v any) { writeJSON(w, 200, map[string]any{"ok": true, "data": v}) }
func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

// ---------- Handlers ----------

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "index not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (a *App) handleMeta(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]any{"name": a.pluginName, "version": "1.0.0", "totalMemMB": int(totalMemMB()), "cores": runtime.GOMAXPROCS(0)})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	cur := a.current
	a.mu.Unlock()
	if cur != nil {
		jsonOK(w, cur)
		return
	}
	jsonOK(w, map[string]any{"running": false, "progress": 0, "stage": "idle"})
}

func (a *App) handleBench(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.current != nil {
		a.mu.Unlock()
		jsonErr(w, 409, "已有跑分正在执行")
		return
	}
	a.cancelOnce = sync.Once{}
	cancel := make(chan struct{})
	a.cancel = cancel
	run := &BenchRun{
		ID:        fmtTime(time.Now()),
		StartedAt: time.Now(),
		Stage:     "初始化",
		Progress:  0,
		Hardware:  collectHardware(),
	}
	a.current = run
	a.mu.Unlock()

	go a.execute(run, cancel)
	jsonOK(w, map[string]any{"id": run.ID, "started": true, "progress": 0})
}

func (a *App) handleCancel(w http.ResponseWriter, r *http.Request) {
	a.cancelRun()
	jsonOK(w, map[string]any{"cancelled": true})
}

func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// 若当前已完成未入历史，先入
	if a.current != nil && a.current.Finished {
		a.appendHistoryLocked(a.current)
		a.current = nil
	}
	hist := a.readHistoryLocked()
	if hist == nil {
		hist = []*BenchRun{}
	}
	jsonOK(w, hist)
}

// ---------- 执行 ----------

func (a *App) execute(run *BenchRun, cancel chan struct{}) {
	bctx := &benchCtx{done: cancel}
	setProgress := func(p int, s string) {
		a.mu.Lock()
		run.Progress = p
		run.Stage = s
		a.mu.Unlock()
	}

	// CPU
	setProgress(2, "CPU 基准测试")
	cpu := bctx.runCPU()
	setProgress(35, "内存基准测试")

	// 内存
	mem := bctx.runMem()
	setProgress(55, "磁盘基准测试")

	// 磁盘（放数据目录下，避免读写系统盘根分区干扰）
	diskDir := filepath.Join(a.dataDir, "bench")
	var disk map[string]float64
	var err error
	if bctx.closed() {
		err = os.ErrClosed
	} else {
		disk, err = bctx.benchDisk(diskDir, 256)
	}
	setProgress(85, "汇总成绩")

	// 总分
	combine := CombineResult{
		Cpu:   cpu["score"],
		Mem:   mem["score"],
		Disk:  diskOrZero(disk, "score"),
	}
	combine.Total = combine.Cpu + combine.Mem + combine.Disk

	a.mu.Lock()
	run.Cpu = cpu
	run.Mem = mem
	run.Disk = disk
	run.Combine = combine
	run.Progress = 100
	run.Stage = "完成"
	run.Finished = true
	if err != nil && err != os.ErrClosed {
		run.Error = err.Error()
	}
	// 入历史
	a.appendHistoryLocked(run)
	a.mu.Unlock()
	log.Printf("[performance-test] 跑分完成 total=%.0f (cpu=%.0f mem=%.0f disk=%.0f)",
		combine.Total, combine.Cpu, combine.Mem, combine.Disk)
}

func diskOrZero(m map[string]float64, k string) float64 {
	if m == nil {
		return 0
	}
	return m[k]
}

func fmtTime(t time.Time) string { return t.Format("20060102-150405") }

// ---------- 历史持久化 ----------

func (a *App) appendHistoryLocked(run *BenchRun) {
	hist := a.readHistoryLocked()
	hist = append(hist, run)
	if len(hist) > 20 {
		hist = hist[len(hist)-20:]
	}
	a.writeHistoryLocked(hist)
}

func (a *App) readHistoryLocked() []*BenchRun {
	b, err := os.ReadFile(a.historyFile())
	if err != nil {
		return nil
	}
	var hist []*BenchRun
	if json.Unmarshal(b, &hist) != nil {
		return nil
	}
	return hist
}

func (a *App) writeHistoryLocked(hist []*BenchRun) {
	b, _ := json.MarshalIndent(hist, "", "  ")
	tmp := a.historyFile() + ".tmp"
	if os.WriteFile(tmp, b, 0600) == nil {
		os.Rename(tmp, a.historyFile())
	}
}

// ---------- 硬件采集 ----------

func collectHardware() HardwareInfo {
	host, _ := os.Hostname()
	mem := int(totalMemMB())
	cpuName := readCPUName()
	return HardwareInfo{
		CPU:        cpuName,
		Core:       runtime.NumCPU(),
		MemTotalMB: mem,
		Arch:       runtime.GOARCH,
		GoVersion:  strings.ReplaceAll(runtime.Version(), "go", "Go "),
		Hostname:   host,
	}
}

func readCPUName() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range stringsSplitLines(string(b)) {
		if strings.HasPrefix(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}