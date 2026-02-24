package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"
)

// ===== eBPF event (커널 struct event와 동일한 레이아웃) =====
// struct event {
//   __u64 ts;
//   __u32 cpu;
//   __u32 pad;
//   __s64 runtime_ns;
//   __u32 _pad;
//   __u64 tg_ptr;
//   __u64 cgid;
// };
type Event struct {
	TS        uint64
	CPU       uint32
	NrRunning uint32  // RT tasks running count
	RuntimeNS int64
	Pad1      uint32
	Pad2      uint32
	TgPtr     uint64
	Cgid      uint64
}

// ===== cgroup v2 mountpoint 찾기 =====
func findCgroup2Mountpoint() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()

	sc := NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.Split(line, " - ")
		if len(parts) != 2 {
			continue
		}
		right := strings.Fields(parts[1]) // "fstype source superopts"
		if len(right) < 1 || right[0] != "cgroup2" {
			continue
		}
		left := strings.Fields(parts[0])
		if len(left) >= 5 {
			return left[4], nil // mountpoint
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("cgroup2 mountpoint not found")
}

// 간단 스캐너(버퍼 큰 줄도 안전하게)
type Scanner struct {
	r   *os.File
	buf []byte
	err error
}

func NewScanner(f *os.File) *Scanner {
	return &Scanner{r: f}
}
func (s *Scanner) Scan() bool {
	s.buf = s.buf[:0]
	tmp := make([]byte, 4096)
	for {
		n, err := s.r.Read(tmp)
		if n > 0 {
			idx := bytes.IndexByte(tmp[:n], '\n')
			if idx >= 0 {
				s.buf = append(s.buf, tmp[:idx]...)
				// seek back leftover
				if _, e := s.r.Seek(int64(idx-n+1), 1); e != nil {
					s.err = e
					return true
				}
				return true
			}
			s.buf = append(s.buf, tmp[:n]...)
			continue
		}
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return false
			}
			if len(s.buf) > 0 {
				return true
			}
			s.err = err
			return false
		}
	}
}
func (s *Scanner) Text() string { return string(s.buf) }
func (s *Scanner) Err() error   { return s.err }

// ===== cgid(inode) → path 캐시 =====
type PathCache struct {
	root  string
	byIno map[uint64]string
}

func buildPathCache(root string) (*PathCache, error) {
	pc := &PathCache{
		root:  root,
		byIno: make(map[uint64]string, 4096),
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		var st unix.Stat_t
		if err := unix.Stat(p, &st); err == nil {
			pc.byIno[uint64(st.Ino)] = p
		}
		return nil
	})
	return pc, err
}

func (pc *PathCache) Lookup(cgid uint64) (string, bool) {
	p, ok := pc.byIno[cgid]
	return p, ok
}

func (pc *PathCache) Refresh(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		var st unix.Stat_t
		if unix.Stat(p, &st) == nil {
			pc.byIno[uint64(st.Ino)] = p
		}
		return nil
	})
}

// ===== 경로에서 Pod UID 추출 =====
// v2(systemd) 예: .../kubepods-burstable-pod7c4e846a80d6368e87772b6039dc06e8.slice/...
// v1(docker)   예: .../kubepods/burstable/pod7c4e846a-80d6-36e8-8777-2b6039dc06e8/...
var (
	// 32 hex (no dash)
	rePodHex32 = regexp.MustCompile(`pod([a-f0-9]{32})`)
	// UUID with dashes
	rePodUUID = regexp.MustCompile(`pod([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})`)

	reContainerID = regexp.MustCompile(`(?:cri-containerd-|docker-|crio-)([a-f0-9]{64})`)
)

func extractPodUID(path string) (podUID string, ok bool) {
	// 1) dashed UUID
	if m := rePodUUID.FindStringSubmatch(path); m != nil {
		return m[1], true
	}
	// 2) 32hex → UUID 형태로 변환(8-4-4-4-12)
	if m := rePodHex32.FindStringSubmatch(path); m != nil {
		raw := m[1]
		if len(raw) == 32 {
			pod := fmt.Sprintf("%s-%s-%s-%s-%s",
				raw[0:8], raw[8:12], raw[12:16], raw[16:20], raw[20:32])
			return pod, true
		}
	}
	return "", false
}

func extractContainerID(path string) (containerID string, ok bool) {
	if m := reContainerID.FindStringSubmatch(path); m != nil {
		return m[1], true
	}
	return "", false
}

// ===== Controller POST (timestamp 포함) =====
func postToController(url, CID string, e Event, cgidPath string) {
	if url == "" {
		return
	}
	body := fmt.Sprintf(`{"container_id":%q,"timestamp":%d}`, CID, e.TS)
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("controller POST err: %v", err)
		return
	}
	_ = resp.Body.Close()
}

