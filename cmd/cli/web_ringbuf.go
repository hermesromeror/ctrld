package cli

import (
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/Control-D-Inc/ctrld"
)

// WebQueryEvent represents a single DNS resolution event captured for the web UI.
type WebQueryEvent struct {
	ID        uint64 `json:"id"`
	Timestamp string `json:"time"`
	ClientIP  string `json:"client_ip"`
	ClientDev string `json:"client_dev"`
	Qtype     string `json:"type"`
	Domain    string `json:"domain"`
	Rcode     string `json:"rcode"`
	Upstream  string `json:"upstream"`
	LatencyMs int64  `json:"latency_ms"`
	Blocked   bool   `json:"blocked"`
	Status    string `json:"status"` // "RESOLVED", "BLOCKED", "CACHED", "NXDOMAIN", "ERROR"
}

// WebRingBuffer is a fixed-size, thread-safe circular buffer for recent DNS queries,
// supporting real-time subscriber channels for Server-Sent Events (SSE).
type WebRingBuffer struct {
	mu          sync.RWMutex
	size        int
	events      []WebQueryEvent
	head        int
	count       int
	nextID      uint64
	subscribers map[chan WebQueryEvent]struct{}
}

// NewWebRingBuffer creates a new ring buffer with the given capacity.
func NewWebRingBuffer(size int) *WebRingBuffer {
	if size <= 0 {
		size = 500
	}
	return &WebRingBuffer{
		size:        size,
		events:      make([]WebQueryEvent, size),
		subscribers: make(map[chan WebQueryEvent]struct{}),
	}
}

// Add appends a new query event to the ring buffer and notifies active subscribers.
func (rb *WebRingBuffer) Add(ev WebQueryEvent) {
	if rb == nil {
		return
	}
	rb.mu.Lock()
	ev.ID = rb.nextID
	rb.nextID++

	rb.events[rb.head] = ev
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}

	// Broadcast to active SSE subscribers (non-blocking)
	for ch := range rb.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
	rb.mu.Unlock()
}

// Snapshot returns a copy of the most recent events, newest first.
func (rb *WebRingBuffer) Snapshot(limit int) []WebQueryEvent {
	if rb == nil {
		return nil
	}
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	n := rb.count
	if limit > 0 && limit < n {
		n = limit
	}
	res := make([]WebQueryEvent, n)

	// Copy newest first
	idx := (rb.head - 1 + rb.size) % rb.size
	for i := 0; i < n; i++ {
		res[i] = rb.events[idx]
		idx = (idx - 1 + rb.size) % rb.size
	}
	return res
}

// Subscribe registers a channel to receive live query events.
func (rb *WebRingBuffer) Subscribe() chan WebQueryEvent {
	if rb == nil {
		return nil
	}
	ch := make(chan WebQueryEvent, 64)
	rb.mu.Lock()
	rb.subscribers[ch] = struct{}{}
	rb.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (rb *WebRingBuffer) Unsubscribe(ch chan WebQueryEvent) {
	if rb == nil {
		return
	}
	rb.mu.Lock()
	delete(rb.subscribers, ch)
	rb.mu.Unlock()
	close(ch)
}

// recordWebQueryEvent safely converts a completed DNS request into a WebQueryEvent.
func (p *prog) recordWebQueryEvent(q dns.Question, answer *dns.Msg, ci *ctrld.ClientInfo, rtt time.Duration, upstream string) {
	if p.webRingBuf == nil {
		return
	}
	rcode := "NOERROR"
	if answer != nil {
		rcode = dns.RcodeToString[answer.Rcode]
	}
	dev := ""
	ip := ""
	if ci != nil {
		dev = ci.Hostname
		ip = ci.IP
	}
	if dev == "" && ip != "" {
		dev = ip
	}

	blocked := false
	status := "RESOLVED"

	if upstream == "cache" {
		status = "CACHED"
	}

	if answer != nil {
		// Detect NextDNS / Adblock block responses: 0.0.0.0, ::, or RFC 8914 EDE blocked/censored/filtered
		for _, rr := range answer.Answer {
			if a, ok := rr.(*dns.A); ok && (a.A.IsUnspecified() || a.A.IsLoopback()) {
				blocked = true
				status = "BLOCKED"
				break
			}
			if aaaa, ok := rr.(*dns.AAAA); ok && (aaaa.AAAA.IsUnspecified() || aaaa.AAAA.IsLoopback()) {
				blocked = true
				status = "BLOCKED"
				break
			}
		}

		if !blocked {
			for _, opt := range answer.Extra {
				if edns0, ok := opt.(*dns.OPT); ok {
					for _, o := range edns0.Option {
						if ede, ok := o.(*dns.EDNS0_EDE); ok {
							// RFC 8914: 15=Blocked, 16=Censored, 17=Filtered, 18=Prohibited
							if ede.InfoCode >= 15 && ede.InfoCode <= 18 {
								blocked = true
								status = "BLOCKED"
								break
							}
						}
					}
				}
			}
		}

		if !blocked {
			if answer.Rcode == dns.RcodeNameError {
				status = "NXDOMAIN"
			} else if answer.Rcode != dns.RcodeSuccess {
				status = rcode
			}
		}
	} else {
		status = "NO_ANSWER"
	}

	ev := WebQueryEvent{
		Timestamp: time.Now().Format("15:04:05"),
		ClientIP:  ip,
		ClientDev: dev,
		Qtype:     dns.TypeToString[q.Qtype],
		Domain:    canonicalName(q.Name),
		Rcode:     rcode,
		Upstream:  upstream,
		LatencyMs: rtt.Milliseconds(),
		Blocked:   blocked,
		Status:    status,
	}
	p.webRingBuf.Add(ev)
}
