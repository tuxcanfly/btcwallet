// Copyright (c) 2016 The btcsuite developers Use of this source code is
// governed by an ISC license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"net"
	"sync"

	"github.com/btcsuite/btcd/addrmgr"
	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/blockchain/indexers"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/database"
	"github.com/btcsuite/btcd/peer"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/bloom"
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

	connReq         *connmgr.ConnReq
	server          *server
	persistent      bool
	continueHash    *wire.ShaHash
	relayMtx        sync.Mutex
	disableRelayTx  bool
	requestQueue    []*wire.InvVect
	requestedTxns   map[wire.ShaHash]struct{}
	requestedBlocks map[wire.ShaHash]struct{}
	filter          *bloom.Filter
	knownAddresses  map[string]struct{}
	banScore        connmgr.DynamicBanScore
	quit            chan struct{}
	// The following chans are used to sync blockmanager and server.
	txProcessed    chan struct{}
	blockProcessed chan struct{}
}

// server provides a bitcoin server for handling communications to and from
// bitcoin peers.
type server struct {
	// The following variables must only be used atomically.
	// Putting the uint64s first makes them 64-bit aligned for 32-bit systems.
	bytesReceived uint64 // Total bytes received from all peers since start.
	bytesSent     uint64 // Total bytes sent by all peers since start.
	started       int32
	shutdown      int32
	shutdownSched int32

	listeners            []net.Listener
	chainParams          *chaincfg.Params
	addrManager          *addrmgr.AddrManager
	sigCache             *txscript.SigCache
	modifyRebroadcastInv chan interface{}
	pendingPeers         chan *serverPeer
	newPeers             chan *serverPeer
	donePeers            chan *serverPeer
	banPeers             chan *serverPeer
	retryPeers           chan *serverPeer
	wakeup               chan struct{}
	query                chan interface{}
	wg                   sync.WaitGroup
	quit                 chan struct{}
	db                   database.DB
	timeSource           blockchain.MedianTimeSource
	services             wire.ServiceFlag

	// The following fields are used for optional indexes.  They will be nil
	// if the associated index is not enabled.  These fields are set during
	// initial creation of the server and never changed afterwards, so they
	// do not need to be protected for concurrent access.
	txIndex   *indexers.TxIndex
	addrIndex *indexers.AddrIndex
}

func newPeerConfig(sp *serverPeer) *peer.Config {
	return &peer.Config{
		Listeners: peer.MessageListeners{
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
