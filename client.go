package zeroconf

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"runtime"
	"strings"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// IPType specifies the IP traffic the client listens for.
// This does not guarantee that only mDNS entries of this sepcific
// type passes. E.g. typical mDNS packets distributed via IPv4, often contain
// both DNS A and AAAA entries.
type IPType uint8

// Options for IPType.
const (
	IPv4        IPType = 0x01
	IPv6        IPType = 0x02
	IPv4AndIPv6        = IPv4 | IPv6 // default option
)

var initialQueryInterval = 4 * time.Second

// Client structure encapsulates both IPv4/IPv6 UDP connections.
type client struct {
	ipv4conn *ipv4.PacketConn
	ipv6conn *ipv6.PacketConn
	ifaces   []net.Interface
}

type clientOpts struct {
	listenOn IPType
	ifaces   []net.Interface
}

// ClientOption fills the option struct to configure intefaces, etc.
type ClientOption func(*clientOpts)

// SelectIPTraffic selects the type of IP packets (IPv4, IPv6, or both) this
// instance listens for.
// This does not guarantee that only mDNS entries of this sepcific
// type passes. E.g. typical mDNS packets distributed via IPv4, may contain
// both DNS A and AAAA entries.
func SelectIPTraffic(t IPType) ClientOption {
	return func(o *clientOpts) {
		o.listenOn = t
	}
}

// SelectIfaces selects the interfaces to query for mDNS records
func SelectIfaces(ifaces []net.Interface) ClientOption {
	return func(o *clientOpts) {
		o.ifaces = ifaces
	}
}

// Browse for all services of a given type in a given domain.
// Received entries are sent on the entries channel.
// It blocks until the context is canceled (or an error occurs).
func Browse(ctx context.Context, service, domain string, entries chan<- *ServiceEntry, opts ...ClientOption) error {
	cl, err := newClient(applyOpts(opts...))
	if err != nil {
		return err
	}
	params := defaultParams(service)
	if domain != "" {
		params.Domain = domain
	}
	params.Entries = entries
	params.isBrowsing = true
	return cl.run(ctx, params)
}

// Lookup a specific service by its name and type in a given domain.
// Received entries are sent on the entries channel.
// It blocks until the context is canceled (or an error occurs).
func Lookup(ctx context.Context, instance, service, domain string, entries chan<- *ServiceEntry, opts ...ClientOption) error {
	cl, err := newClient(applyOpts(opts...))
	if err != nil {
		return err
	}
	params := defaultParams(service)
	params.Instance = instance
	if domain != "" {
		params.Domain = domain
	}
	params.Entries = entries
	return cl.run(ctx, params)
}

func applyOpts(options ...ClientOption) clientOpts {
	// Apply default configuration and load supplied options.
	var conf = clientOpts{
		listenOn: IPv4AndIPv6,
	}
	for _, o := range options {
		if o != nil {
			o(&conf)
		}
	}
	return conf
}

func (c *client) run(ctx context.Context, params *lookupParams) error {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.mainloop(ctx, params)
	}()

	// If previous probe was ok, it should be fine now. In case of an error later on,
	// the entries' queue is closed.
	// Periodic query causes lots of (most probably) unneccessary queries as services will announce themselves and send updates when required
	/*
		err := c.periodicQuery(ctx, params)
		cancel()
		<-done
		return err
	*/

	// Do a single query
	err := c.query(params)

	if err != nil {
		cancel()
		return err
	}

	<-ctx.Done()
	cancel()
	return nil
}

// defaultParams returns a default set of QueryParams.
func defaultParams(service string) *lookupParams {
	return newLookupParams("", service, "local", false, make(chan *ServiceEntry))
}

// Client structure constructor
func newClient(opts clientOpts) (*client, error) {
	ifaces := opts.ifaces
	if len(ifaces) == 0 {
		ifaces = listMulticastInterfaces()
	}
	// IPv4 interfaces
	var ipv4conn *ipv4.PacketConn
	if (opts.listenOn & IPv4) > 0 {
		var err error
		ipv4conn, err = joinUdp4Multicast(ifaces)
		if err != nil {
			return nil, err
		}
	}
	// IPv6 interfaces
	var ipv6conn *ipv6.PacketConn
	if (opts.listenOn & IPv6) > 0 {
		var err error
		ipv6conn, err = joinUdp6Multicast(ifaces)
		if err != nil {
			return nil, err
		}
	}

	return &client{
		ipv4conn: ipv4conn,
		ipv6conn: ipv6conn,
		ifaces:   ifaces,
	}, nil
}

