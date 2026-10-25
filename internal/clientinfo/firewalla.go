package clientinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Control-D-Inc/ctrld/internal/router"
	"github.com/Control-D-Inc/ctrld/internal/router/firewalla"
)

const defaultFirewallaRedisCliPath = "/usr/bin/redis-cli"

const firewallaRedisLuaScript = `
local macs = redis.call("ZRANGE", "host:active:mac", 0, -1)
local dhcp_macs = redis.call("SMEMBERS", "host:dhcpconf:mac")
local seen = {}
local result = {}
for _, mac in ipairs(macs) do seen[string.lower(mac)] = mac end
for _, mac in ipairs(dhcp_macs) do seen[string.lower(mac)] = mac end
for _, raw_mac in pairs(seen) do
    local key = "host:mac:" .. raw_mac
    if redis.call("TYPE", key).ok == "hash" then
        local name = redis.call("HGET", key, "name") or ""
        local bname = redis.call("HGET", key, "bname") or ""
        local dhcp = redis.call("HGET", key, "dhcpName") or ""
        local vendor = redis.call("HGET", key, "macVendor") or ""
        local ip = redis.call("HGET", key, "ipv4Addr") or redis.call("HGET", key, "ipv4") or ""
        if ip ~= "" then
            local ip_mac = redis.call("HGET", "host:ip4:" .. ip, "mac") or ""
            if ip_mac ~= "" and string.lower(ip_mac) ~= string.lower(raw_mac) then
                ip = ""
            end
        end
        local ldom = redis.call("HGET", key, "userLocalDomain") or ""
        if ldom == "" then ldom = redis.call("HGET", key, "localDomain") or "" end
        table.insert(result, {mac=string.lower(raw_mac), ip=ip, name=name, bname=bname, dhcp=dhcp, vendor=vendor, ldom=ldom})
    end
end
if #result == 0 then
    local cursor = "0"
    repeat
        local scan_result = redis.call("SCAN", cursor, "MATCH", "host:mac:*", "COUNT", 1000)
        cursor = scan_result[1]
        for _, key in ipairs(scan_result[2]) do
            if redis.call("TYPE", key).ok == "hash" then
                local raw_mac = string.gsub(key, "host:mac:", "")
                local mac = string.lower(raw_mac)
                if not seen[mac] then
                    seen[mac] = raw_mac
                    local name = redis.call("HGET", key, "name") or ""
                    local bname = redis.call("HGET", key, "bname") or ""
                    local dhcp = redis.call("HGET", key, "dhcpName") or ""
                    local vendor = redis.call("HGET", key, "macVendor") or ""
                    local ip = redis.call("HGET", key, "ipv4Addr") or redis.call("HGET", key, "ipv4") or ""
                    if ip ~= "" then
                        local ip_mac = redis.call("HGET", "host:ip4:" .. ip, "mac") or ""
                        if ip_mac ~= "" and string.lower(ip_mac) ~= mac then
                            ip = ""
                        end
                    end
                    local ldom = redis.call("HGET", key, "userLocalDomain") or ""
                    if ldom == "" then ldom = redis.call("HGET", key, "localDomain") or "" end
                    table.insert(result, {mac=mac, ip=ip, name=name, bname=bname, dhcp=dhcp, vendor=vendor, ldom=ldom})
                end
            end
        end
    until cursor == "0"
end
return cjson.encode(result)
`

type firewallaDeviceEntry struct {
	MAC    string `json:"mac"`
	IP     string `json:"ip"`
	Name   string `json:"name"`
	BName  string `json:"bname"`
	DHCP   string `json:"dhcp"`
	Vendor string `json:"vendor"`
	LDom   string `json:"ldom"`
}

// firewallaDiscover provides client discovery functionality on Firewalla routers via local Redis.
type firewallaDiscover struct {
	mac2name     sync.Map // mac => hostname
	ip2name      sync.Map // ip  => hostname
	mac2ip       sync.Map // mac => ip
	ip2mac       sync.Map // ip  => mac
	domain2ip    sync.Map // lowercase domain/slug => ip
	redisCliPath string
	lastRefresh  time.Time
	refreshMu    sync.Mutex
}

func (f *firewallaDiscover) refresh() error {
	if router.Name() != firewalla.Name {
		return nil
	}
	f.refreshMu.Lock()
	defer f.refreshMu.Unlock()
	if !f.lastRefresh.IsZero() && time.Since(f.lastRefresh) < 5*time.Minute {
		return nil
	}
	err := f.refreshDevices()
	if err == nil {
		f.lastRefresh = time.Now()
	}
	return err
}

func (f *firewallaDiscover) LookupHostnameByIP(ip string) string {
	if ip == "" {
		return ""
	}
	val, ok := f.ip2name.Load(ip)
	if !ok {
		return ""
	}
	return val.(string)
}

func (f *firewallaDiscover) LookupHostnameByMac(mac string) string {
	if mac == "" {
		return ""
	}
	val, ok := f.mac2name.Load(strings.ToLower(mac))
	if !ok {
		return ""
	}
	return val.(string)
}

func (f *firewallaDiscover) LookupIP(mac string) string {
	if mac == "" {
		return ""
	}
	val, ok := f.mac2ip.Load(strings.ToLower(mac))
	if !ok {
		return ""
	}
	return val.(string)
}

func (f *firewallaDiscover) LookupMac(ip string) string {
	if ip == "" {
		return ""
	}
	val, ok := f.ip2mac.Load(ip)
	if !ok {
		return ""
	}
	return val.(string)
}

func (f *firewallaDiscover) List() []string {
	if f == nil {
		return nil
	}
	var ips []string
	f.ip2mac.Range(func(key, value any) bool {
		ips = append(ips, key.(string))
		return true
	})
	return ips
}

