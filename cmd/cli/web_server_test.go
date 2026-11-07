package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Control-D-Inc/ctrld"
	"github.com/miekg/dns"
)

func TestWebRingBuffer(t *testing.T) {
	rb := NewWebRingBuffer(5)

	// Add 7 events to test wrap-around
	for i := 1; i <= 7; i++ {
		rb.Add(WebQueryEvent{
			Domain:    "test" + string(rune('0'+i)) + ".com",
			LatencyMs: int64(i * 2),
		})
	}

	snap := rb.Snapshot(3)
	if len(snap) != 3 {
		t.Fatalf("expected 3 snapshot events, got %d", len(snap))
	}
	// Newest first
	if snap[0].Domain != "test7.com" {
		t.Errorf("expected newest event test7.com, got %s", snap[0].Domain)
	}
	if snap[1].Domain != "test6.com" {
		t.Errorf("expected 2nd event test6.com, got %s", snap[1].Domain)
	}
	if snap[2].Domain != "test5.com" {
		t.Errorf("expected 3rd event test5.com, got %s", snap[2].Domain)
	}

	// Test subscription
	ch := rb.Subscribe()
	rb.Add(WebQueryEvent{Domain: "stream.com"})

	select {
	case ev := <-ch:
		if ev.Domain != "stream.com" {
			t.Errorf("expected stream.com from SSE channel, got %s", ev.Domain)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for subscriber event")
	}

	rb.Unsubscribe(ch)
}

func TestWebServerEndpoints(t *testing.T) {
	p := &prog{
		cfg: &ctrld.Config{
			Service: ctrld.ServiceConfig{
				CacheSize: 1024,
			},
			Web: &ctrld.WebConfig{
				Enable:   true,
				ListenIP: "127.0.0.1",
				Port:     0,
			},
			Upstream: map[string]*ctrld.UpstreamConfig{
				"0": {
					Name:     "Test-NextDNS",
					Type:     "doh3",
					Endpoint: "https://dns.nextdns.io/test",
				},
			},
		},
		webRingBuf: NewWebRingBuffer(100),
	}

	ws, err := p.newWebServer()
	if err != nil {
		t.Fatalf("newWebServer failed: %v", err)
	}

	// 1. Test GET /
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ctrld Manager") {
		t.Errorf("missing expected title in index.html")
	}

	// 2. Test GET /api/status
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/status, got %d", rec.Code)
	}
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("invalid json in /api/status: %v", err)
	}
	if status["version"] == "" {
		t.Errorf("expected non-empty version in status")
	}

	// 3. Test GET /api/queries
	p.webRingBuf.Add(WebQueryEvent{
		Domain:   "apple.com",
		Qtype:    "A",
		ClientIP: "192.168.1.50",
		Status:   "RESOLVED",
	})
	req = httptest.NewRequest(http.MethodGet, "/api/queries?limit=10", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/queries, got %d", rec.Code)
	}
	var queries []WebQueryEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &queries); err != nil {
		t.Fatalf("invalid json in /api/queries: %v", err)
	}
	if len(queries) != 1 || queries[0].Domain != "apple.com" {
		t.Errorf("unexpected queries list: %+v", queries)
	}

	// 4. Test POST /api/flush-cache
	req = httptest.NewRequest(http.MethodPost, "/api/flush-cache", nil)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for /api/flush-cache, got %d", rec.Code)
	}

	// 5. Test SSE /api/stream
	server := httptest.NewServer(ws.mux)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/stream", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	defer sseResp.Body.Close()

	reader := bufio.NewReader(sseResp.Body)
	// Initial comment
	line, _ := reader.ReadString('\n')
	if !strings.HasPrefix(line, ": connected") {
		t.Errorf("expected ': connected', got %q", line)
	}

	// Send an event
	p.webRingBuf.Add(WebQueryEvent{
		Domain: "live-event.org",
		Status: "BLOCKED",
	})

	// Wait for event data
	found := false
	for i := 0; i < 5; i++ {
		line, err = reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			if strings.Contains(line, "live-event.org") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("SSE did not stream live-event.org")
	}
}