// ===== eBPF 로드/부착 =====
func loadAndAttach(objPath string) (*ebpf.Collection, link.Link, *ringbuf.Reader, error) {
	spec, err := ebpf.LoadCollectionSpec(objPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load spec: %w", err)
	}
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("new collection: %w", err)
	}
	prog := coll.Programs["on_update_curr_dl_se"]
	if prog == nil {
		coll.Close()
		return nil, nil, nil, fmt.Errorf("program on_update_curr_dl_se not found")
	}
	l, err := link.AttachTracing(link.TracingOptions{Program: prog})
	if err != nil {
		coll.Close()
		return nil, nil, nil, fmt.Errorf("attach fentry: %w", err)
	}
	evMap := coll.Maps["events"]
	if evMap == nil {
		l.Close()
		coll.Close()
		return nil, nil, nil, fmt.Errorf("map events not found")
	}
	rb, err := ringbuf.NewReader(evMap)
	if err != nil {
		l.Close()
		coll.Close()
		return nil, nil, nil, fmt.Errorf("ringbuf: %w", err)
	}
	return coll, l, rb, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ===== 통계 저장 함수 =====
func saveStats(overrunCount map[string]int, totalCnt int) {
	timestamp := time.Now().Format("20060102_150405")
	filename := fmt.Sprintf("overrun_stats_%s.log", timestamp)
	
	f, err := os.Create(filename)
	if err != nil {
		log.Printf("Failed to create stats file: %v", err)
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== HCBS Overrun Statistics ===\n")
	fmt.Fprintf(f, "Timestamp: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(f, "Total Overruns: %d\n", totalCnt)
	fmt.Fprintf(f, "Unique Containers: %d\n\n", len(overrunCount))
	
	// Container별 정렬하여 출력
	type ContainerStat struct {
		CID   string
		Count int
	}
	
	var stats []ContainerStat
	for cid, count := range overrunCount {
		stats = append(stats, ContainerStat{CID: cid, Count: count})
	}
	
	// 횟수 내림차순 정렬
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Count > stats[j].Count
	})
	
	fmt.Fprintf(f, "Container ID\t\t\t\t\t\tOverrun Count\n")
	fmt.Fprintf(f, "============================================================\n")
	for _, stat := range stats {
		fmt.Fprintf(f, "%s\t%d\n", stat.CID, stat.Count)
	}
	
	log.Printf("Statistics saved to %s", filename)
}

func main() {
	log.SetFlags(0)

	obj := getenv("BPF_OBJECT", "./hcbs_overrun.bpf.o")
	controllerURL := "http://localhost:30090/overrun" // os.Getenv("CONTROLLER_URL") // 비워두면 POST 생략

	// 1) cgroup2 root & cache
	root, err := findCgroup2Mountpoint()
	if err != nil {
		log.Fatalf("cgroup2 mountpoint: %v", err)
	}
	pc, err := buildPathCache(root)
	if err != nil {
		log.Fatalf("buildPathCache: %v", err)
	}
	log.Printf("cgroup2 root: %s (cached %d entries)", root, len(pc.byIno))

	// 2) eBPF attach
	coll, lnk, rb, err := loadAndAttach(obj)
	if err != nil {
		log.Fatalf("bpf load/attach: %v", err)
	}
	defer rb.Close()
	defer lnk.Close()
	defer coll.Close()

	log.Printf("HCBS overrun agent running. (send only podUID)")

	//cnt := 0
	
	// Container ID별 overrun 횟수 추적
	overrunCount := make(map[string]int)
	totalCnt := 0
	
	// 시그널 핸들러 설정 (Ctrl+C)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	
	// 고루틴으로 시그널 대기
	go func() {
		<-sigChan
		log.Printf("\nReceived interrupt signal. Saving statistics...")
		saveStats(overrunCount, totalCnt)
		os.Exit(0)
	}()
	
	// 3) event loop
	for {
		rec, err := rb.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			log.Printf("Read Err")
			continue
		}

		var ev Event
		if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &ev); err != nil {
			log.Printf("Binary Err")
			continue
		}
		//log.Printf("[%d] overrun detected! : %d", cnt, ev.TS)
		//cnt += 1

		// cgid → path
		path, ok := pc.Lookup(ev.Cgid)
		if !ok {
			// 새 cgroup 등장 가능 → 한번 갱신
			pc.Refresh(root)
			path, ok = pc.Lookup(ev.Cgid)
			if !ok {
				continue
			}
		}
		//log.Printf("Path %s", path)

		// path → podUID
		cid, ok := extractContainerID(path)
		if !ok {
			// 파드 아닌 그룹이면 무시
			continue
		}

		// Container별 overrun 횟수 증가
		overrunCount[cid]++
		totalCnt++
		
		// 로컬 로그 (Container ID별 횟수 출력 + RT task count)
		log.Printf("[Total: %d] Container %s... overrun count: %d (ts=%d, cpu=%d, runtime=%d ns, rt_tasks=%d)",
			totalCnt, cid[:12], overrunCount[cid], ev.TS, ev.CPU, ev.RuntimeNS, ev.NrRunning)

		// 컨트롤러로 uid만 전송
		
		postToController(controllerURL, cid, ev, path)
	}
}
