// SPDX-License-Identifier: Apache-2.0
// 基准引擎：CPU / 内存 / 磁盘 基准测试的实现（参考宝塔跑分 bt_score 思想，纯 Go）。
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"
)

// ---------- 可取消上下文（用于跑分中途终止/退出） ----------

type benchCtx struct {
	done chan struct{}
	once sync.Once
}

func (b *benchCtx) cancel() { b.once.Do(func() { close(b.done) }) }
func (b *benchCtx) closed() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

// ---------- CPU 基准 ----------

const cpuIterations = 3000000 // 3M 次混合运算

// cpuInt 整数运算：大量整数运算，衡量整数吞吐（百万元算/秒）。
func cpuInt() float64 {
	start := time.Now()
	var x int64 = 0
	i := 0
	for x = 0; x < 1e7; x++ {
		_ = x*x + 3*x + 7
		i++
	}
	elapsed := time.Since(start).Seconds()
	ops := float64(i)
	return ops / elapsed / 1e6 // 百万次/秒
}

// cpuFloat 浮点运算：浮点乘加与数学函数，衡量浮点吞吐。
func cpuFloat() float64 {
	start := time.Now()
	acc := 0.0
	for i := 0; i < cpuIterations; i++ {
		acc = math.Sin(float64(i))*1.0001 + math.Cos(float64(i))*0.9999
	}
	_ = acc
	elapsed := time.Since(start).Seconds()
	return float64(cpuIterations) / elapsed / 1e6 // 百万次/秒
}

// cpuPi 圆周率（级数迭代）：类似宝塔跑分的圆周率算法，衡量迭代计算能力。
func cpuPi() float64 {
	// 莱布尼茨级数 π/4 = 1 - 1/3 + 1/5 - 1/7 ...
	// 固定做 4M 次迭代，返回每秒百万次迭代
	const iters = 4000000
	start := time.Now()
	sum := 0.0
	den := 1.0
	sign := 1.0
	for i := 0; i < iters; i++ {
		sum += sign / den
		den += 2
		sign = -sign
	}
	_ = sum * 4
	elapsed := time.Since(start).Seconds()
	return float64(iters) / elapsed / 1e6
}

// cpuAll 综合 CPU 跑分，并行多核。
func (b *benchCtx) runCPU() map[string]float64 {
	n := runtime.GOMAXPROCS(0)
	if n < 1 {
		n = 1
	}
	res := map[string]float64{}

	// 整数：多核并行
	var wg sync.WaitGroup
	intVals := make([]float64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			intVals[idx] = cpuInt()
		}(i)
	}
	wg.Wait()
	var intSum float64
	for _, v := range intVals {
		intSum += v
	}
	res["int_mops"] = intSum

	// 浮点：多核并行
	floatVals := make([]float64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			floatVals[idx] = cpuFloat()
		}(i)
	}
	wg.Wait()
	var floatSum float64
	for _, v := range floatVals {
		floatSum += v
	}
	res["float_mops"] = floatSum

	// 圆周率单线程（代表单核迭代能力）
	pi := cpuPi()
	res["pi_miter"] = pi

	// 分数加权（对齐宝塔风格：CPU 子项按固定权重合成）
	res["score"] = intSum*1.0 + floatSum*0.8 + pi*1.2
	return res
}

// ---------- 内存基准 ----------

// memRw 读写带宽：对 N MB 的大数组做连续(逐字)顺序写/读，真实触碰全部内存，
// 返回写入/读取带宽(MB/s)。逐字累积校验和并用 KeepAlive 防止编译器消除读循环。
func memRw(mb int) (writeMBps, readMBps float64) {
	buf := make([]byte, mb<<20)
	const word = 8
	n := len(buf) / word
	// 写：逐字填充整块缓冲
	start := time.Now()
	var ws uint64
	for i := 0; i < n; i++ {
		v := uint64(i)
		binary.LittleEndian.PutUint64(buf[i*word:], v)
		ws ^= v
	}
	writeMBps = float64(mb) / time.Since(start).Seconds()
	runtime.KeepAlive(ws)

	// 读：逐字累积校验和，触摸全部内存且不可被优化掉
	var acc uint64
	start = time.Now()
	for i := 0; i < n; i++ {
		acc += binary.LittleEndian.Uint64(buf[i*word:])
	}
	readMBps = float64(mb) / time.Since(start).Seconds()
	runtime.KeepAlive(acc)
	return
}

// runMem 内存基准。
func (b *benchCtx) runMem() map[string]float64 {
	res := map[string]float64{}
	// 128MB 数组读写（设备内存充足场景）；内存过小自动降档
	memTotal := totalMemMB()
	mb := 128
	if memTotal > 0 && memTotal < 700 {
		mb = 64
	}
	if memTotal > 0 && memTotal < 300 {
		mb = 32
	}
	wr, rd := memRw(mb)
	res["write_mbps"] = wr
	res["read_mbps"] = rd
	res["score"] = (wr + rd) / 2
	return res
}

