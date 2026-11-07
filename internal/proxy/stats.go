package proxy

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const maxUnknown = 32

// Stats — счётчики демона; читаются из LuCI через файл.
type Stats struct {
	Active, Total, WS, Fallback, WSErrors, BytesUp, BytesDown   atomic.Int64
	Worker, WorkerErrors                                        atomic.Int64
	FirstReply404, FirstReply429, FirstReply444, AdmissionDrops atomic.Int64

	started time.Time
	mu      sync.Mutex
	unknown map[string]struct{}
	order   []string
}

func NewStats() *Stats {
	return &Stats{started: time.Now(), unknown: map[string]struct{}{}}
}

// NoteUnknown запоминает адрес, для которого не нашлось DC. Возвращает true
// только в первый раз — чтобы в журнал уходила одна строка на адрес.
func (s *Stats) NoteUnknown(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.unknown[ip]; ok {
		return false
	}
	if len(s.order) >= maxUnknown {
		delete(s.unknown, s.order[0])
		s.order = s.order[1:]
	}
	s.unknown[ip] = struct{}{}
	s.order = append(s.order, ip)
	return true
}

// Snapshot — сериализуемый снимок.
type Snapshot struct {
	Started        int64    `json:"started"`
	Updated        int64    `json:"updated"`
	Active         int64    `json:"active"`
	Total          int64    `json:"total"`
	WS             int64    `json:"ws"`
	Worker         int64    `json:"worker"`
	WorkerErrors   int64    `json:"worker_errors"`
	Fallback       int64    `json:"fallback"`
	WSErrors       int64    `json:"ws_errors"`
	BytesUp        int64    `json:"bytes_up"`
	BytesDown      int64    `json:"bytes_down"`
	UnknownDC      []string `json:"unknown_dc"`
	FirstReply404  int64    `json:"first_reply_404"`
	FirstReply429  int64    `json:"first_reply_429"`
	FirstReply444  int64    `json:"first_reply_444"`
	AdmissionDrops int64    `json:"admission_drops"`
}

func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	unk := append([]string{}, s.order...)
	s.mu.Unlock()
	return Snapshot{
		Started: s.started.Unix(), Updated: time.Now().Unix(),
		Active: s.Active.Load(), Total: s.Total.Load(), WS: s.WS.Load(),
		Worker: s.Worker.Load(), WorkerErrors: s.WorkerErrors.Load(),
		Fallback: s.Fallback.Load(), WSErrors: s.WSErrors.Load(),
		BytesUp: s.BytesUp.Load(), BytesDown: s.BytesDown.Load(),
		UnknownDC:     unk,
		FirstReply404: s.FirstReply404.Load(), FirstReply429: s.FirstReply429.Load(),
		FirstReply444: s.FirstReply444.Load(), AdmissionDrops: s.AdmissionDrops.Load(),
	}
}

// WriteFile пишет снимок атомарно: читатель никогда не увидит полфайла.
func (s *Stats) WriteFile(path string) error {
	data, err := json.Marshal(s.Snapshot())
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
