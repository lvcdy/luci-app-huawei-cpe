// Package cache 是轮询快照的内存缓存层。
//
// 职责边界：
//   - 只持有最近一次采集结果；API 读缓存，绝不触发对 CPE 的请求（架构 §5）；
//   - 快照由 poller 经 PutSnapshot 写入（同一设备串行写，无竞争）；
//   - 提供 TTL 语义：超过 StaleTTL 的快照标记 stale（数据仍返回，由调用方降级展示）。
package cache

import (
	"sync"
	"time"

	"huawei-cpe/internal/poller"
)

// StaleTTL 是快照新鲜度阈值：超过该时长未更新的快照视为陈旧。
// 轮询间隔缺省 60s；5~10s 级刷新窗口内（多个请求并发读）命中同一快照。
const StaleTTL = 10 * time.Second

// Store 是设备快照缓存（并发安全）。
type Store struct {
	mu    sync.RWMutex
	items map[string]item
}

type item struct {
	snap poller.Snapshot
	at   time.Time // 写入（采集）时间
}

// New 创建空缓存。
func New() *Store {
	return &Store{items: map[string]item{}}
}

// PutSnapshot 写入设备最新快照（poller.Sink 接口实现）。
func (s *Store) PutSnapshot(id string, snap poller.Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[id] = item{snap: snap, at: time.Now()}
}

// Get 返回设备快照及新鲜度。
// ok 表示该设备有过采集记录（不代表新鲜）；fresh 表示在 StaleTTL 内。
// 永不触发对 CPE 的请求。
func (s *Store) Get(id string) (snap poller.Snapshot, fresh bool, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, exists := s.items[id]
	if !exists {
		return poller.Snapshot{}, false, false
	}
	return it.snap, time.Since(it.at) <= StaleTTL, true
}

// IDs 返回有缓存记录的设备 ID 列表（无序）。
func (s *Store) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.items))
	for id := range s.items {
		ids = append(ids, id)
	}
	return ids
}

// Remove 删除设备缓存（配置删除设备时调用）。
func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, id)
}