var cleanupFreq = 10 * time.Second

// Start listeners and waits for the shutdown signal from exit channel
func (c *client) mainloop(ctx context.Context, params *lookupParams) {
	// start listening for responses
	msgCh := make(chan *dns.Msg, 32)
	if c.ipv4conn != nil {
		go c.recv(ctx, c.ipv4conn, msgCh)
	}
	if c.ipv6conn != nil {
		go c.recv(ctx, c.ipv6conn, msgCh)
	}

	// Iterate through channels from listeners goroutines
	var entries map[string]*ServiceEntry
	sentEntries := make(map[string]*ServiceEntry)

	ticker := time.NewTicker(cleanupFreq)
	defer ticker.Stop()
	for {
		var now time.Time
		select {
		case <-ctx.Done():
			// Context expired. Notify subscriber that we are done here.
			params.done()
			c.shutdown()
			return
		case t := <-ticker.C:
			for k, e := range sentEntries {
				if t.After(e.Expiry) {
					delete(sentEntries, k)
				}
			}
			continue
		case msg := <-msgCh:
			now = time.Now()
			entries = make(map[string]*ServiceEntry)
			sections := append(msg.Answer, msg.Ns...)
			sections = append(sections, msg.Extra...)

			for _, answer := range sections {
				header := answer.Header()

				switch rr := answer.(type) {
				case *dns.PTR:
					if params.ServiceName() != rr.Hdr.Name {
						continue
					}
					if params.ServiceInstanceName() != "" && params.ServiceInstanceName() != rr.Ptr {
						continue
					}
					if _, found := entries[rr.Ptr]; !found {
						entries[rr.Ptr] = newServiceEntry(
							trimDot(strings.Replace(rr.Ptr, rr.Hdr.Name, "", -1)),
							params.Service,
							params.Domain)
					}
					entries[rr.Ptr].Expiry = now.Add(time.Duration(rr.Hdr.Ttl) * time.Second)
					// Cache Flush takes most significant bit of class. If that's set class gets 32768 added
					entries[rr.Ptr].CacheFlush = header.Class > 32768
				case *dns.SRV:
					if params.ServiceInstanceName() != "" && params.ServiceInstanceName() != rr.Hdr.Name {
						continue
					} else if !strings.HasSuffix(rr.Hdr.Name, params.ServiceName()) {
						continue
					}
					if _, found := entries[rr.Hdr.Name]; !found {
						entries[rr.Hdr.Name] = newServiceEntry(
							trimDot(strings.Replace(rr.Hdr.Name, params.ServiceName(), "", 1)),
							params.Service,
							params.Domain)
					}
					entries[rr.Hdr.Name].HostName = rr.Target
					entries[rr.Hdr.Name].Port = int(rr.Port)
					entries[rr.Hdr.Name].Expiry = now.Add(time.Duration(rr.Hdr.Ttl) * time.Second)
					// Cache Flush takes most significant bit of class. If that's set class gets 32768 added
					entries[rr.Hdr.Name].CacheFlush = header.Class > 32768
				case *dns.TXT:
					if params.ServiceInstanceName() != "" && params.ServiceInstanceName() != rr.Hdr.Name {
						continue
					} else if !strings.HasSuffix(rr.Hdr.Name, params.ServiceName()) {
						continue
					}
					if _, found := entries[rr.Hdr.Name]; !found {
						entries[rr.Hdr.Name] = newServiceEntry(
							trimDot(strings.Replace(rr.Hdr.Name, params.ServiceName(), "", 1)),
							params.Service,
							params.Domain)
					}
					entries[rr.Hdr.Name].Text = rr.Txt
					entries[rr.Hdr.Name].Expiry = now.Add(time.Duration(rr.Hdr.Ttl) * time.Second)
					// Cache Flush takes most significant bit of class. If that's set class gets 32768 added
					entries[rr.Hdr.Name].CacheFlush = header.Class > 32768
				}
			}
			// Associate IPs in a second round as other fields should be filled by now.
			for _, answer := range sections {
				switch rr := answer.(type) {
				case *dns.A:
					for k, e := range entries {
						if e.HostName == rr.Hdr.Name {
							entries[k].AddrIPv4 = append(entries[k].AddrIPv4, rr.A)
						}
					}
				case *dns.AAAA:
					for k, e := range entries {
						if e.HostName == rr.Hdr.Name {
							entries[k].AddrIPv6 = append(entries[k].AddrIPv6, rr.AAAA)
						}
					}
				}
			}
		}

		if len(entries) > 0 {
			for k, e := range entries {
				if !e.Expiry.After(now) {
					delete(entries, k)
					delete(sentEntries, k)
					continue
				}

				if entry, found := sentEntries[k]; found {
					// Only sent entry update if it expires in less than 1 minute
					if !e.Expiry.After(entry.Expiry.Add(-1*time.Minute)) && !e.CacheFlush {
						continue
					}
				}

				// If this is an DNS-SD query do not throw PTR away.
				// It is expected to have only PTR for enumeration
				/*
					if params.ServiceRecord.ServiceTypeName() != params.ServiceRecord.ServiceName() {
						// Require at least one resolved IP address for ServiceEntry
						// TODO: wait some more time as chances are high both will arrive.
						if len(e.AddrIPv4) == 0 && len(e.AddrIPv6) == 0 {
							continue
						}
					}
				*/
				// Submit entry to subscriber and cache it.
				// This is also a point to possibly stop probing actively for a
				// service entry.
				params.Entries <- e
				sentEntries[k] = e
				if !params.isBrowsing {
					params.disableProbing()
				}
			}
		}
	}
}

