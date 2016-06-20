// Copyright (c) 2016 The btcsuite developers Use of this source code is
// governed by an ISC license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/addrmgr"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/bloom"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

const (
	// defaultServices describes the default services that are supported by
	// the server.
	defaultServices = wire.SFNodeBloom

	// defaultMaxOutbound is the default number of max outbound peers.
	defaultMaxOutbound = 8

	// connectionRetryInterval is the base amount of time to wait in between
	// retries when connecting to persistent peers.  It is adjusted by the
	// number of retries such that there is a retry backoff.
	connectionRetryInterval = time.Second * 5
)

var (
	// userAgentName is the user agent name and is used to help identify
	// ourselves to other bitcoin peers.
	userAgentName = "btcd"

	// userAgentVersion is the user agent version and is used to help
	// identify ourselves to other bitcoin peers.
	userAgentVersion = fmt.Sprintf("%d.%d.%d", appMajor, appMinor, appPatch)
)

// serverPeer extends the peer to maintain state shared by the server and
// the blockmanager.
type serverPeer struct {
	*peer.Peer

	connReq    *connmgr.ConnReq
	server     *server
	persistent bool
	filter     *bloom.Filter
	quit       chan struct{}
}

// newServerPeer returns a new serverPeer instance. The peer needs to be set by
// the caller.
func newServerPeer(s *server, isPersistent bool) *serverPeer {
	return &serverPeer{
		server:     s,
		persistent: isPersistent,
		filter:     nil,
		quit:       make(chan struct{}),
	}
}

// server provides a bitcoin server for handling communications to and from
// bitcoin peers.
type server struct {
	started  int32
	shutdown int32

	chainParams *chaincfg.Params
	connManager *connmgr.ConnManager
	addrManager *addrmgr.AddrManager
	newPeers    chan *serverPeer
	donePeers   chan *serverPeer
	banPeers    chan *serverPeer
	wakeup      chan struct{}
	query       chan interface{}
	wg          sync.WaitGroup
	quit        chan struct{}
	timeSource  blockchain.MedianTimeSource
	services    wire.ServiceFlag
	outpoints   []*wire.OutPoint
}

// newOutboundPeer initializes a new outbound peer and setups the message
// listeners.
func (s *server) newOutboundPeer(addr string, persistent bool) *serverPeer {
	sp := newServerPeer(s, persistent)
	p, err := peer.NewOutboundPeer(newPeerConfig(sp), addr)
	if err != nil {
		log.Errorf("Cannot create outbound peer %s: %v", addr, err)
		return nil
	}
	sp.Peer = p
	go s.peerDoneHandler(sp)
	return sp
}

// peerDoneHandler handles peer disconnects by notifiying the server that it's
// done.
func (s *server) peerDoneHandler(sp *serverPeer) {
	sp.WaitForDisconnect()
	s.donePeers <- sp
	close(sp.quit)
}

// peerState maintains state of inbound, persistent, outbound peers as well
// as banned peers and outbound groups.
type peerState struct {
	inboundPeers     map[int32]*serverPeer
	outboundPeers    map[int32]*serverPeer
	persistentPeers  map[int32]*serverPeer
	banned           map[string]time.Time
	outboundGroups   map[string]int
	maxOutboundPeers int
}

// Count returns the count of all known peers.
func (ps *peerState) Count() int {
	return len(ps.inboundPeers) + len(ps.outboundPeers) +
		len(ps.persistentPeers)
}

// forAllOutboundPeers is a helper function that runs closure on all outbound
// peers known to peerState.
func (ps *peerState) forAllOutboundPeers(closure func(sp *serverPeer)) {
	for _, e := range ps.outboundPeers {
		closure(e)
	}
	for _, e := range ps.persistentPeers {
		closure(e)
	}
}

// forAllPeers is a helper function that runs closure on all peers known to
// peerState.
func (ps *peerState) forAllPeers(closure func(sp *serverPeer)) {
	for _, e := range ps.inboundPeers {
		closure(e)
	}
	ps.forAllOutboundPeers(closure)
}

