package layer2

import (
	"net"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
)

// Announce is used to "announce" new IPs mapped to the node's MAC address.
type Announce struct {
	logger         log.Logger
	bindInterfaces []string

	sync.RWMutex
	arps   map[int]*arpResponder
	ndps   map[int]*ndpResponder
	ips    map[string]net.IP // map containing IPs we should announce
	leader bool
}

// New returns an initialized Announce.
func New(l log.Logger, ifaces ...string) (*Announce, error) {
	ret := &Announce{
		logger:         l,
		bindInterfaces: ifaces,
		arps:           map[int]*arpResponder{},
		ndps:           map[int]*ndpResponder{},
		ips:            make(map[string]net.IP),
	}
	go ret.interfaceScan()

	return ret, nil
}

func (a *Announce) interfaceScan() {
	for {
		a.updateInterfaces()
		time.Sleep(10 * time.Second)
	}
}

func (a *Announce) updateInterfaces() {
	ifs, err := net.Interfaces()
	if err != nil {
		a.logger.Log("op", "getInterfaces", "error", err, "msg", "couldn't list interfaces")
		return
	}

	a.Lock()
	defer a.Unlock()

	keepARP, keepNDP := map[int]bool{}, map[int]bool{}
	for _, intf := range ifs {
		ifi := intf
		l := log.With(a.logger, "interface", ifi.Name)
		addrs, err := ifi.Addrs()
		if err != nil {
			l.Log("op", "getAddresses", "error", err, "msg", "couldn't get addresses for interface")
			return
		}

		if ifi.Flags&net.FlagUp == 0 {
			continue
		}

		if len(a.bindInterfaces) > 0 && !isBindInterface(ifi.Name, a.bindInterfaces) {
			continue
		}

		if ifi.Flags&net.FlagBroadcast != 0 {
			keepARP[ifi.Index] = true
		}

		for _, a := range addrs {
			ipaddr, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipaddr.IP.To4() != nil || !ipaddr.IP.IsLinkLocalUnicast() {
				continue
			}
			keepNDP[ifi.Index] = true
			break
		}

		if keepARP[ifi.Index] && a.arps[ifi.Index] == nil {
			resp, err := newARPResponder(a.logger, &ifi, a.shouldAnnounce)
			if err != nil {
				l.Log("op", "createARPResponder", "error", err, "msg", "failed to create ARP responder")
				return
			}
			a.arps[ifi.Index] = resp
			l.Log("event", "createARPResponder", "msg", "created ARP responder for interface")
		}
		if keepNDP[ifi.Index] && a.ndps[ifi.Index] == nil {
			resp, err := newNDPResponder(a.logger, &ifi, a.shouldAnnounce)
			if err != nil {
				l.Log("op", "createNDPResponder", "error", err, "msg", "failed to create NDP responder")
				return
			}
			a.ndps[ifi.Index] = resp
			l.Log("event", "createNDPResponder", "msg", "created NDP responder for interface")
		}
	}

	for i, client := range a.arps {
		if !keepARP[i] {
			client.Close()
			delete(a.arps, i)
			a.logger.Log("interface", client.Interface(), "event", "deleteARPResponder", "msg", "deleted ARP responder for interface")
		}
	}
	for i, client := range a.ndps {
		if !keepNDP[i] {
			client.Close()
			delete(a.ndps, i)
			a.logger.Log("interface", client.Interface(), "event", "deleteNDPResponder", "msg", "deleted NDP responder for interface")
		}
	}

	return
}

func (a *Announce) gratuitous(ip net.IP) error {
	a.Lock()
	defer a.Unlock()

	if ip.To4() != nil {
		for _, client := range a.arps {
			if err := client.Gratuitous(ip); err != nil {
				return err
			}
		}
	} else {
		for _, client := range a.ndps {
			if err := client.Gratuitous(ip); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *Announce) shouldAnnounce(ip net.IP) dropReason {
	a.RLock()
	defer a.RUnlock()
	if !a.leader {
		return dropReasonNotLeader
	}
	for _, i := range a.ips {
		if i.Equal(ip) {
			return dropReasonNone
		}
	}
	return dropReasonAnnounceIP
}

// SetBalancer adds ip to the set of announced addresses.
func (a *Announce) SetBalancer(name string, ip net.IP) {
	a.Lock()
	defer a.Unlock()

	// Kubernetes may inform us that we should advertise this address multiple
	// times, so just no-op any subsequent requests.
	if _, ok := a.ips[name]; ok {
		return
	}

	for _, client := range a.ndps {
		if err := client.Watch(ip); err != nil {
			a.logger.Log("op", "watchMulticastGroup", "error", err, "ip", ip, "msg", "failed to watch NDP multicast group for IP, NDP responder will not respond to requests for this address")
		}
	}

	a.ips[name] = ip
}

// DeleteBalancer deletes an address from the set of addresses we should announce.
func (a *Announce) DeleteBalancer(name string) {
	a.Lock()
	defer a.Unlock()

	ip, ok := a.ips[name]
	if !ok {
		return
	}

	for _, client := range a.ndps {
		if err := client.Unwatch(ip); err != nil {
			a.logger.Log("op", "unwatchMulticastGroup", "error", err, "ip", ip, "msg", "failed to unwatch NDP multicast group for IP")
		}
	}

	delete(a.ips, name)
}

// AnnounceName returns true when we have an announcement under name.
func (a *Announce) AnnounceName(name string) bool {
	a.RLock()
	defer a.RUnlock()
	_, ok := a.ips[name]
	return ok
}

// dropReason is the reason why a layer2 protocol packet was not
// responded to.
type dropReason int

// Various reasons why a packet was dropped.
const (
	dropReasonNone dropReason = iota
	dropReasonClosed
	dropReasonError
	dropReasonARPReply
	dropReasonMessageType
	dropReasonNoSourceLL
	dropReasonEthernetDestination
	dropReasonAnnounceIP
	dropReasonNotLeader
)

func isBindInterface(interfaceName string, bindInterfaces []string) bool {
	for _, b := range bindInterfaces {
		if b == interfaceName {
			return true
		}
	}
	return false
}