func TestWebAuthToken(t *testing.T) {
	p := &prog{
		cfg: &ctrld.Config{
			Web: &ctrld.WebConfig{
				Enable:    true,
				AuthToken: "secret123",
			},
		},
	}
	ws, _ := p.newWebServer()
	handler := ws.authMiddleware(ws.mux)

	// Unauthorized request
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized without token, got %d", rec.Code)
	}

	// Authorized via header
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Auth-Token", "secret123")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK with valid X-Auth-Token, got %d", rec.Code)
	}

	// Authorized via query param
	req = httptest.NewRequest(http.MethodGet, "/api/status?token=secret123", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK with valid ?token=, got %d", rec.Code)
	}
}

func TestRecordWebQueryEvent(t *testing.T) {
	p := &prog{
		webRingBuf: NewWebRingBuffer(10),
	}

	q := dns.Question{
		Name:  "adservice.google.com.",
		Qtype: dns.TypeA,
	}
	ans := new(dns.Msg)
	ans.SetQuestion(q.Name, q.Qtype)
	// Add 0.0.0.0 answer to simulate NextDNS block
	ans.Answer = append(ans.Answer, &dns.A{
		Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
		A:   []byte{0, 0, 0, 0},
	})

	ci := &ctrld.ClientInfo{
		IP:       "192.168.30.181",
		Hostname: "Office-TV",
	}

	p.recordWebQueryEvent(q, ans, ci, 3*time.Millisecond, "upstream.0")

	snap := p.webRingBuf.Snapshot(1)
	if len(snap) != 1 {
		t.Fatalf("expected 1 event, got %d", len(snap))
	}
	ev := snap[0]
	if !ev.Blocked {
		t.Errorf("expected Blocked=true for 0.0.0.0 response")
	}
	if ev.Status != "BLOCKED" {
		t.Errorf("expected Status=BLOCKED, got %s", ev.Status)
	}
	if ev.ClientDev != "Office-TV" {
		t.Errorf("expected ClientDev=Office-TV, got %s", ev.ClientDev)
	}
	if ev.Domain != "adservice.google.com" {
		t.Errorf("expected canonical domain adservice.google.com, got %s", ev.Domain)
	}
}

func TestConfigSaveValidation(t *testing.T) {
	tmpDir := t.TempDir()
	cfgFile := tmpDir + "/ctrld.toml"
	oldPath := configPath
	configPath = cfgFile
	defer func() { configPath = oldPath }()

	_ = os.WriteFile(cfgFile, []byte("[service]\n  log_level = 'info'\n"), 0600)

	p := &prog{
		cfg: &ctrld.Config{
			Web: &ctrld.WebConfig{Enable: true},
		},
	}
	ws, _ := p.newWebServer()

	// 1. Invalid TOML syntax
	body := strings.NewReader(`{"content":"[service\nlog_level = 'unclosed"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/config-save", body)
	rec := httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for invalid TOML, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Error de sintaxis TOML") {
		t.Errorf("missing syntax error description: %s", rec.Body.String())
	}

	// 2. Empty content
	body = strings.NewReader(`{"content":""}`)
	req = httptest.NewRequest(http.MethodPost, "/api/config-save", body)
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for empty content, got %d", rec.Code)
	}

	// 3. Valid TOML save
	validToml := "[service]\n  log_level = 'debug'\n[web]\n  enable = true\n"
	payload, _ := json.Marshal(map[string]string{"content": validToml})
	req = httptest.NewRequest(http.MethodPost, "/api/config-save", strings.NewReader(string(payload)))
	rec = httptest.NewRecorder()
	ws.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 OK for valid save, got %d: %s", rec.Code, rec.Body.String())
	}

	// Check file was updated
	saved, _ := os.ReadFile(cfgFile)
	if string(saved) != validToml {
		t.Errorf("saved content mismatch: %s", string(saved))
	}
}