// handleAddPeerMsg deals with adding new peers.  It is invoked from the
// peerHandler goroutine.
func (s *server) handleAddPeerMsg(state *peerState, sp *serverPeer) bool {
	if sp == nil {
		return false
	}

	// Ignore new peers if we're shutting down.
	if atomic.LoadInt32(&s.shutdown) != 0 {
		log.Infof("New peer %s ignored - server is shutting down", sp)
		sp.Disconnect()
		return false
	}

	// Disconnect banned peers.
	host, _, err := net.SplitHostPort(sp.Addr())
	if err != nil {
		log.Debugf("can't split hostport %v", err)
		sp.Disconnect()
		return false
	}
	if banEnd, ok := state.banned[host]; ok {
		if time.Now().Before(banEnd) {
			log.Debugf("Peer %s is banned for another %v - disconnecting",
				host, banEnd.Sub(time.Now()))
			sp.Disconnect()
			return false
		}

		log.Infof("Peer %s is no longer banned", host)
		delete(state.banned, host)
	}

	// Add the new peer and start it.
	log.Debugf("New peer %s", sp)
	if sp.Inbound() {
		state.inboundPeers[sp.ID()] = sp
	} else {
		state.outboundGroups[addrmgr.GroupKey(sp.NA())]++
		if sp.persistent {
			state.persistentPeers[sp.ID()] = sp
		} else {
			state.outboundPeers[sp.ID()] = sp
		}
	}

	return true
}

// handleDonePeerMsg deals with peers that have signalled they are done.  It is
// invoked from the peerHandler goroutine.
func (s *server) handleDonePeerMsg(state *peerState, sp *serverPeer) {
	var list map[int32]*serverPeer
	if sp.persistent {
		list = state.persistentPeers
	} else if sp.Inbound() {
		list = state.inboundPeers
	} else {
		list = state.outboundPeers
	}
	if _, ok := list[sp.ID()]; ok {
		if !sp.Inbound() && sp.VersionKnown() {
			state.outboundGroups[addrmgr.GroupKey(sp.NA())]--
		}
		if sp.persistent && sp.connReq != nil {
			s.connManager.Disconnect(sp.connReq.ID())
		}
		delete(list, sp.ID())
		log.Debugf("Removed peer %s", sp)
		return
	}

	if sp.connReq != nil {
		s.connManager.Remove(sp.connReq.ID())
	}

	// Update the address' last seen time if the peer has acknowledged
	// our version and has sent us its version as well.
	if sp.VerAckReceived() && sp.VersionKnown() && sp.NA() != nil {
		s.addrManager.Connected(sp.NA())
	}

	// If we get here it means that either we didn't know about the peer
	// or we purposefully deleted it.
}

// handleBanPeerMsg deals with banning peers.  It is invoked from the
// peerHandler goroutine.
func (s *server) handleBanPeerMsg(state *peerState, sp *serverPeer) {
	host, _, err := net.SplitHostPort(sp.Addr())
	if err != nil {
		log.Debugf("can't split ban peer %s %v", sp.Addr(), err)
		return
	}
	direction := directionString(sp.Inbound())
	log.Infof("Banned peer %s (%s) for %v", host, direction,
		cfg.BanDuration)
	state.banned[host] = time.Now().Add(cfg.BanDuration)
}

