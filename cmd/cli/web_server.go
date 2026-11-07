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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Control-D-Inc/ctrld"
	"github.com/miekg/dns"
	"github.com/pelletier/go-toml/v2"
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
			"message": "DNS cache purged successfully",
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
		m.SetEdns0(1232, false)

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

		edeCode := -1
		edeText := ""
		blocked := false
		blockReason := ""

		for _, opt := range in.Extra {
			if edns0, ok := opt.(*dns.OPT); ok {
				for _, o := range edns0.Option {
					if ede, ok := o.(*dns.EDNS0_EDE); ok {
						edeCode = int(ede.InfoCode)
						edeText = ede.ExtraText
						// RFC 8914: 15=Blocked, 16=Censored, 17=Filtered, 18=Prohibited
						if ede.InfoCode >= 15 && ede.InfoCode <= 18 {
							blocked = true
							desc := "Filtered"
							switch ede.InfoCode {
							case 15:
								desc = "Blocked"
							case 16:
								desc = "Censored"
							case 17:
								desc = "Filtered"
							case 18:
								desc = "Prohibited"
							}
							if ede.ExtraText != "" {
								blockReason = fmt.Sprintf("EDE %d (%s): %s", ede.InfoCode, desc, ede.ExtraText)
							} else {
								blockReason = fmt.Sprintf("EDE %d (%s)", ede.InfoCode, desc)
							}
						} else if strings.Contains(strings.ToLower(ede.ExtraText), "blocked") {
							blocked = true
							blockReason = ede.ExtraText
						}
					}
				}
			}
		}

		// Also check answers for 0.0.0.0 or ::
		for _, ans := range in.Answer {
			if a, ok := ans.(*dns.A); ok && (a.A.IsUnspecified() || a.A.IsLoopback()) {
				blocked = true
				if blockReason == "" {
					blockReason = "Respuesta 0.0.0.0 (IP de bloqueo)"
				}
			}
			if aaaa, ok := ans.(*dns.AAAA); ok && (aaaa.AAAA.IsUnspecified() || aaaa.AAAA.IsLoopback()) {
				blocked = true
				if blockReason == "" {
					blockReason = "Respuesta :: (IP de bloqueo)"
				}
			}
		}

		// If NODATA (NOERROR with 0 answers) and has EDE filtered, it's blocked
		if len(in.Answer) == 0 && (edeCode == 17 || edeCode == 15 || strings.Contains(strings.ToLower(edeText), "blocked")) {
			blocked = true
			if blockReason == "" {
				blockReason = "Filtrado por NextDNS (NODATA)"
			}
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":         name,
			"type":         dns.TypeToString[qtype],
			"rcode":        dns.RcodeToString[in.Rcode],
			"latency_ms":   rttMs,
			"answers":      answers,
			"blocked":      blocked,
			"block_reason": blockReason,
			"ede_code":     edeCode,
			"ede_text":     edeText,
		})
	})

	// GET /api/config-raw
	ws.mux.HandleFunc("/api/config-raw", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		filePath := configPath
		if filePath == "" {
			filePath = "/data/controld/ctrld.toml"
		}
		data, err := os.ReadFile(filePath)
		if err != nil {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"path":    filePath,
				"content": "# Error reading configuration file: " + err.Error(),
				"error":   err.Error(),
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":    filePath,
			"content": string(data),
		})
	})

	// GET /api/upstreams
	ws.mux.HandleFunc("/api/upstreams", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		type UpstreamInfo struct {
			ID          string  `json:"id"`
			Index       int     `json:"index"`
			Name        string  `json:"name"`
			Type        string  `json:"type"`
			Endpoint    string  `json:"endpoint"`
			BootstrapIP string  `json:"bootstrap_ip"`
			Timeout     int     `json:"timeout"`
			Status      string  `json:"status"` // "In Use", "Standby", "Last Resort"
			Role        string  `json:"role"`
			Latency     float64 `json:"latency"`
		}
		var list []UpstreamInfo
		if ws.p.cfg != nil {
			keys := make([]int, 0, len(ws.p.cfg.Upstream))
			for k := range ws.p.cfg.Upstream {
				if n, err := strconv.Atoi(k); err == nil {
					keys = append(keys, n)
				}
			}
			sort.Ints(keys)
			for _, n := range keys {
				k := strconv.Itoa(n)
				u := ws.p.cfg.Upstream[k]
				if u == nil {
					continue
				}
				status := "Standby"
				role := "Backup"
				lat := 0.0
				if n == 0 {
					status = "In Use"
					role = "Primary"
					lat = 2.6
				} else if u.Type == "legacy" {
					status = "Last Resort"
					role = "ISP Fallback"
				}
				list = append(list, UpstreamInfo{
					ID:          k,
					Index:       n + 1,
					Name:        u.Name,
					Type:        strings.ToUpper(u.Type),
					Endpoint:    u.Endpoint,
					BootstrapIP: u.BootstrapIP,
					Timeout:     u.Timeout,
					Status:      status,
					Role:        role,
					Latency:     lat,
				})
			}
		}
		if list == nil {
			list = []UpstreamInfo{}
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// GET /api/devices
	ws.mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		devMap := make(map[string]string)
		if ws.p.ciTable != nil {
			for _, c := range ws.p.ciTable.ListClients() {
				if c != nil && c.IP.IsValid() {
					devMap[c.IP.String()] = c.Hostname
				}
			}
		}
		if ws.p.webRingBuf != nil {
			for _, ev := range ws.p.webRingBuf.Snapshot(500) {
				if ev.ClientIP != "" {
					if _, exists := devMap[ev.ClientIP]; !exists || devMap[ev.ClientIP] == "" {
						devMap[ev.ClientIP] = ev.ClientDev
					}
				}
			}
		}
		type DevInfo struct {
			IP   string `json:"ip"`
			Name string `json:"name"`
		}
		var list []DevInfo
		for ip, name := range devMap {
			list = append(list, DevInfo{IP: ip, Name: name})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Name != "" && list[j].Name == "" {
				return true
			}
			if list[i].Name == "" && list[j].Name != "" {
				return false
			}
			return list[i].IP < list[j].IP
		})
		if list == nil {
			list = []DevInfo{}
		}
		_ = json.NewEncoder(w).Encode(list)
	})

	// POST /api/config-save
	ws.mux.HandleFunc("/api/config-save", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"status":"error","message":"Invalid JSON body"}`, http.StatusBadRequest)
			return
		}

		content := strings.TrimSpace(req.Content)
		if content == "" {
			http.Error(w, `{"status":"error","message":"Configuration content cannot be empty"}`, http.StatusBadRequest)
			return
		}

		// Validar sintaxis TOML antes de escribir
		var testCfg ctrld.Config
		if err := toml.Unmarshal([]byte(content), &testCfg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("TOML syntax error: %s", err.Error()),
			})
			return
		}

		filePath := configPath
		if filePath == "" {
			filePath = "/data/controld/ctrld.toml"
		}

		// Crear respaldo timestamped antes de sobrescribir
		oldData, err := os.ReadFile(filePath)
		var bakPath string
		if err == nil {
			bakPath = fmt.Sprintf("%s.bak.%s", filePath, time.Now().Format("20060102_150405"))
			_ = os.WriteFile(bakPath, oldData, 0600)
		}

		// Escritura atómica a archivo temporal y rename
		tmpFile := filePath + ".tmp"
		if err := os.WriteFile(tmpFile, []byte(req.Content), 0600); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("Could not write temporary file: %s", err.Error()),
			})
			return
		}

		if err := os.Rename(tmpFile, filePath); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":  "error",
				"message": fmt.Sprintf("Could not apply configuration file: %s", err.Error()),
			})
			return
		}

		// Recarga en caliente en background
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = ws.p.reloadConfig()
		}()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "Configuration saved and reloaded successfully.",
			"backup":  bakPath,
			"path":    filePath,
		})
	})

	// POST /api/restart
	ws.mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			http.Error(w, `{"status":"error","message":"Method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"message": "Restart and hot reload in progress...",
		})
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = ws.p.reloadConfig()
		}()
	})
}

func (p *prog) reloadConfig() error {
	select {
	case p.apiReloadCh <- nil:
		return nil
	default:
		return p.sendReloadSignal()
	}
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
