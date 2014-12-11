/*
 * Copyright (c) 2013, 2014 Conformal Systems LLC <info@conformal.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package chain

import (
	"errors"
	"sync"
	"time"

	"github.com/conformal/btcnet"
	"github.com/conformal/btcrpcclient"
	"github.com/conformal/btcutil"
	"github.com/conformal/btcwallet/txstore"
	"github.com/conformal/btcwallet/waddrmgr"
	"github.com/conformal/btcwire"
	"github.com/conformal/btcws"
)

type Client struct {
	*btcrpcclient.Client
	netParams *btcnet.Params

	enqueueNotification chan interface{}
	dequeueNotification chan interface{}
	currentBlock        chan *waddrmgr.BlockStamp

	// Notification channels regarding the state of the client.  These exist
	// so other components can listen in on chain activity.  These are
	// initialized as nil, and must be created by calling one of the Listen*
	// methods.
	connected        chan bool
	notificationLock sync.Locker

	quit    chan struct{}
	wg      sync.WaitGroup
	started bool
	quitMtx sync.Mutex
}

func NewClient(net *btcnet.Params, connect, user, pass string, certs []byte) (*Client, error) {
	client := Client{
		netParams:           net,
		enqueueNotification: make(chan interface{}),
		dequeueNotification: make(chan interface{}),
		currentBlock:        make(chan *waddrmgr.BlockStamp),
		notificationLock:    new(sync.Mutex),
		quit:                make(chan struct{}),
	}
	ntfnCallbacks := btcrpcclient.NotificationHandlers{
		OnClientConnected:   client.onClientConnect,
		OnBlockConnected:    client.onBlockConnected,
		OnBlockDisconnected: client.onBlockDisconnected,
		OnRecvTx:            client.onRecvTx,
		OnRedeemingTx:       client.onRedeemingTx,
		OnRescanFinished:    client.onRescanFinished,
		OnRescanProgress:    client.onRescanProgress,
	}
	conf := btcrpcclient.ConnConfig{
		Host:                connect,
		Endpoint:            "ws",
		User:                user,
		Pass:                pass,
		Certificates:        certs,
		DisableConnectOnNew: true,
	}
	c, err := btcrpcclient.New(&conf, &ntfnCallbacks)
	if err != nil {
		return nil, err
	}
	client.Client = c
	return &client, nil
}

func (c *Client) Start() error {
	err := c.Connect(5) // attempt connection 5 tries at most
	if err != nil {
		return err
	}

	// Verify that the server is running on the expected network.
	net, err := c.GetCurrentNet()
	if err != nil {
		c.Disconnect()
		return err
	}
	if net != c.netParams.Net {
		c.Disconnect()
		return errors.New("mismatched networks")
	}

	c.quitMtx.Lock()
	c.started = true
	c.quitMtx.Unlock()

	c.wg.Add(1)
	go c.handler()
	return nil
}

func (c *Client) Stop() {
	c.quitMtx.Lock()
	defer c.quitMtx.Unlock()

	select {
	case <-c.quit:
	default:
		close(c.quit)
		c.Client.Shutdown()

		if !c.started {
			close(c.dequeueNotification)
		}
	}
}

func (c *Client) WaitForShutdown() {
	c.Client.WaitForShutdown()
	c.wg.Wait()
}

func (c *Client) Notifications() <-chan interface{} {
	return c.dequeueNotification
}

func (c *Client) BlockStamp() (*waddrmgr.BlockStamp, error) {
	select {
	case bs := <-c.currentBlock:
		return bs, nil
	case <-c.quit:
		return nil, errors.New("disconnected")
	}
}

// Notification types.  These are defined here and processed from from reading
// a notificationChan to avoid handling these notifications directly in
// btcrpcclient callbacks, which isn't very Go-like and doesn't allow
// blocking client calls.
type (
	BlockConnected    waddrmgr.BlockStamp
	BlockDisconnected waddrmgr.BlockStamp
	RecvTx            struct {
		Tx    *btcutil.Tx    // Index is guaranteed to be set.
		Block *txstore.Block // nil if unmined
	}
	RedeemingTx struct {
		Tx    *btcutil.Tx    // Index is guaranteed to be set.
		Block *txstore.Block // nil if unmined
	}
	RescanProgress struct {
		Hash   *btcwire.ShaHash
		Height int32
		Time   time.Time
	}
	RescanFinished struct {
		Hash   *btcwire.ShaHash
		Height int32
		Time   time.Time
	}
)

// parseBlock parses a btcws definition of the block a tx is mined it to the
// Block structure of the txstore package, and the block index.  This is done
// here since btcrpcclient doesn't parse this nicely for us.
func parseBlock(block *btcws.BlockDetails) (blk *txstore.Block, idx int, err error) {
	if block == nil {
		return nil, btcutil.TxIndexUnknown, nil
	}
	blksha, err := btcwire.NewShaHashFromStr(block.Hash)
	if err != nil {
		return nil, btcutil.TxIndexUnknown, err
	}
	blk = &txstore.Block{
		Height: block.Height,
		Hash:   *blksha,
		Time:   time.Unix(block.Time, 0),
	}
	return blk, block.Index, nil
}

func (c *Client) onClientConnect() {
	log.Info("Established websocket RPC connection to btcd")
	c.notifyConnected(true)
}

func (c *Client) onBlockConnected(hash *btcwire.ShaHash, height int32) {
	c.enqueueNotification <- BlockConnected{Hash: *hash, Height: height}
}

func (c *Client) onBlockDisconnected(hash *btcwire.ShaHash, height int32) {
	c.enqueueNotification <- BlockDisconnected{Hash: *hash, Height: height}
}

func (c *Client) onRecvTx(tx *btcutil.Tx, block *btcws.BlockDetails) {
	var blk *txstore.Block
	index := btcutil.TxIndexUnknown
	if block != nil {
		var err error
		blk, index, err = parseBlock(block)
		if err != nil {
			// Log and drop improper notification.
			log.Errorf("recvtx notification bad block: %v", err)
			return
		}
	}
	tx.SetIndex(index)
	c.enqueueNotification <- RecvTx{tx, blk}
}

func (c *Client) onRedeemingTx(tx *btcutil.Tx, block *btcws.BlockDetails) {
	var blk *txstore.Block
	index := btcutil.TxIndexUnknown
	if block != nil {
		var err error
		blk, index, err = parseBlock(block)
		if err != nil {
			// Log and drop improper notification.
			log.Errorf("redeemingtx notification bad block: %v", err)
			return
		}
	}
	tx.SetIndex(index)
	c.enqueueNotification <- RedeemingTx{tx, blk}
}

func (c *Client) onRescanProgress(hash *btcwire.ShaHash, height int32, blkTime time.Time) {
	c.enqueueNotification <- &RescanProgress{hash, height, blkTime}
}

func (c *Client) onRescanFinished(hash *btcwire.ShaHash, height int32, blkTime time.Time) {
	c.enqueueNotification <- &RescanFinished{hash, height, blkTime}
}

// handler maintains a queue of notifications and the current state (best
// block) of the chain.
func (c *Client) handler() {
	hash, height, err := c.GetBestBlock()
	if err != nil {
		close(c.quit)
		c.wg.Done()
	}

	bs := &waddrmgr.BlockStamp{Hash: *hash, Height: height}

	// TODO: Rather than leaving this as an unbounded queue for all types of
	// notifications, try dropping ones where a later enqueued notification
	// can fully invalidate one waiting to be processed.  For example,
	// blockconnected notifications for greater block heights can remove the
	// need to process earlier blockconnected notifications still waiting
	// here.

	var notifications []interface{}
	enqueue := c.enqueueNotification
	var dequeue chan interface{}
	var next interface{}
out:
	for {
		select {
		case n, ok := <-enqueue:
			if !ok {
				// If no notifications are queued for handling,
				// the queue is finished.
				if len(notifications) == 0 {
					break out
				}
				// nil channel so no more reads can occur.
				enqueue = nil
				continue
			}
			if len(notifications) == 0 {
				next = n
				dequeue = c.dequeueNotification
			}
			notifications = append(notifications, n)

		case dequeue <- next:
			if n, ok := next.(BlockConnected); ok {
				bs = (*waddrmgr.BlockStamp)(&n)
			}

			notifications[0] = nil
			notifications = notifications[1:]
			if len(notifications) != 0 {
				next = notifications[0]
			} else {
				// If no more notifications can be enqueued, the
				// queue is finished.
				if enqueue == nil {
					break out
				}
				dequeue = nil
			}

		case c.currentBlock <- bs:

		case <-c.quit:
			break out
		}
	}
	close(c.dequeueNotification)
	c.wg.Done()
}

// ErrDuplicateListen is returned for any attempts to listen for the same
// notification more than once.  If callers must pass along a notifiation to
// multiple places, they must broadcast it themself.
var ErrDuplicateListen = errors.New("duplicate listen")

type noopLocker struct{}

func (noopLocker) Lock()   {}
func (noopLocker) Unlock() {}

// ListenConnected returns a channel that passes the current connection state
// of the client.  This will be automatically sent to when the client is first
// connected, as well as the current state whenever NotifyConnected is
// forcibly called.
//
// If this is called twice, ErrDuplicateListen is returned.
func (c *Client) ListenConnected() (<-chan bool, error) {
	c.notificationLock.Lock()
	defer c.notificationLock.Unlock()

	if c.connected != nil {
		return nil, ErrDuplicateListen
	}
	c.connected = make(chan bool)
	c.notificationLock = noopLocker{}
	return c.connected, nil
}

func (c *Client) notifyConnected(connected bool) {
	c.notificationLock.Lock()
	if c.connected != nil {
		c.connected <- connected
	}
	c.notificationLock.Unlock()
}

// NotifyConnected sends the channel notification for a connected or
// disconnected client.  This is exported so it can be called by other
// packages which require notifying the current connection state.
//
// TODO: This shouldn't exist, but the current notification API requires it.
func (c *Client) NotifyConnected() {
	connected := !c.Client.Disconnected()
	c.notifyConnected(connected)
}