// peerHandler is used to handle peer operations such as adding and removing
// peers to and from the server, banning peers, and broadcasting messages to
// peers.  It must be run in a goroutine.
func (s *server) peerHandler() {
	s.addrManager.Start()

	log.Tracef("Starting peer handler")

	state := &peerState{
		inboundPeers:     make(map[int32]*serverPeer),
		persistentPeers:  make(map[int32]*serverPeer),
		outboundPeers:    make(map[int32]*serverPeer),
		banned:           make(map[string]time.Time),
		maxOutboundPeers: defaultMaxOutbound,
		outboundGroups:   make(map[string]int),
	}

	if !cfg.DisableDNSSeed {
		// Add peers discovered through DNS to the address manager.
		connmgr.SeedFromDNS(s.chainParams, btcwalletLookup, func(addrs []*wire.NetAddress) {
			// Bitcoind uses a lookup of the dns seeder here. This
			// is rather strange since the values looked up by the
			// DNS seed lookups will vary quite a lot.
			// to replicate this behaviour we put all addresses as
			// having come from the first one.
			s.addrManager.AddAddresses(addrs, addrs[0])
		})
	}

	// Start up persistent peers.
	permanentPeers := cfg.ConnectPeers
	if len(permanentPeers) == 0 {
		permanentPeers = cfg.AddPeers
	}
	for _, addr := range permanentPeers {
		go s.connManager.Connect(&connmgr.ConnReq{Addr: addr, Permanent: true})
	}
	// TODO: Sync Connects
	time.Sleep(time.Millisecond)
	s.connManager.Start()

	// if nothing else happens, wake us up soon.
	time.AfterFunc(10*time.Second, func() { s.wakeup <- struct{}{} })

out:
	for {
		select {
		// New peers connected to the server.
		case p := <-s.newPeers:
			s.handleAddPeerMsg(state, p)

		// Disconnected peers.
		case p := <-s.donePeers:
			s.handleDonePeerMsg(state, p)

		// Peer to ban.
		case p := <-s.banPeers:
			s.handleBanPeerMsg(state, p)

		// Used by timers below to wake us back up.
		case <-s.wakeup:
			// this page left intentionally blank

		case <-s.quit:
			// Disconnect all peers on server shutdown.
			state.forAllPeers(func(sp *serverPeer) {
				log.Tracef("Shutdown peer %s", sp)
				sp.Disconnect()
			})
			break out
		}

		// Don't try to connect to more peers when running on the
		// simulation test network.  The simulation network is only
		// intended to connect to specified peers and actively avoid
		// advertising and connecting to discovered peers.
		if cfg.SimNet {
			continue
		}

		// TODO: Connect to more peers - this is now managed by connmgr.
		// Make sure connmgr covers all the work that was earlier here.
	}

	s.connManager.Stop()
	s.addrManager.Stop()

	// Drain channels before exiting so nothing is left waiting around
	// to send.
cleanup:
	for {
		select {
		case <-s.newPeers:
		case <-s.donePeers:
		case <-s.wakeup:
		case <-s.query:
		default:
			break cleanup
		}
	}
	s.wg.Done()
	log.Tracef("Peer handler done")
}

// AddPeer adds a new peer that has already been connected to the server.
func (s *server) AddPeer(sp *serverPeer) {
	s.newPeers <- sp
}

// BanPeer bans a peer that has already been connected to the server by ip.
func (s *server) BanPeer(sp *serverPeer) {
	s.banPeers <- sp
}

func (s *server) AddOutPoint(outpoint *wire.OutPoint) {
	s.outpoints = append(s.outpoints, outpoint)
}

// Start begins accepting connections from peers.
func (s *server) Start() {
	// Already started?
	if atomic.AddInt32(&s.started, 1) != 1 {
		return
	}

	log.Trace("Starting server")

	// Start the peer handler which in turn starts the address and block
	// managers.
	s.wg.Add(1)
	go s.peerHandler()
}

// Stop gracefully shuts down the server by stopping and disconnecting all
// peers and the main listener.
func (s *server) Stop() error {
	// Make sure this only happens once.
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		log.Infof("Server is already in the process of shutting down")
		return nil
	}

	log.Warnf("Server shutting down")

	// Signal the remaining goroutines to quit.
	close(s.quit)
	return nil
}

// WaitForShutdown blocks until the main listener and peer handlers are stopped.
func (s *server) WaitForShutdown() {
	s.wg.Wait()
}

