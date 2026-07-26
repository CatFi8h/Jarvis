// Package subs is the per-chat, per-parser subscriber registry.
//
// Telegram bots can't enumerate the chats they belong to — a chat only becomes
// known when it messages the bot. So we remember every chat that talks to us and
// broadcast to them. Because one bot process now drives several parsers, each
// chat holds independent per-parser preferences (subscribed / alerts muted /
// heartbeat off), persisted to JSON so they survive restarts.
package subs

import (
	"encoding/json"
	"os"
	"sync"
)

// Prefs are one chat's preferences for one parser.
type Prefs struct {
	Subscribed   bool `json:"subscribed"`
	Muted        bool `json:"muted"`         // alerts off (/<parser>_stop)
	HeartbeatOff bool `json:"heartbeat_off"` // periodic pings off
}

type subscriber struct {
	ChatID  string            `json:"chat_id"`
	Parsers map[string]*Prefs `json:"parsers"` // key: parser name ("nike","gr")
}

// Registry is a mutex-guarded map of chat id -> subscriber, persisted to disk.
type Registry struct {
	mu   sync.Mutex
	path string
	subs map[string]*subscriber
}

// New loads the registry from path (a missing/invalid file starts empty).
func New(path string) *Registry {
	r := &Registry{path: path, subs: map[string]*subscriber{}}
	if data, err := os.ReadFile(path); err == nil {
		var list []*subscriber
		if json.Unmarshal(data, &list) == nil {
			for _, s := range list {
				if s.Parsers == nil {
					s.Parsers = map[string]*Prefs{}
				}
				r.subs[s.ChatID] = s
			}
		}
	}
	return r
}

// save writes the registry to disk. Caller holds r.mu.
func (r *Registry) save() {
	list := make([]*subscriber, 0, len(r.subs))
	for _, s := range r.subs {
		list = append(list, s)
	}
	if data, err := json.MarshalIndent(list, "", "  "); err == nil {
		os.WriteFile(r.path, data, 0o644)
	}
}

// prefsLocked returns the prefs for (chatID, parser), creating the subscriber and
// prefs entry if absent. Caller holds r.mu.
func (r *Registry) prefsLocked(chatID, parser string) *Prefs {
	s, ok := r.subs[chatID]
	if !ok {
		s = &subscriber{ChatID: chatID, Parsers: map[string]*Prefs{}}
		r.subs[chatID] = s
	}
	if s.Parsers == nil {
		s.Parsers = map[string]*Prefs{}
	}
	p, ok := s.Parsers[parser]
	if !ok {
		p = &Prefs{}
		s.Parsers[parser] = p
	}
	return p
}

func (r *Registry) mutate(chatID, parser string, fn func(*Prefs)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(r.prefsLocked(chatID, parser))
	r.save()
}

// Subscribe opts a chat into a parser and unmutes its alerts.
func (r *Registry) Subscribe(chatID, parser string) {
	r.mutate(chatID, parser, func(p *Prefs) { p.Subscribed = true; p.Muted = false })
}

// Mute toggles alert muting for a chat's parser subscription.
func (r *Registry) Mute(chatID, parser string, muted bool) {
	r.mutate(chatID, parser, func(p *Prefs) { p.Muted = muted })
}

// SetHeartbeat toggles heartbeat pings for a chat's parser subscription.
func (r *Registry) SetHeartbeat(chatID, parser string, off bool) {
	r.mutate(chatID, parser, func(p *Prefs) { p.HeartbeatOff = off })
}

// Get returns a copy of a chat's prefs for a parser (zero value if none).
func (r *Registry) Get(chatID, parser string) Prefs {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.subs[chatID]; ok {
		if p, ok := s.Parsers[parser]; ok {
			return *p
		}
	}
	return Prefs{}
}

// Targets returns the chat ids that should receive a message for a parser:
// subscribed and (for alerts) not muted, or (for heartbeats) not silenced.
func (r *Registry) Targets(parser string, wantAlert bool) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for id, s := range r.subs {
		p, ok := s.Parsers[parser]
		if !ok || !p.Subscribed {
			continue
		}
		if wantAlert && !p.Muted {
			out = append(out, id)
		}
		if !wantAlert && !p.HeartbeatOff {
			out = append(out, id)
		}
	}
	return out
}

// Count returns how many chats are subscribed to a parser.
func (r *Registry) Count(parser string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.subs {
		if p, ok := s.Parsers[parser]; ok && p.Subscribed {
			n++
		}
	}
	return n
}