// Shutdown client will close currently open connections and channel implicitly.
func (c *client) shutdown() {
	if c.ipv4conn != nil {
		c.ipv4conn.Close()
	}
	if c.ipv6conn != nil {
		c.ipv6conn.Close()
	}
}

// Data receiving routine reads from connection, unpacks packets into dns.Msg
// structures and sends them to a given msgCh channel
func (c *client) recv(ctx context.Context, l interface{}, msgCh chan *dns.Msg) {
	var readFrom func([]byte) (n int, src net.Addr, err error)

	switch pConn := l.(type) {
	case *ipv6.PacketConn:
		readFrom = func(b []byte) (n int, src net.Addr, err error) {
			n, _, src, err = pConn.ReadFrom(b)
			return
		}
	case *ipv4.PacketConn:
		readFrom = func(b []byte) (n int, src net.Addr, err error) {
			n, _, src, err = pConn.ReadFrom(b)
			return
		}

	default:
		return
	}

	buf := make([]byte, 65536)
	var fatalErr error
	for {
		// Handles the following cases:
		// - ReadFrom aborts with error due to closed UDP connection -> causes ctx cancel
		// - ReadFrom aborts otherwise.
		// TODO: the context check can be removed. Verify!
		if ctx.Err() != nil || fatalErr != nil {
			return
		}

		n, _, err := readFrom(buf)
		if err != nil {
			fatalErr = err
			continue
		}
		msg := new(dns.Msg)
		if err := msg.Unpack(buf[:n]); err != nil {
			// log.Printf("[WARN] mdns: Failed to unpack packet: %v", err)
			continue
		}
		select {
		case msgCh <- msg:
			// Submit decoded DNS message and continue.
			//log.Printf("New msg sent to channel: %v\n", msg)
		case <-ctx.Done():
			// Abort.
			return
		}
	}
}

// periodicQuery sens multiple probes until a valid response is received by
// the main processing loop or some timeout/cancel fires.
// TODO: move error reporting to shutdown function as periodicQuery is called from
// go routine context.
func (c *client) periodicQuery(ctx context.Context, params *lookupParams) error {
	// Do the first query immediately.
	if err := c.query(params); err != nil {
		return err
	}

	const maxInterval = 60 * time.Second
	interval := initialQueryInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			// Wait for next iteration.
		case <-params.stopProbing:
			// Chan is closed (or happened in the past).
			// Done here. Received a matching mDNS entry.
			return nil
		case <-ctx.Done():
			if params.isBrowsing {
				return nil
			}
			return ctx.Err()
		}

		if err := c.query(params); err != nil {
			return err
		}
		// Exponential increase of the interval with jitter:
		// the new interval will be between 1.5x and 2.5x the old interval, capped at maxInterval.
		if interval != maxInterval {
			interval += time.Duration(rand.Int63n(interval.Nanoseconds())) + interval/2
			if interval > maxInterval {
				interval = maxInterval
			}
		}
		timer.Reset(interval)
	}
}

