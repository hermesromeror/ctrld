package clientinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func Test_firewallaDiscover_refreshDevices_skipsUnavailableRedis(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fd := &firewallaDiscover{redisCliPath: filepath.Join(t.TempDir(), "redis-cli")}
		if err := fd.refreshDevices(); err != nil {
			t.Fatalf("refreshDevices() error = %v", err)
		}
	})

	t.Run("not executable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "redis-cli")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		fd := &firewallaDiscover{redisCliPath: path}
		if err := fd.refreshDevices(); err != nil {
			t.Fatalf("refreshDevices() error = %v", err)
		}
	})
}

func Test_firewallaDiscover_storeDevices(t *testing.T) {
	fd := &firewallaDiscover{}
	jsonInput := `[
		{"mac":"02:75:af:bc:91:2c","ip":"192.168.50.20","name":"Homeassistant","bname":"homeassistant","dhcp":"homeassistant","vendor":"Unknown","ldom":"homeassistant"},
		{"mac":"1c:f2:9a:07:21:e8","ip":"192.168.30.189","name":"Master Bedroom Google Display","bname":"Master Bedroom display","dhcp":"","vendor":"Google, Inc.","ldom":"master.bedroom.google.display"},
		{"mac":"34:e1:d1:80:64:21","ip":"192.168.30.254","name":"","bname":"Hubitat Hubitat","dhcp":"","vendor":"Hubitat Inc.","ldom":"hubitat"},
		{"mac":"32:69:5d:f0:1f:7a","ip":"192.168.20.104","name":"","bname":"Mariana’s iPad","dhcp":"iPad","vendor":"Unknown","ldom":"marianas.ipad"},
		{"mac":"20:6d:31:31:1c:ac","ip":"192.168.20.1","name":"","bname":"FIREWALLA INC","dhcp":"","vendor":"FIREWALLA INC","ldom":""},
		{"mac":"00:00:00:00:00:00","ip":"192.168.20.99","name":"Phantom","bname":"","dhcp":"","vendor":"","ldom":""}
	]`
	if err := fd.storeDevices(strings.NewReader(jsonInput)); err != nil {
		t.Fatalf("storeDevices() error = %v", err)
	}

	if got := fd.LookupHostnameByMac("02:75:af:bc:91:2c"); got != "Homeassistant" {
		t.Errorf("want Homeassistant, got %q", got)
	}
	if got := fd.LookupHostnameByIP("192.168.30.189"); got != "Master Bedroom Google Display" {
		t.Errorf("want Master Bedroom Google Display, got %q", got)
	}
	if got := fd.LookupHostnameByIP("192.168.30.254"); got != "Hubitat Hubitat" {
		t.Errorf("want Hubitat Hubitat, got %q", got)
	}
	if got := fd.LookupHostnameByMac("32:69:5d:f0:1f:7a"); got != "Mariana's iPad" {
		t.Errorf("want Mariana's iPad, got %q", got)
	}
	if got := fd.LookupHostnameByMac("20:6d:31:31:1c:ac"); got != "Firewalla" {
		t.Errorf("want Firewalla, got %q", got)
	}
	if got := fd.LookupHostnameByIP("192.168.20.99"); got != "" {
		t.Errorf("zero mac should be ignored, got %q", got)
	}
	if got := fd.lookupIPByHostname("master.bedroom.google.display", false); got != "192.168.30.189" {
		t.Errorf("lookupIPByHostname ldom want 192.168.30.189, got %q", got)
	}
	if got := fd.lookupIPByHostname("homeassistant.lan", false); got != "192.168.50.20" {
		t.Errorf("lookupIPByHostname .lan want 192.168.50.20, got %q", got)
	}
}
