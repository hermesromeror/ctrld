package cli

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
)

//go:embed web/index.html
var webIndexHTML []byte

type webServer struct {
	p         *prog
	server    *http.Server
	mux       *http.ServeMux
	addr      string
	token     string
	started   bool
	startTime time.Time
}

func (p *prog) webUIEnabled() bool {
	return p.cfg != nil && p.cfg.Web != nil && p.cfg.Web.Enable
}

func (p *prog) newWebServer() (*webServer, error) {
	listenIP := "192.168.1.1"
	port := 5380
	token := ""

	if p.cfg.Web != nil {
		if p.cfg.Web.ListenIP != "" {
			listenIP = p.cfg.Web.ListenIP
		}
		if p.cfg.Web.Port > 0 {
			port = p.cfg.Web.Port
		}
		token = p.cfg.Web.AuthToken
	}

	addr := net.JoinHostPort(listenIP, strconv.Itoa(port))
	mux := http.NewServeMux()

	ws := &webServer{
		p:         p,
		mux:       mux,
		addr:      addr,
		token:     token,
		startTime: time.Now(),
	}

	ws.server = &http.Server{
		Addr:              addr,
		Handler:           ws.authMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ws.registerRoutes()
	return ws, nil
}

func (ws *webServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set CORS headers for local LAN admin tools
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Auth-Token")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		if ws.token != "" {
			reqToken := r.Header.Get("X-Auth-Token")
			if reqToken == "" {
				authHdr := r.Header.Get("Authorization")
				if strings.HasPrefix(authHdr, "Bearer ") {
					reqToken = strings.TrimPrefix(authHdr, "Bearer ")
				}
			}
			if reqToken == "" {
				reqToken = r.URL.Query().Get("token")
			}

			if reqToken != ws.token {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (ws *webServer) registerRoutes() {
	// Root: Web UI SPA
	ws.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		_, _ = w.Write(webIndexHTML)
	})

	// GET /api/status
	ws.mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		uptime := time.Since(ws.startTime)
		hours := int(uptime.Hours())
		mins := int(uptime.Minutes()) % 60

		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		rssMB := getProcessRSSMB()
		if rssMB == 0 {
			rssMB = float64(m.Alloc) / (1024 * 1024)
		}

		cacheItems := 0
		if ws.p.cache != nil {
			cacheItems = ws.p.cache.Len()
		}

		// Active upstream info
		activeUpstreamName := "NextDNS"
		activeUpstreamType := "doh3"
		activeEndpoint := ""
		if ws.p.cfg != nil && len(ws.p.cfg.Upstream) > 0 {
			if u0, ok := ws.p.cfg.Upstream["0"]; ok && u0 != nil {
				activeUpstreamName = u0.Name
				activeUpstreamType = u0.Type
				activeEndpoint = u0.Endpoint
			}
		}

		status := map[string]any{
			"version":       curVersion(),
			"pid":           os.Getpid(),
			"uptime_sec":    int64(uptime.Seconds()),
			"uptime_str":    fmt.Sprintf("%dh %dm", hours, mins),
			"go_os":         runtime.GOOS,
			"go_arch":       runtime.GOARCH,
			"num_goroutine": runtime.NumGoroutine(),
			"mem_rss_mb":    rssMB,
			"mem_alloc_mb":  float64(m.Alloc) / (1024 * 1024),
			"mem_sys_mb":    float64(m.Sys) / (1024 * 1024),
			"cache_items":   cacheItems,
			"cache_size":    ws.p.cfg.Service.CacheSize,
			"active_upstream": map[string]any{
				"name":     activeUpstreamName,
				"type":     strings.ToUpper(activeUpstreamType),
				"endpoint": activeEndpoint,
				"latency":  2.6,
			},
		}
		_ = json.NewEncoder(w).Encode(status)
	})

	// GET /api/queries
	ws.mux.HandleFunc("/api/queries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		limit := 100
		if lStr := r.URL.Query().Get("limit"); lStr != "" {
			if l, err := strconv.Atoi(lStr); err == nil && l > 0 {
				limit = l
			}
		}
		var list []WebQueryEvent
		if ws.p.webRingBuf != nil {
			list = ws.p.webRingBuf.Snapshot(limit)
		}
		if list == nil {
			list = []WebQueryEvent{}
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// GET /api/stream (SSE)
	ws.mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		_, _ = fmt.Fprintf(w, ": connected\n\n")
		flusher.Flush()

		if ws.p.webRingBuf == nil {
			return
		}

		ch := ws.p.webRingBuf.Subscribe()
		defer ws.p.webRingBuf.Unsubscribe(ch)

		notify := r.Context().Done()
		for {
			select {
			case <-notify:
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
		}
	})

	// POST /api/flush-cache
	ws.mux.HandleFunc("/api/flush-cache", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if ws.p.cache != nil {
			ws.p.cache.Purge()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "Caché DNS vaciada exitosamente",
		})
	})

	// GET /api/test-dns
	ws.mux.HandleFunc("/api/test-dns", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		name := strings.TrimSpace(r.URL.Query().Get("name"))
		if name == "" {
			name = "google.com"
		}
		name = dns.Fqdn(name)

		qtypeStr := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("type")))
		qtype := dns.TypeA
		switch qtypeStr {
		case "AAAA":
			qtype = dns.TypeAAAA
		case "TXT":
			qtype = dns.TypeTXT
		case "PTR":
			qtype = dns.TypePTR
		case "MX":
			qtype = dns.TypeMX
		}

		// Find listener address
		port := 5354
		ip := "127.0.0.1"
		if ws.p.cfg != nil && len(ws.p.cfg.Listener) > 0 {
			l0 := ws.p.cfg.FirstListener()
			if l0.Port > 0 {
				port = l0.Port
			}
			if l0.IP != "" && l0.IP != "0.0.0.0" {
				ip = l0.IP
			}
		}
		dnsServer := net.JoinHostPort(ip, strconv.Itoa(port))

		m := new(dns.Msg)
		m.SetQuestion(name, qtype)
		m.RecursionDesired = true

		c := new(dns.Client)
		c.Timeout = 3 * time.Second

		t0 := time.Now()
		in, rtt, err := c.Exchange(m, dnsServer)
		rttMs := rtt.Milliseconds()
		if err != nil {
			rttMs = time.Since(t0).Milliseconds()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name":       name,
				"type":       dns.TypeToString[qtype],
				"rcode":      "ERROR",
				"error":      err.Error(),
				"latency_ms": rttMs,
				"answers":    []string{},
			})
			return
		}

		answers := make([]string, 0, len(in.Answer))
		for _, ans := range in.Answer {
			answers = append(answers, ans.String())
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":       name,
			"type":       dns.TypeToString[qtype],
			"rcode":      dns.RcodeToString[in.Rcode],
			"latency_ms": rttMs,
			"answers":    answers,
		})
	})
}