// Performs the actual query by service name (browse) or service instance name (lookup),
// start response listeners goroutines and loops over the entries channel.
func (c *client) query(params *lookupParams) error {
	var serviceName, serviceInstanceName string
	serviceName = fmt.Sprintf("%s.%s.", trimDot(params.Service), trimDot(params.Domain))

	// send the query
	m := new(dns.Msg)
	if params.Instance != "" { // service instance name lookup
		serviceInstanceName = fmt.Sprintf("%s.%s", params.Instance, serviceName)
		m.Question = []dns.Question{
			{Name: serviceInstanceName, Qtype: dns.TypeSRV, Qclass: dns.ClassINET},
			{Name: serviceInstanceName, Qtype: dns.TypeTXT, Qclass: dns.ClassINET},
			{Name: serviceInstanceName, Qtype: dns.TypeANY, Qclass: dns.ClassINET},
		}
	} else if len(params.Subtypes) > 0 { // service subtype browse
		m.SetQuestion(params.Subtypes[0], dns.TypePTR)
	} else { // service name browse
		m.SetQuestion(serviceName, dns.TypePTR)
	}
	m.RecursionDesired = false
	return c.sendQuery(m)
}

// Pack the dns.Msg and write to available connections (multicast)
func (c *client) sendQuery(msg *dns.Msg) error {
	buf, err := msg.Pack()
	if err != nil {
		return err
	}
	if c.ipv4conn != nil {
		// See https://pkg.go.dev/golang.org/x/net/ipv4#pkg-note-BUG
		// As of Golang 1.18.4
		// On Windows, the ControlMessage for ReadFrom and WriteTo methods of PacketConn is not implemented.
		var wcm ipv4.ControlMessage
		for ifi := range c.ifaces {
			switch runtime.GOOS {
			case "darwin", "ios", "linux":
				wcm.IfIndex = c.ifaces[ifi].Index
			case "windows":
				if c.ifaces[ifi].Name == "Teredo Tunneling Pseudo-Interface" {
					//log.Println("Skipping Teredo interface on windows")
				} else {
					if err := c.ipv4conn.SetMulticastInterface(&c.ifaces[ifi]); err != nil {
						log.Printf("[WARN] mdns: Failed to set multicast interface %s: %v", c.ifaces[ifi].Name, err)
					}
				}
			default:
				if err := c.ipv4conn.SetMulticastInterface(&c.ifaces[ifi]); err != nil {
					log.Printf("[WARN] mdns: Failed to set multicast interface %s: %v", c.ifaces[ifi].Name, err)
				}
			}
			c.ipv4conn.WriteTo(buf, &wcm, ipv4Addr)
		}
	}
	if c.ipv6conn != nil {
		// See https://pkg.go.dev/golang.org/x/net/ipv6#pkg-note-BUG
		// As of Golang 1.18.4
		// On Windows, the ControlMessage for ReadFrom and WriteTo methods of PacketConn is not implemented.
		var wcm ipv6.ControlMessage
		for ifi := range c.ifaces {
			switch runtime.GOOS {
			case "darwin", "ios", "linux":
				wcm.IfIndex = c.ifaces[ifi].Index
			case "windows":
				if c.ifaces[ifi].Name == "Teredo Tunneling Pseudo-Interface" {
					//log.Println("Skipping Teredo interface on windows")
				} else {
					if err := c.ipv4conn.SetMulticastInterface(&c.ifaces[ifi]); err != nil {
						log.Printf("[WARN] mdns: Failed to set multicast interface %s: %v", c.ifaces[ifi].Name, err)
					}
				}
			default:
				if err := c.ipv6conn.SetMulticastInterface(&c.ifaces[ifi]); err != nil {
					log.Printf("[WARN] mdns: Failed to set multicast interface %s: %v", c.ifaces[ifi].Name, err)
				}
			}
			c.ipv6conn.WriteTo(buf, &wcm, ipv6Addr)
		}
	}
	return nil
}