func (f *firewallaDiscover) lookupIPByHostname(name string, v6 bool) string {
	if f == nil || v6 || name == "" {
		return ""
	}
	key := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if val, ok := f.domain2ip.Load(key); ok {
		if addr, err := netip.ParseAddr(val.(string)); err == nil && addr.Is4() {
			return addr.String()
		}
	}
	// Also try stripping .lan or .local suffix.
	for _, suffix := range []string{".lan", ".local", ".home.arpa"} {
		if strings.HasSuffix(key, suffix) {
			short := strings.TrimSuffix(key, suffix)
			if val, ok := f.domain2ip.Load(short); ok {
				if addr, err := netip.ParseAddr(val.(string)); err == nil && addr.Is4() {
					return addr.String()
				}
			}
		}
	}
	return ""
}

func (f *firewallaDiscover) refreshDevices() error {
	cliPath := f.redisExecutable()
	fi, err := os.Stat(cliPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat redis-cli executable: %w", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0111 == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, cliPath, "--raw", "EVAL", firewallaRedisLuaScript, "0")
	b, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("redis-cli eval: %w", err)
	}
	return f.storeDevices(bytes.NewReader(b))
}

func (f *firewallaDiscover) redisExecutable() string {
	if f.redisCliPath != "" {
		return f.redisCliPath
	}
	return defaultFirewallaRedisCliPath
}

func (f *firewallaDiscover) storeDevices(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) {
		return nil
	}
	var entries []firewallaDeviceEntry
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return err
	}
	for _, dev := range entries {
		mac := strings.ToLower(strings.TrimSpace(dev.MAC))
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}
		if _, err := net.ParseMAC(mac); err != nil {
			continue
		}

		chosen := selectFirewallaHostname(dev)
		ip := normalizeIP(strings.TrimSpace(dev.IP))
		if ip != "" && net.ParseIP(ip) == nil {
			ip = ""
		}

		if chosen != "" {
			f.mac2name.Store(mac, chosen)
			if ip != "" {
				f.ip2name.Store(ip, chosen)
				registerDomainSlug(f, chosen, ip)
			}
		}
		if ip != "" {
			f.mac2ip.Store(mac, ip)
			f.ip2mac.Store(ip, mac)
			if ldom := strings.ToLower(strings.TrimSpace(dev.LDom)); ldom != "" {
				registerDomainSlug(f, ldom, ip)
			}
		}
	}
	return nil
}

func selectFirewallaHostname(dev firewallaDeviceEntry) string {
	name := strings.TrimSpace(dev.Name)
	bname := strings.TrimSpace(dev.BName)
	dhcp := strings.TrimSpace(dev.DHCP)
	vendor := strings.TrimSpace(dev.Vendor)

	var chosen string
	switch {
	case name != "":
		chosen = name
	case bname != "" && !strings.EqualFold(bname, vendor):
		chosen = bname
	case dhcp != "" && dhcp != "*":
		chosen = dhcp
	case bname != "":
		if strings.EqualFold(bname, "FIREWALLA INC") {
			chosen = "Firewalla"
		} else {
			chosen = bname
		}
	}
	return cleanFirewallaHostname(chosen)
}

func cleanFirewallaHostname(s string) string {
	if s == "" {
		return ""
	}
	// Replace smart quotes/apostrophes with ASCII equivalents for HTTP header safety.
	r := strings.NewReplacer(
		"’", "'",
		"‘", "'",
		"“", "\"",
		"”", "\"",
	)
	s = r.Replace(s)
	s = strings.TrimSpace(s)
	for _, suffix := range []string{".local.", ".local", ".lan.", ".lan"} {
		if strings.HasSuffix(strings.ToLower(s), suffix) {
			s = s[:len(s)-len(suffix)]
			break
		}
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "."))
	// Ensure printable ASCII (0x20-0x7E) for HTTP headers.
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c <= 0x7e {
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}

// isReservedOrDisallowedDomain blocks dangerous names like wpad, isatap, router, localhost
// to prevent internal DNS poisoning/spoofing.
func isReservedOrDisallowedDomain(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, suffix := range []string{".lan", ".local", ".home.arpa", ".domain"} {
		lower = strings.TrimSuffix(lower, suffix)
	}
	// Extract the first label or single label
	firstLabel := lower
	if idx := strings.IndexByte(lower, '.'); idx != -1 {
		firstLabel = lower[:idx]
	}
	switch firstLabel {
	case "wpad", "isatap", "localhost", "broadcasthost", "router", "firewalla", "gateway", "dns", "ns", "proxy",
		"com", "net", "org", "arpa", "edu", "gov", "mil", "int", "io", "local", "lan", "home":
		return true
	default:
		switch lower {
		case "wpad", "isatap", "localhost", "broadcasthost", "router", "firewalla", "gateway", "dns", "ns", "proxy",
			"com", "net", "org", "arpa", "edu", "gov", "mil", "int", "io", "local", "lan", "home":
			return true
		default:
			return false
		}
	}
}

func registerDomainSlug(f *firewallaDiscover, rawName, ip string) {
	clean := strings.ToLower(strings.TrimSpace(rawName))
	for _, suffix := range []string{".lan", ".local", ".home.arpa"} {
		clean = strings.TrimSuffix(clean, suffix)
	}
	if clean == "" || isReservedOrDisallowedDomain(clean) {
		return
	}
	f.domain2ip.Store(clean, ip)
	slug := strings.ReplaceAll(clean, " ", "-")
	if slug != clean {
		f.domain2ip.Store(slug, ip)
	}
}

func (f *firewallaDiscover) String() string {
	return "firewalla"
}
