package main

import (
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/zeroconf/v2"
)

// Announcer advertises the bridge as _kerf._tcp so Kerf can list it without
// an address being typed. Failure is logged, not fatal: the address still works.
//
// It names its own host ("<machine>-kerf.local") and lists the addresses
// itself rather than leaving both to the library, which takes the OS host
// name as given ("conors-Air.home" becomes "conors-Air.home.local", which
// nothing resolves) and announces every interface, Docker bridges included.
// Addresses are re-checked every 30 s, because a Pi that boots before its
// DHCP lease would otherwise announce nothing for ever.
type Announcer struct {
	mu     sync.Mutex
	server *zeroconf.Server
	ips    string
	stop   chan struct{}
	done   chan struct{}
}

func StartAnnouncer(cfg Config, httpPort int) *Announcer {
	txt := []string{"version=" + Version}
	if _, port, err := net.SplitHostPort(cfg.RawAddr); err == nil && port != "" {
		txt = append(txt, "raw="+port)
	}
	a := &Announcer{stop: make(chan struct{}), done: make(chan struct{})}
	go a.run(cfg.Name, announceHost(), httpPort, txt)
	return a
}

func (a *Announcer) run(name, host string, port int, txt []string) {
	defer close(a.done)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		ifaces, ips := lanAddresses()
		if key := strings.Join(ips, ","); key != a.ips {
			a.shutdown()
			a.ips = key
			if len(ips) == 0 {
				log.Printf("Bonjour: no network address yet")
			} else if s, err := zeroconf.RegisterProxy(name, "_kerf._tcp", "local.", port, host, ips, txt, ifaces); err != nil {
				log.Printf("Bonjour announcement failed: %v", err)
			} else {
				a.mu.Lock()
				a.server = s
				a.mu.Unlock()
				log.Printf("announced as %q on %s.local (%s), port %d", name, host, key, port)
			}
		}
		select {
		case <-a.stop:
			return
		case <-tick.C:
		}
	}
}

func (a *Announcer) shutdown() {
	a.mu.Lock()
	s := a.server
	a.server = nil
	a.mu.Unlock()
	if s == nil {
		return
	}
	// Never let a stuck Bonjour goodbye hold up a restart or a service stop.
	done := make(chan struct{})
	go func() { s.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// Stop sends the goodbye and ends the address watch. Safe on nil.
func (a *Announcer) Stop() {
	if a == nil {
		return
	}
	close(a.stop)
	<-a.done
	a.shutdown()
}

// announceHost is the machine's own name, first label only, made DNS-safe,
// with "-kerf" added so it never collides with the name the OS announces.
func announceHost() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "bridge"
	}
	return s + "-kerf"
}

// virtualIface matches interfaces a phone on the LAN can't reach.
var virtualIface = []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "utun", "awdl", "llw", "tailscale", "wg", "zt"}

// lanAddresses returns the up, multicast, non-loopback interfaces and their
// IPv4 addresses (Kerf connects over IPv4), sorted so changes are detectable.
func lanAddresses() ([]net.Interface, []string) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, nil
	}
	var ifaces []net.Interface
	var ips []string
next:
	for _, ifc := range all {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagMulticast == 0 {
			continue
		}
		for _, p := range virtualIface {
			if strings.HasPrefix(strings.ToLower(ifc.Name), p) {
				continue next
			}
		}
		addrs, _ := ifc.Addrs()
		found := false
		for _, ad := range addrs {
			if n, ok := ad.(*net.IPNet); ok {
				if v4 := n.IP.To4(); v4 != nil && !v4.IsLinkLocalUnicast() {
					ips = append(ips, v4.String())
					found = true
				}
			}
		}
		if found {
			ifaces = append(ifaces, ifc)
		}
	}
	sort.Strings(ips)
	return ifaces, ips
}
