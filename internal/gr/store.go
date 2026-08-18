package gr

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"time"
)

// Search is one chat's watch for a specific train (route + date + time and/or
// number). A chat can run any number of searches concurrently.
type Search struct {
	ID       int    `json:"id"`
	ChatID   string `json:"chat_id"`
	FromCode string `json:"from_code"`
	ToCode   string `json:"to_code"`
	Date     string `json:"date"`                   // YYYY-MM-DD
	DepTime  string `json:"dep_time,omitempty"`     // HH:MM, normalized
	TrainNum string `json:"train_number,omitempty"` // e.g. "803"

	CreatedAt time.Time `json:"created_at"`
	LastAlert time.Time `json:"last_alert,omitempty"`
	Warned    bool      `json:"warned_not_found,omitempty"`

	// runtime state, not persisted
	LastResult string `json:"-"`
	Available  bool   `json:"-"`
}

// routeKey groups searches that can share one API request.
func (s *Search) routeKey() routeKey {
	return routeKey{From: s.FromCode, To: s.ToCode, Date: s.Date}
}

type routeKey struct {
	From, To, Date string
}

// store is the mutex-guarded set of active searches, persisted to JSON.
type store struct {
	mu     sync.Mutex
	path   string
	nextID int
	list   []*Search
}

type storeFile struct {
	NextID   int       `json:"next_id"`
	Searches []*Search `json:"searches"`
}

func newStore(path string) *store {
	st := &store{path: path, nextID: 1}
	if data, err := os.ReadFile(path); err == nil {
		var f storeFile
		if json.Unmarshal(data, &f) == nil {
			st.list = f.Searches
			st.nextID = f.NextID
			for _, s := range st.list {
				if s.ID >= st.nextID {
					st.nextID = s.ID + 1
				}
			}
			if st.nextID < 1 {
				st.nextID = 1
			}
		}
	}
	return st
}

// save writes the store to disk. Caller holds st.mu.
func (st *store) save() {
	data, err := json.MarshalIndent(storeFile{NextID: st.nextID, Searches: st.list}, "", "  ")
	if err == nil {
		os.WriteFile(st.path, data, 0o644)
	}
}

// Add registers a new search and returns it (a copy).
func (st *store) Add(s Search) Search {
	st.mu.Lock()
	defer st.mu.Unlock()
	s.ID = st.nextID
	st.nextID++
	s.CreatedAt = time.Now()
	cp := s
	st.list = append(st.list, &cp)
	st.save()
	return s
}

// Remove deletes a chat's search by id; returns the removed search if found.
func (st *store) Remove(chatID string, id int) (Search, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i, s := range st.list {
		if s.ID == id && s.ChatID == chatID {
			out := *s
			st.list = append(st.list[:i], st.list[i+1:]...)
			st.save()
			return out, true
		}
	}
	return Search{}, false
}

// RemoveAll deletes every search of a chat and returns how many were removed.
func (st *store) RemoveAll(chatID string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	kept := st.list[:0]
	n := 0
	for _, s := range st.list {
		if s.ChatID == chatID {
			n++
			continue
		}
		kept = append(kept, s)
	}
	st.list = kept
	if n > 0 {
		st.save()
	}
	return n
}

// ExpireBefore removes searches dated strictly before date (YYYY-MM-DD sorts
// lexicographically) and returns them so their chats can be notified.
func (st *store) ExpireBefore(date string) []Search {
	st.mu.Lock()
	defer st.mu.Unlock()
	var expired []Search
	kept := st.list[:0]
	for _, s := range st.list {
		if s.Date < date {
			expired = append(expired, *s)
			continue
		}
		kept = append(kept, s)
	}
	st.list = kept
	if len(expired) > 0 {
		st.save()
	}
	return expired
}

// Snapshot returns copies of all searches, ordered by id.
func (st *store) Snapshot() []Search {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Search, 0, len(st.list))
	for _, s := range st.list {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ForChat returns copies of one chat's searches, ordered by id.
func (st *store) ForChat(chatID string) []Search {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []Search
	for _, s := range st.list {
		if s.ChatID == chatID {
			out = append(out, *s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// CountForChat returns how many searches a chat is running.
func (st *store) CountForChat(chatID string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, s := range st.list {
		if s.ChatID == chatID {
			n++
		}
	}
	return n
}

// Update mutates one search under the lock; persist=true also saves to disk
// (use it for fields that must survive a restart, e.g. LastAlert).
func (st *store) Update(id int, persist bool, fn func(*Search)) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, s := range st.list {
		if s.ID == id {
			fn(s)
			if persist {
				st.save()
			}
			return
		}
	}
}