// ---------- 磁盘基准 ----------

// benchDisk 磁盘 IO：顺序写/读 + 4K 随机读/写，返回 MB/s 与 IOPS。
// dir 为测试目录，sizeMB 为测试文件大小。
func (b *benchCtx) benchDisk(dir string, sizeMB int) (map[string]float64, error) {
	res := map[string]float64{}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}
	final, err := filepath.Abs(filepath.Join(dir, "bench.tmp"))
	if err != nil {
		return res, err
	}

	block := make([]byte, 1<<20) // 1MB
	for i := range block {
		block[i] = byte(i)
	}

	// 顺序写
	f, err := os.OpenFile(final, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return res, err
	}
	start := time.Now()
	total := sizeMB
	for i := 0; i < total; i++ {
		if _, err := f.Write(block); err != nil {
			f.Close()
			os.Remove(final)
			return res, err
		}
		if b.closed() {
			f.Close()
			os.Remove(final)
			return res, fmt.Errorf("cancelled")
		}
	}
	f.Sync()
	seqWrite := float64(total) / time.Since(start).Seconds()
	f.Close()

	// 顺序读
	rf, err := os.Open(final)
	if err != nil {
		os.Remove(final)
		return res, err
	}
	start = time.Now()
	rblock := make([]byte, 1<<20)
	for i := 0; i < total; i++ {
		if _, err := rf.Read(rblock); err != nil {
			break
		}
	}
	seqRead := float64(total) / time.Since(start).Seconds()
	res["seq_write_mbps"] = seqWrite
	res["seq_read_mbps"] = seqRead
	rf.Close()

	// 随机读 + 4K 读（IOPS）
	iopsCtx := &benchCtx{done: b.done}
	rd4k, rd4kIOPS := randomRead4K(final, iopsCtx)
	res["rand4k_read_iops"] = rd4kIOPS
	res["rand4k_read_mbps"] = rd4k

	// 4K 随机写（IOPS）
	wr4k, wr4kIOPS := randomWrite4K(final, iopsCtx)
	res["rand4k_write_iops"] = wr4kIOPS
	res["rand4k_write_mbps"] = wr4k

	// 磁盘分数：宝塔风格 = IO 响应（MB/s 加权 + IOPS）
	res["score"] = (seqWrite+seqRead)*0.5 + (rd4k+wr4k)*0.2 + (rd4kIOPS+wr4kIOPS)/1000

	os.Remove(final)
	return res, nil
}

// randomRead4K 4K 随机读：读取 size 次 4K 块，返回 MB/s 与 IOPS。
func randomRead4K(path string, b *benchCtx) (float64, float64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	const blockSize = 4096
	const reads = 8192
	buf := make([]byte, blockSize)
	info, _ := f.Stat()
	sz := info.Size()
	if sz <= 0 {
		return 0, 0
	}
	start := time.Now()
	for i := 0; i < reads; i++ {
		off := int64(i*blockSize) % sz
		if _, err := f.ReadAt(buf, off); err != nil {
			break
		}
		if b.closed() {
			return 0, 0
		}
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	mbs := float64(reads*blockSize) / elapsed / 1e6
	iops := float64(reads) / elapsed
	return mbs, iops
}

// randomWrite4K 4K 随机写（追加新文件，衡量写 IOPS）。
func randomWrite4K(path string, b *benchCtx) (float64, float64) {
	f, err := os.Create(path + ".w")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	defer os.Remove(path + ".w")
	const blockSize = 4096
	const writes = 4096
	buf := make([]byte, blockSize)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	start := time.Now()
	for i := 0; i < writes; i++ {
		if _, err := f.WriteAt(buf, int64(i)*blockSize); err != nil {
			break
		}
		if b.closed() {
			return 0, 0
		}
	}
	f.Sync()
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, 0
	}
	mbs := float64(writes*blockSize) / elapsed / 1e6
	iops := float64(writes) / elapsed
	return mbs, iops
}

// ---------- 系统辅助 ----------

// totalMemMB 以 MB 为单位返回物理内存总量（读 /proc/meminfo）。
func totalMemMB() float64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range stringsSplitLines(string(b)) {
		var kb int
		if n, _ := fmt.Sscanf(line, "MemTotal: %d kB", &kb); n == 1 && kb > 0 {
			return float64(kb) / 1024
		}
	}
	return 0
}

func stringsSplitLines(s string) []string {
	var out []string
	cur := ""
	for _, c := range s {
		if c == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// format 汇总排序用。
func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}