func (sp *serverPeer) OnVersion(p *peer.Peer, msg *wire.MsgVersion) {
	// Add the remote peer time as a sample for creating an offset against
	// the local clock to keep the network time in sync.
	sp.server.timeSource.AddTimeSample(p.Addr(), msg.Timestamp)

	// Update the address manager and request known addresses from the
	// remote peer for outbound connections.  This is skipped when running
	// on the simulation test network since it is only intended to connect
	// to specified peers and actively avoids advertising and connecting to
	// discovered peers.
	if !cfg.SimNet {
		addrManager := sp.server.addrManager
		// Outbound connections.
		if !p.Inbound() {
			// Request known addresses if the server address manager needs
			// more and the peer has a protocol version new enough to
			// include a timestamp with addresses.
			hasTimestamp := p.ProtocolVersion() >=
				wire.NetAddressTimeVersion
			if addrManager.NeedMoreAddresses() && hasTimestamp {
				p.QueueMessage(wire.NewMsgGetAddr(), nil)
			}

			// Mark the address as a known good address.
			addrManager.Good(p.NA())
		} else {
			// A peer might not be advertising the same address that it
			// actually connected from.  One example of why this can happen
			// is with NAT.  Only add the address to the address manager if
			// the addresses agree.
			if addrmgr.NetAddressKey(&msg.AddrMe) == addrmgr.NetAddressKey(p.NA()) {
				addrManager.AddAddress(p.NA(), p.NA())
				addrManager.Good(p.NA())
			}
		}
	}

	// Request bloom filter
	filter := bloom.NewFilter(10, 0, 0.0001, wire.BloomUpdateAll)
	for _, outpoint := range sp.server.outpoints {
		filter.AddOutPoint(outpoint)
	}
	p.QueueMessage(filter.MsgFilterLoad(), nil)

	// Add valid peer to the server.
	sp.server.AddPeer(sp)
}

func newPeerConfig(sp *serverPeer) *peer.Config {
	return &peer.Config{
		Listeners: peer.MessageListeners{
			OnVersion: sp.OnVersion,
			// Note: The reference client currently bans peers that send alerts
			// not signed with its key.  We could verify against their key, but
			// since the reference client is currently unwilling to support
			// other implementations' alert messages, we will not relay theirs.
			OnAlert: nil,
		},
		BestLocalAddress: sp.server.addrManager.GetBestLocalAddress,
		HostToNetAddress: sp.server.addrManager.HostToNetAddress,
		Proxy:            cfg.Proxy,
		UserAgentName:    userAgentName,
		UserAgentVersion: userAgentVersion,
		ChainParams:      sp.server.chainParams,
		Services:         sp.server.services,
		ProtocolVersion:  wire.SendHeadersVersion,
	}
}

func newServer(chainParams *chaincfg.Params) (*server, error) {
	services := defaultServices
	amgr := addrmgr.New(cfg.DataDir.Value, btcwalletLookup)

	s := server{
		chainParams: chainParams,
		addrManager: amgr,
		newPeers:    make(chan *serverPeer, defaultMaxOutbound),
		donePeers:   make(chan *serverPeer, defaultMaxOutbound),
		banPeers:    make(chan *serverPeer, defaultMaxOutbound),
		wakeup:      make(chan struct{}),
		query:       make(chan interface{}),
		quit:        make(chan struct{}),
		timeSource:  blockchain.NewMedianTime(),
		services:    services,
	}

	// Create a connection manager.
	connmgrCfg := &connmgr.Config{
		RetryDuration: connectionRetryInterval,
		MaxOutbound:   defaultMaxOutbound,
		Dial:          btcwalletDial,
		OnConnection: func(c *connmgr.ConnReq, conn net.Conn) {
			sp := s.newOutboundPeer(c.Addr, c.Permanent)
			if sp != nil {
				if err := sp.Connect(conn); err != nil {
					log.Errorf("Can't use connection: %v", err)
					return
				}
				sp.connReq = c
				log.Debugf("Connected to %s", sp.Addr())
				s.addrManager.Attempt(sp.NA())
			}
		},
	}
	if len(cfg.ConnectPeers) == 0 {
		connmgrCfg.GetNewAddress = func() string {
			addr := s.addrManager.GetAddress("any")
			if addr == nil {
				return ""
			}
			return addrmgr.NetAddressKey(addr.NetAddress())
		}
	}
	cmgr, err := connmgr.New(connmgrCfg)
	if err != nil {
		return nil, err
	}
	s.connManager = cmgr

	return &s, nil
}
