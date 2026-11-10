/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package plc4x

// Finding devices on a network (beehive R-134), with the discovery of the
// PLC4X drivers that have one: they broadcast on the local networks and
// report each device that answers, with a connection string for it.

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
)

// Discovered is a device a driver found.
type Discovered struct {
	Protocol  string
	Transport string
	// Name is what the device calls itself, if it says.
	Name string
	// URL is a connection string for it, to start a Connection from.
	URL string
	// Address is where it answered (the transport's host and port).
	Address string
	// Attributes are what else it reported (vendor, product, serial ...),
	// as text.
	Attributes map[string]string
}

// DiscoverOptions narrow a discovery.
type DiscoverOptions struct {
	// Protocols to discover with; empty for every driver that can.
	Protocols []string
	// Wait is how long to listen for answers; 0 is 5 seconds, at most 60.
	Wait time.Duration
	// LocalAddress, when set, is the local interface address to search
	// from; empty searches from every interface.
	LocalAddress string
}

var (
	discoverOnce sync.Once
	discoverable map[string]bool
)

// discovers reports whether a protocol's driver finds devices.
func discovers(protocol string) bool {
	discoverOnce.Do(func() {
		discoverable = map[string]bool{}
		for code, register := range protocols() {
			d := register(plc4go.NewPlcDriverManager())
			discoverable[code] = d.SupportsDiscovery()
			closeSoon(d)
		}
	})
	return discoverable[protocol]
}

// closeSoon closes a driver, waiting at most a second: some drivers' Close
// waits for their discovery's listeners (ads and bacnet-ip on Windows), and
// a search must not hang on them. A Close still running is left to finish.
func closeSoon(d any) {
	c, ok := d.(io.Closer)
	if !ok {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Close()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// DiscoverProtocols returns the protocols whose drivers find devices, sorted.
func DiscoverProtocols() []string {
	var out []string
	for _, code := range KnownProtocols() {
		if discovers(code) {
			out = append(out, code)
		}
	}
	return out
}

// DiscoverResult is what a search found, and what kept part of it from
// running (a driver that could not listen on an interface, say).
type DiscoverResult struct {
	Found    []Discovered
	Problems []string
}

// Discover searches the local networks for devices and returns those that
// answered within the wait, once each, sorted by protocol and address. It
// opens no connection to them and changes nothing. A driver that cannot
// search every interface at once (one it cannot listen on) is run again on
// each interface on its own.
func Discover(ctx context.Context, o DiscoverOptions) (DiscoverResult, error) {
	codes := o.Protocols
	if len(codes) == 0 {
		codes = DiscoverProtocols()
	}
	for _, code := range codes {
		if !discovers(code) {
			return DiscoverResult{}, fmt.Errorf("plc4x: %s devices cannot be found on a network: add them by address", code)
		}
	}
	wait := o.Wait
	if wait <= 0 {
		wait = 5 * time.Second
	}
	wait = min(wait, time.Minute)
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	var mu sync.Mutex
	found := map[string]Discovered{}
	var problems []string
	add := func(d Discovered) {
		mu.Lock()
		defer mu.Unlock()
		if _, seen := found[d.URL]; !seen {
			found[d.URL] = d
		}
	}
	problem := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	var wg sync.WaitGroup
	for _, code := range codes {
		wg.Go(func() {
			err := discoverOn(ctx, code, o.LocalAddress, add)
			if err == nil || o.LocalAddress != "" {
				if err != nil {
					problem("%s from %s: %v", code, o.LocalAddress, err)
				}
				return
			}
			addrs := localAddresses()
			if len(addrs) == 0 {
				problem("%s: %v", code, err)
				return
			}
			var each sync.WaitGroup
			ok := false
			var okMu sync.Mutex
			for _, a := range addrs {
				each.Go(func() {
					if err := discoverOn(ctx, code, a, add); err != nil {
						problem("%s from %s: %v", code, a, err)
						return
					}
					okMu.Lock()
					ok = true
					okMu.Unlock()
				})
			}
			each.Wait()
			if !ok {
				problem("%s found nothing: no interface could be searched", code)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	out := make([]Discovered, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Discovered) int {
		if c := strings.Compare(a.Protocol, b.Protocol); c != 0 {
			return c
		}
		return strings.Compare(a.Address, b.Address)
	})
	slices.Sort(problems)
	return DiscoverResult{Found: out, Problems: problems}, nil
}

// discoverOn runs one driver's discovery, from one local address or from
// every interface (local ""), until ctx ends.
func discoverOn(ctx context.Context, code, local string, add func(Discovered)) error {
	m, err := NewDriverManagerFor(code)
	if err != nil {
		return err
	}
	defer func() {
		if d, err := m.GetDriver(code); err == nil {
			closeSoon(d)
		}
	}()
	opts := []plc4go.WithDiscoveryOption{plc4go.WithDiscoveryOptionProtocol(code)}
	if local != "" {
		opts = append(opts, plc4go.WithDiscoveryOptionLocalAddress(local))
	}
	if err := m.Discover(ctx, func(item apiModel.PlcDiscoveryItem) { add(discovered(item)) }, opts...); err != nil && ctx.Err() == nil {
		return err
	}
	<-ctx.Done() // drivers report answers as they arrive
	return nil
}

// localAddresses are the IPv4 addresses of the interfaces that are up,
// not loopback and not link-local.
func localAddresses() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// discovered reads a driver's discovery item.
func discovered(item apiModel.PlcDiscoveryItem) Discovered {
	u := item.GetTransportUrl()
	d := Discovered{
		Protocol: item.GetProtocolCode(), Transport: item.GetTransportCode(),
		Name: item.GetName(), URL: item.GetConnectionUrl(), Address: u.Host,
	}
	if d.URL == "" {
		d.URL = fmt.Sprintf("%s:%s://%s", d.Protocol, d.Transport, u.Host)
	}
	if attrs := item.GetAttributes(); len(attrs) > 0 {
		d.Attributes = map[string]string{}
		for k, v := range attrs {
			if v != nil {
				d.Attributes[k] = v.String()
			}
		}
	}
	return d
}