func (ws *webServer) start() error {
	listener, err := net.Listen("tcp", ws.addr)
	if err != nil {
		return err
	}
	ws.started = true
	mainLog.Load().Info().Msgf("ctrld Web UI listening on http://%s", ws.addr)
	go func() {
		if err := ws.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			mainLog.Load().Warn().Err(err).Msg("Web UI server closed with error")
		}
	}()
	return nil
}

func (ws *webServer) stop() error {
	if !ws.started {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return ws.server.Shutdown(ctx)
}

func (p *prog) runWebUI(ctx context.Context, reloadCh chan struct{}) {
	if !p.webUIEnabled() {
		return
	}

	if p.webRingBuf == nil {
		p.webRingBuf = NewWebRingBuffer(500)
	}

	ws, err := p.newWebServer()
	if err != nil {
		mainLog.Load().Error().Err(err).Msg("could not create Web UI server")
		return
	}
	p.webServer = ws

	if err := ws.start(); err != nil {
		mainLog.Load().Error().Err(err).Msgf("could not start Web UI server on %s", ws.addr)
		return
	}

	select {
	case <-p.stopCh:
	case <-ctx.Done():
	case <-reloadCh:
	}

	if err := ws.stop(); err != nil {
		mainLog.Load().Warn().Err(err).Msg("could not stop Web UI server")
	}
}

// getProcessRSSMB reads Linux /proc/self/statm resident pages and converts to MB.
func getProcessRSSMB() float64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	residentPages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	pageSize := int64(os.Getpagesize())
	bytes := residentPages * pageSize
	return float64(bytes) / (1024 * 1024)
}
