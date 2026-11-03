package clientinfo

import (
	"bufio"
	"os"
	"strings"
)

const procNetArpFile = "/proc/net/arp"

func (a *arpDiscover) scan() {
	f, err := os.Open(procNetArpFile)
	if err != nil {
		return
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	s.Scan() // skip header
	for s.Scan() {
		line := s.Text()
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		ip := fields[0]
		flags := fields[2]
		mac := fields[3]
		if flags == "0x0" || mac == "00:00:00:00:00:00" || mac == "*" || mac == "" {
			continue
		}
		a.mac.Store(ip, mac)
		a.ip.Store(mac, ip)
	}
}
