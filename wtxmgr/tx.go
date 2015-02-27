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

package wtxmgr

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/walletdb"
)

// TxRecord is the record type for all transactions in the store.  If the
// transaction is mined, BlockTxKey will be the lookup key for the transaction.
// Otherwise, the embedded BlockHeight will be -1.
type TxRecord struct {
	BlockTxKey
	*txRecord
	s *Store
}

// Debits is the type representing any TxRecord which debits from previous
// wallet transaction outputs.
type Debits struct {
	*TxRecord
}

// Credit is the type representing a transaction output which was spent or
// is still spendable by wallet.  A UTXO is an unspent Credit, but not all
// Credits are UTXOs.
type Credit struct {
	*TxRecord
	OutputIndex uint32
}

// blockAmounts holds the immediately spendable and reward (coinbase) amounts
// as a result of all transactions in a block.
type blockAmounts struct {
	Spendable btcutil.Amount
	Reward    btcutil.Amount
}

// Store implements a transaction store for storing and managing wallet
// transactions.
type Store struct {
	namespace walletdb.Namespace

	mtx sync.RWMutex

	// unconfirmed holds a collection of wallet transactions that have not
	// been mined into a block yet.
	unconfirmed unconfirmedStore

	// Channels to notify callers of changes to the transaction store.
	// These are only created when a caller calls the appropiate
	// registration method.
	newCredit        chan Credit
	newDebits        chan Debits
	minedCredit      chan Credit
	minedDebits      chan Debits
	notificationLock sync.Locker
}

// Block holds details about a block that contains wallet transactions.
type Block struct {
	// Block holds the hash, time, and height of the block.
	Hash   wire.ShaHash
	Time   time.Time
	Height int32

	// amountDeltas is the net increase or decrease of BTC controllable by
	// wallet addresses due to transactions in this block.  This value only
	// tracks the total amount, not amounts for individual addresses (which
	// this txstore implementation is not aware of).
	amountDeltas blockAmounts
}

// BlockTxKey is a lookup key for a single mined transaction in the store.
type BlockTxKey struct {
	BlockIndex  int
	BlockHeight int32
}

// BlockOutputKey is a lookup key for a transaction output from a block in the
// store.
type BlockOutputKey struct {
	BlockTxKey
	OutputIndex uint32
}

// txRecord holds all credits and debits created by a transaction's inputs and
// outputs.
type txRecord struct {
	// tx is the transaction that all records in this structure reference.
	tx *btcutil.Tx

	// debit records the debits this transaction creates, or nil if the
	// transaction does not spend any previous credits.
	debits *debits

	// credits holds all records for received transaction outputs from
	// this transaction.
	credits []*credit

	received time.Time
}

// debits records the debits a transaction record makes from previous wallet
// transaction credits.
type debits struct {
	amount btcutil.Amount
	spends []BlockOutputKey
}

// credit describes a transaction output which was or is spendable by wallet.
type credit struct {
	change  bool
	spentBy *BlockTxKey // nil if unspent
}

// lookupBlock fetches the block at the given height from the store.
// It returns ErrBlockNotFound if no block with the given height is saved in
// the store.
func (s *Store) lookupBlock(height int32) (*Block, error) {
	var block *Block
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		block, err = fetchBlockByHeight(wtx, height)
		return err
	})
	return block, err
}

// deleteBlock deletes the block at the given height from the store.
func (s *Store) deleteBlock(height int32) error {
	return s.namespace.Update(func(wtx walletdb.Tx) error {
		return deleteBlock(wtx, height)
	})
}

// records returns a slice of transaction records upto the given height saved
// by the store.
func (s *Store) records(height int32) ([]*txRecord, error) {
	var records []*txRecord
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		records, err = fetchBlockTxRecords(wtx, height)
		return err
	})
	return records, err
}

// lookupBlockTx fetches the transaction record with the given block tx key
// from the store.
// It returns ErrTxHashNotFound if no transaction hash is mapped to the given
// block tx key
// It returns ErrTxRecordNotFound if no transaction record with the given hash
// is found.
func (s *Store) lookupBlockTx(key BlockTxKey) (*txRecord, error) {
	var txHash *wire.ShaHash
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		txHash, err = fetchTxHashFromBlockTxKey(wtx, &key)
		return err
	})
	if err != nil {
		return nil, err
	}
	var record *txRecord
	err = s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		record, err = fetchTxRecord(wtx, txHash)
		return err
	})
	return record, err
}

// lookupBlockDebits returns the debits the given block tx key creates.
// It returns MissingDebitsError if the given block tx key has no debits.
func (s *Store) lookupBlockDebits(key BlockTxKey) (*debits, error) {
	r, err := s.lookupBlockTx(key)
	if err != nil {
		return nil, err
	}
	if r.debits == nil {
		str := fmt.Sprintf("missing record for debits at block %d index %d",
			key.BlockHeight, key.BlockIndex)
		return nil, txStoreError(ErrMissingDebits, str, nil)
	}
	return r.debits, nil
}

// lookupBlockCredit returns the credits the given block output key creates.
// It returns MissingCreditError if the given block output key has no credits.
func (r *txRecord) lookupBlockCredit(key BlockOutputKey) (*credit, error) {
	switch {
	case len(r.credits) <= int(key.OutputIndex):
		fallthrough
	case r.credits[key.OutputIndex] == nil:
		str := fmt.Sprintf("missing record for received transaction output at "+
			"block %d index %d output %d", key.BlockHeight, key.BlockIndex,
			key.OutputIndex)
		return nil, txStoreError(ErrMissingCredit, str, nil)
	}
	return r.credits[key.OutputIndex], nil
}

// lookupBlockCredit returns the credits the given block output key creates.
// It returns MissingCreditError if the given block output key has no credits.
func (s *Store) lookupBlockCredit(key BlockOutputKey) (*credit, error) {
	txRecord, err := s.lookupBlockTx(key.BlockTxKey)
	if err != nil {
		return nil, err
	}
	return txRecord.lookupBlockCredit(key)
}

// insertBlock inserts the given block into the store.
// If a block at the same height already exists in the store,
// it fetches and returns the block from the store.
func (s *Store) insertBlock(block *Block) (*Block, error) {
	b, err := s.lookupBlock(block.Height)
	if err != nil {
		switch err.(TxStoreError).ErrorCode {
		case ErrBlockNotFound:
			b = &Block{
				Hash:   block.Hash,
				Time:   block.Time,
				Height: block.Height,
			}
			err := s.namespace.Update(func(wtx walletdb.Tx) error {
				return putBlock(wtx, b)
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return b, nil
}

// updateBlock updates the given block in the store.
func (s *Store) updateBlock(b *Block) error {
	if _, err := s.lookupBlock(b.Height); err != nil {
		return err
	}
	err := s.namespace.Update(func(wtx walletdb.Tx) error {
		return updateBlock(wtx, b)
	})
	return err
}

func (s *Store) putCredit(hash *wire.ShaHash, c *credit) error {
	return s.namespace.Update(func(wtx walletdb.Tx) error {
		return putCredit(wtx, hash, c)
	})
}

func (s *Store) updateCredit(hash *wire.ShaHash, i uint32, c *credit) error {
	return s.namespace.Update(func(wtx walletdb.Tx) error {
		return updateCredit(wtx, hash, i, c)
	})
}

func (s *Store) putDebits(hash *wire.ShaHash, d *debits) error {
	return s.namespace.Update(func(wtx walletdb.Tx) error {
		return putDebits(wtx, hash, d)
	})
}

// insertTxRecord inserts the given transaction record into the given block.
func (s *Store) insertTxRecord(r *txRecord, block *Block) error {
	log.Infof("Inserting transaction %v from block %d", r.tx.Sha(), block.Height)
	return s.namespace.Update(func(wtx walletdb.Tx) error {
		return putTxRecord(wtx, block, r)
	})
}

// lookupTxRecord fetches the transaction record with the given transaction
// hash.
// It returns ErrTxRecordNotFound if no transaction record with the given hash
// is found.
func (s *Store) lookupTxRecord(hash *wire.ShaHash) (*txRecord,
	error) {
	var record *txRecord
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		record, err = fetchTxRecord(wtx, hash)
		return err
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return record, nil
}

// insertUnspent inserts the given unspent outpoint and it's corresponding
// block tx key.
func (s *Store) insertUnspent(op *wire.OutPoint, key *BlockTxKey) error {
	return s.namespace.Update(func(wtx walletdb.Tx) error {
		return putUnspent(wtx, op, key)
	})
}

// insertBlockTxRecord inserts the given transaction record into the given
// block.
func (s *Store) insertBlockTxRecord(r *txRecord, block *Block) error {
	if _, err := s.insertBlock(block); err != nil {
		return err
	}
	return s.insertTxRecord(r, block)
}

// blocks returns a slice of all the blocks in the store.
func (s *Store) blocks() ([]*Block, error) {
	var blocks []*Block
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		blocks, err = fetchAllBlocks(wtx)
		return err
	})
	return blocks, err
}

// sliceBlocks returns a slice of blocks with height greater than or equal to
// the given height.
func (s *Store) sliceBlocks(height int32) ([]*Block, error) {
	var blocks []*Block
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		blocks, err = fetchBlocks(wtx, height)
		return err
	})
	return blocks, err
}

func (s *Store) moveMinedTx(r *txRecord, block *Block) error {
	log.Infof("Marking unconfirmed transaction %v mined in block %d",
		r.tx.Sha(), block.Height)

	if err := s.unconfirmed.deleteTxRecord(r.Tx()); err != nil {
		return err
	}

	// Find collection and insert records.  Error out if there are records
	// saved for this block and index.
	key := BlockTxKey{r.Tx().Index(), block.Height}
	b, err := s.insertBlock(block)
	if err != nil {
		return err
	}
	if err := s.insertBlockTxRecord(r, block); err != nil {
		return err
	}

	for _, input := range r.Tx().MsgTx().TxIn {
		if err := s.unconfirmed.deletePrevOutPointSpender(&input.PreviousOutPoint); err != nil {
			return err
		}

		// For all mined transactions with credits spent by this
		// transaction, remove them from the spentBlockOutPoints map
		// (signifying that there is no longer an unconfirmed
		// transaction which spending that credit), and update the
		// credit's spent by tracking with the block key of the
		// newly-mined transaction.
		prev, err := s.unconfirmed.lookupSpentBlockOutPointKey(&input.PreviousOutPoint)
		if err != nil {
			continue
		}
		if err := s.unconfirmed.deleteSpentBlockOutpoint(&input.PreviousOutPoint, prev); err != nil {
			return err
		}
		rr, err := s.lookupBlockTx(prev.BlockTxKey)
		if err != nil {
			return err
		}
		rr.credits[prev.OutputIndex].spentBy = &key
		// debits should already be non-nil
		r.debits.spends = append(r.debits.spends, *prev)
	}
	if r.debits != nil {
		d := Debits{&TxRecord{key, r, s}}
		s.notifyMinedDebits(d)
	}

	// For each credit in r, if the credit is spent by another unconfirmed
	// transaction, move the spending transaction from spentUnconfirmed
	// (which signifies a transaction spending another unconfirmed tx) to
	// spentBlockTxs (which signifies an unconfirmed transaction spending a
	// confirmed tx) and modify the mined transaction's record to refer to
	// the credit being spent by an unconfirmed transaction.
	//
	// If the credit is not spent, modify the store's unspent bookkeeping
	// maps to include the credit and increment the amount deltas by the
	// credit's value.
	op := wire.OutPoint{Hash: *r.Tx().Sha()}
	for i, credit := range r.credits {
		if credit == nil {
			continue
		}
		op.Index = uint32(i)
		outputKey := BlockOutputKey{key, op.Index}
		if rr, err := s.unconfirmed.fetchPrevOutPointSpender(&op); err == nil {
			if err := s.unconfirmed.deleteOutPointSpender(&op); err != nil {
				return err
			}
			if err := s.unconfirmed.setBlockOutPointSpender(&op, &outputKey, rr); err != nil {
				return err
			}
			credit.spentBy = &BlockTxKey{BlockHeight: -1}
		} else if credit.spentBy == nil {
			// Mark outpoint unspent.
			if err := s.insertUnspent(&op, &key); err != nil {
				return err
			}

			// Increment spendable amount delta as a result of
			// moving this credit to this block.
			value := r.Tx().MsgTx().TxOut[i].Value
			b.amountDeltas.Spendable += btcutil.Amount(value)
		}

		c := Credit{&TxRecord{key, r, s}, op.Index}
		s.notifyMinedCredit(c)
	}

	// If this moved transaction debits from any previous credits, decrement
	// the amount deltas by the total input amount.  Because this
	// transaction was moved from the unconfirmed transaction set, this can
	// never be a coinbase.
	if r.debits != nil {
		b.amountDeltas.Spendable -= r.debits.amount
	}

	return s.updateBlock(b)
}

// InsertTx records a transaction as belonging to a wallet's transaction
// history.  If block is nil, the transaction is considered unspent, and the
// transaction's index must be unset.  Otherwise, the transaction index must be
// set if a non-nil block is set.
//
// The transaction record is returned.  Credits and debits may be added to the
// transaction by calling methods on the TxRecord.
func (s *Store) InsertTx(tx *btcutil.Tx, block *Block) (*TxRecord, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	// The receive time will be the earlier of now and the block time
	// (if any).
	received := time.Now()

	// Verify that the index of the transaction within the block is
	// set if a block is set, and unset if there is no block.
	index := tx.Index()
	switch {
	case index == btcutil.TxIndexUnknown && block != nil:
		return nil, errors.New("transaction block index unset")
	case index != btcutil.TxIndexUnknown && block == nil:
		return nil, errors.New("transaction block index set")
	}

	// Simply create or return the transaction record if this transaction
	// is unconfirmed.
	if block == nil {
		r, err := s.unconfirmed.insertTxRecord(tx)
		if err != nil {
			return nil, err
		}
		r.received = received
		return &TxRecord{BlockTxKey{BlockHeight: -1}, r, s}, nil
	}

	// Check if block records already exist for this tx.  If so,
	// we're done.
	key := BlockTxKey{index, block.Height}
	record, err := s.lookupBlockTx(key)
	if err != nil {
		if !isTxHashNotFoundErr(err) {
			return nil, err
		}
	} else {
		// Verify that the txs actually match.
		if *record.tx.Sha() != *tx.Sha() {
			str := "inconsistent transaction store"
			return nil, txStoreError(ErrInconsistentStore, str, nil)
		}
		return &TxRecord{key, record, s}, nil
	}

	// If the exact tx (not a double spend) is already included but
	// unconfirmed, move it to a block.
	if r, err := s.unconfirmed.lookupTxRecord(tx.Sha()); err == nil {
		r.Tx().SetIndex(tx.Index())
		if err := s.moveMinedTx(r, block); err != nil {
			return nil, err
		}
		return &TxRecord{key, r, s}, nil
	}

	// If this transaction is not already saved unconfirmed, remove all
	// unconfirmed transactions that are now invalidated due to being a
	// double spend.  This also handles killing unconfirmed transaction
	// spend chains if any other unconfirmed transactions spend outputs
	// of the removed double spend.
	if err := s.removeDoubleSpends(tx); err != nil {
		return nil, err
	}

	r := &txRecord{tx: tx}
	if err := s.insertBlockTxRecord(r, block); err != nil {
		return nil, err
	}
	if r.received.IsZero() {
		if !block.Time.IsZero() && block.Time.Before(received) {
			received = block.Time
		}
		r.received = received
	}
	return &TxRecord{key, r, s}, nil
}

// Received returns the earliest known time the transaction was received by.
func (t *TxRecord) Received() time.Time {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	return t.received
}

// Block returns the block details for a transaction.  If the transaction is
// unmined, both the block and returned error are nil.
func (t *TxRecord) Block() (*Block, error) {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	b, err := t.s.lookupBlock(t.BlockHeight)
	if isMissingBlockErr(err) {
		return nil, nil
	}
	return b, err
}

// AddDebits marks a transaction record as having debited from previous wallet
// credits.
func (t *TxRecord) AddDebits() (Debits, error) {
	t.s.mtx.Lock()
	defer t.s.mtx.Unlock()

	if t.debits == nil {
		spent, err := t.s.findPreviousCredits(t.Tx())
		if err != nil {
			return Debits{}, err
		}
		debitAmount, err := t.s.markOutputsSpent(spent, t)
		if err != nil {
			return Debits{}, err
		}

		prevOutputKeys := make([]BlockOutputKey, len(spent))
		for i, c := range spent {
			prevOutputKeys[i] = c.outputKey()
		}

		t.debits = &debits{amount: debitAmount, spends: prevOutputKeys}

		log.Debugf("Transaction %v spends %d previously-unspent "+
			"%s totaling %v", t.tx.Sha(), len(spent),
			pickNoun(len(spent), "output", "outputs"), debitAmount)
	}

	switch t.BlockHeight {
	case -1: // unconfirmed
		if t.txRecord.debits != nil {
			if err := t.s.unconfirmed.putDebits(t.tx.Sha(), t.debits); err != nil {
				return Debits{}, err
			}
		}
	default:
		if t.txRecord.debits != nil {
			if err := t.s.putDebits(t.tx.Sha(), t.debits); err != nil {
				return Debits{}, err
			}
		}
	}

	d := Debits{t}
	t.s.notifyNewDebits(d)
	return d, nil
}

// findPreviousCredits searches for all unspent credits that make up the inputs
// for tx.
func (s *Store) findPreviousCredits(tx *btcutil.Tx) ([]Credit, error) {
	type createdCredit struct {
		credit Credit
		err    error
	}

	inputs := tx.MsgTx().TxIn
	creditChans := make([]chan createdCredit, len(inputs))
	for i, txIn := range inputs {
		creditChans[i] = make(chan createdCredit)
		go func(i int, op wire.OutPoint) {
			key, err := s.lookupUnspentOutput(&op)
			if err != nil {
				// Does this input spend an unconfirmed output?
				var spent bool
				if _, err := s.unconfirmed.fetchPrevOutPointSpender(&op); err == nil {
					spent = true
				}
				r, err := s.unconfirmed.lookupTxRecord(&op.Hash)
				switch {
				// Not an unconfirmed tx.
				case err != nil:
					fallthrough
				// Output isn't a credit.
				case len(r.credits) <= int(op.Index):
					fallthrough
				// Output isn't a credit.
				case r.credits[op.Index] == nil:
					fallthrough
				// Credit already spent.
				case spent == true:
					close(creditChans[i])
					return
				}
				t := &TxRecord{BlockTxKey{BlockHeight: -1}, r, s}
				c := Credit{t, op.Index}
				creditChans[i] <- createdCredit{credit: c}
				return
			}
			r, err := s.lookupBlockTx(*key)
			if err != nil {
				creditChans[i] <- createdCredit{err: err}
				return
			}
			t := &TxRecord{*key, r, s}
			c := Credit{t, op.Index}
			creditChans[i] <- createdCredit{credit: c}
		}(i, txIn.PreviousOutPoint)
	}
	spent := make([]Credit, 0, len(inputs))
	for _, c := range creditChans {
		cc, ok := <-c
		if !ok {
			continue
		}
		if cc.err != nil {
			return nil, cc.err
		}
		spent = append(spent, cc.credit)
	}
	return spent, nil
}

// markOutputsSpent marks each previous credit spent by t as spent.  The total
// input of all spent previous outputs is returned.
func (s *Store) markOutputsSpent(spent []Credit, t *TxRecord) (btcutil.Amount, error) {
	var a btcutil.Amount
	for _, prev := range spent {
		op := prev.outPoint()
		switch prev.BlockHeight {
		case -1: // unconfirmed
			if t.BlockHeight != -1 {
				// a confirmed tx cannot spend a previous output from an unconfirmed tx
				str := "inconsistent transaction store"
				return 0, txStoreError(ErrInconsistentStore, str, nil)
			}
			op := prev.outPoint()
			if err := s.unconfirmed.setOutPointSpender(op, t.txRecord); err != nil {
				return 0, err
			}
		default:
			// Update spent info.
			credit := prev.txRecord.credits[prev.OutputIndex]
			if credit.spentBy != nil {
				if *credit.spentBy == t.BlockTxKey {
					continue
				}
				str := "inconsistent transaction store"
				return 0, txStoreError(ErrInconsistentStore, str, nil)
			}
			credit.spentBy = &t.BlockTxKey
			if err := s.deleteUnspentOutput(op); err != nil {
				return 0, err
			}
			if t.BlockHeight == -1 { // unconfirmed
				key := prev.outputKey()
				if err := s.unconfirmed.setBlockOutPointSpender(op, &key, t.txRecord); err != nil {
					return 0, err
				}
			}

			// Increment total debited amount.
			a += prev.amount()
			if err := s.updateCredit(prev.tx.Sha(), prev.OutputIndex, credit); err != nil {
				return 0, err
			}
		}
	}

	// If t refers to a mined transaction, update its block's amount deltas
	// by the total debited amount.
	if t.BlockHeight != -1 {
		b, err := s.lookupBlock(t.BlockHeight)
		if err != nil {
			return 0, err
		}
		b.amountDeltas.Spendable -= a
		err = t.s.updateBlock(b)
		if err != nil {
			return 0, err
		}
	}

	return a, nil
}

func (t *TxRecord) setCredit(index uint32, change bool, tx *btcutil.Tx) error {
	if t.txRecord.credits == nil {
		t.txRecord.credits = make([]*credit, 0, len(tx.MsgTx().TxOut))
	}
	for i := uint32(len(t.txRecord.credits)); i <= index; i++ {
		t.txRecord.credits = append(t.txRecord.credits, nil)
	}
	if t.txRecord.credits[index] != nil {
		if *t.txRecord.tx.Sha() == *tx.Sha() {
			str := "duplicate insert"
			return txStoreError(ErrDuplicateInsert, str, nil)
		}
		str := "inconsistent transaction store"
		return txStoreError(ErrInconsistentStore, str, nil)
	}
	t.txRecord.credits[index] = &credit{change: change}
	return nil
}

// AddCredit marks the transaction record as containing a transaction output
// spendable by wallet.  The output is added unspent, and is marked spent
// when a new transaction spending the output is inserted into the store.
func (t *TxRecord) AddCredit(index uint32, change bool) (Credit, error) {
	t.s.mtx.Lock()
	defer t.s.mtx.Unlock()

	if len(t.tx.MsgTx().TxOut) <= int(index) {
		return Credit{}, errors.New("transaction output does not exist")
	}

	if err := t.setCredit(index, change, t.tx); err != nil {
		if isDuplicateInsertErr(err) {
			return Credit{t, index}, nil
		}
		return Credit{}, err
	}

	txOutAmt := btcutil.Amount(t.tx.MsgTx().TxOut[index].Value)
	log.Debugf("Marking transaction %v output %d (%v) spendable",
		t.tx.Sha(), index, txOutAmt)

	switch t.BlockHeight {
	case -1: // unconfirmed
		if err := t.s.unconfirmed.putCredit(t.tx.Sha(), &credit{change: change}); err != nil {
			return Credit{}, err
		}
	default:
		b, err := t.s.lookupBlock(t.BlockHeight)
		if err != nil {
			return Credit{}, err
		}

		// New outputs are added unspent.
		op := wire.OutPoint{Hash: *t.tx.Sha(), Index: index}
		if err := t.s.insertUnspent(&op, &t.BlockTxKey); err != nil {
			return Credit{}, err
		}
		switch t.tx.Index() {
		case 0: // Coinbase
			b.amountDeltas.Reward += txOutAmt
		default:
			b.amountDeltas.Spendable += txOutAmt
		}
		if err := t.s.updateBlock(b); err != nil {
			return Credit{}, err
		}
		if err := t.s.putCredit(t.tx.Sha(), &credit{change: change}); err != nil {
			return Credit{}, err
		}
	}
	c := Credit{t, index}
	t.s.notifyNewCredit(c)
	return c, nil
}

// Rollback removes all blocks at height onwards, moving any transactions within
// each block to the unconfirmed pool.
func (s *Store) Rollback(height int32) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	detached, err := s.sliceBlocks(height)
	if err != nil {
		return err
	}

	for _, b := range detached {
		records, err := s.records(b.Height)
		if err != nil {
			return err
		}
		movedTxs := len(records)
		// Don't include coinbase transaction with number of moved txs.
		// There should always be at least one tx in a block collection,
		// and if there is a coinbase, it would be at index 0.
		key := BlockTxKey{0, b.Height}
		tx, err := s.lookupBlockTx(key)
		if err == nil && tx.Tx().Index() == 0 {
			movedTxs--
		}
		log.Infof("Rolling back block %d (%d transactions marked "+
			"unconfirmed)", b.Height, movedTxs)
		if err := s.deleteBlock(b.Height); err != nil {
			return err
		}
		for _, r := range records {
			oldTxIndex := r.Tx().Index()

			// If the removed transaction is a coinbase, do not move
			// it to unconfirmed.
			if oldTxIndex == 0 {
				continue
			}

			r.Tx().SetIndex(btcutil.TxIndexUnknown)
			if _, err := s.unconfirmed.insertTxRecord(r.Tx()); err != nil {
				return err
			}

			// For each detached spent credit, remove from the
			// store's unspent map, and lookup the spender and
			//  modify its debit record to reference spending an
			// unconfirmed transaction.
			for outIdx, credit := range r.credits {
				if credit == nil {
					continue
				}

				op := wire.OutPoint{
					Hash:  *r.Tx().Sha(),
					Index: uint32(outIdx),
				}
				if err := s.deleteUnspentOutput(&op); err != nil {
					return err
				}

				spenderKey := credit.spentBy
				if spenderKey == nil {
					continue
				}

				prev := BlockOutputKey{
					BlockTxKey: BlockTxKey{
						BlockIndex:  oldTxIndex,
						BlockHeight: b.Height,
					},
					OutputIndex: uint32(outIdx),
				}

				// Lookup txRecord of the spending transaction.  Spent
				// tracking differs slightly depending on whether the
				// spender is confirmed or not.
				switch spenderKey.BlockHeight {
				case -1: // unconfirmed
					spender, err := s.unconfirmed.fetchBlockOutPointSpender(&prev)
					if err != nil {
						return err
					}

					// Swap the maps the spender is saved in.
					if err := s.unconfirmed.deleteSpentBlockOutpoint(&op, &prev); err != nil {
						return err
					}
					if err := s.unconfirmed.setOutPointSpender(&op, spender); err != nil {
						return err
					}

				default:
					spender, err := s.lookupBlockTx(*spenderKey)
					if err != nil {
						return err
					}

					if spender.debits == nil {
						str := fmt.Sprintf("missing record for debits at block %d index %d",
							spenderKey.BlockHeight, spenderKey.BlockIndex)
						return txStoreError(ErrMissingDebits, str, nil)
					}

					current := BlockOutputKey{
						BlockTxKey: BlockTxKey{BlockHeight: -1},
					}
					err = spender.swapDebits(prev, current)
					if err != nil {
						return err
					}
				}

			}

			// If this transaction debits any previous credits,
			// modify each previous credit to mark it as spent
			// by an unconfirmed tx.
			if r.debits != nil {
				for _, prev := range r.debits.spends {
					rr, err := s.lookupBlockTx(prev.BlockTxKey)
					if err != nil {
						return err
					}
					c, err := rr.lookupBlockCredit(prev)
					if err != nil {
						return err
					}
					op := wire.OutPoint{
						Hash:  *rr.Tx().Sha(),
						Index: prev.OutputIndex,
					}
					if err := s.unconfirmed.setBlockOutPointSpender(&op, &prev, r); err != nil {
						return err
					}
					c.spentBy = &BlockTxKey{BlockHeight: -1}
				}

				// Debit tracking for unconfirmed transactions is
				// done by the unconfirmed store's maps, and not
				// in the txRecord itself.
				r.debits.spends = nil
			}
		}
	}
	return nil
}

func (r *txRecord) swapDebits(previous, current BlockOutputKey) error {
	for i, outputKey := range r.debits.spends {
		if outputKey == previous {
			r.debits.spends[i] = current
			return nil
		}
	}

	str := fmt.Sprintf("missing record for received transaction output at "+
		"block %d index %d output %d", previous.BlockHeight, previous.BlockIndex,
		previous.OutputIndex)
	return txStoreError(ErrMissingCredit, str, nil)
}

// UnminedDebitTxs returns the underlying transactions for all wallet
// transactions which debit from previous outputs and are not known to have
// been mined in a block.
func (s *Store) UnminedDebitTxs() []*btcutil.Tx {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	unconfirmed, err := s.unconfirmed.records()
	if err != nil {
		log.Errorf("Error fetching records: %v", err)
		return nil
	}
	unmined := make([]*btcutil.Tx, 0, len(unconfirmed))
	records, err := s.unconfirmed.fetchConfirmedSpends()
	if err != nil {
		log.Errorf("Error fetching confirmed spends: %v", err)
		return nil
	}
	for _, r := range records {
		unmined = append(unmined, r.Tx())
	}
	spends, err := s.unconfirmed.fetchUnconfirmedSpends()
	if err != nil {
		log.Errorf("Error fetching unconfirmed spends: %v", err)
		return nil
	}
	for _, r := range spends {
		unmined = append(unmined, r.Tx())
	}
	return unmined
}

// removeDoubleSpends checks for any unconfirmed transactions which would
// introduce a double spend if tx was added to the store (either as a confirmed
// or unconfirmed transaction).  If one is found, it and all transactions which
// spends its outputs (if any) are removed, and all previous inputs for any
// removed transactions are set to unspent.
func (s *Store) removeDoubleSpends(tx *btcutil.Tx) error {
	if ds := s.unconfirmed.findDoubleSpend(tx); ds != nil {
		log.Debugf("Removing double spending transaction %v", ds.tx.Sha())
		return s.removeConflict(ds)
	}
	return nil
}

// removeConflict removes an unconfirmed transaction record and all spend chains
// deriving from it from the store.  This is designed to remove transactions
// that would otherwise result in double spend conflicts if left in the store.
// All not-removed credits spent by removed transactions are set unspent.
func (s *Store) removeConflict(r *txRecord) error {
	u := &s.unconfirmed

	// If this transaction contains any spent credits (which must be spent by
	// other unconfirmed transactions), recursively remove each spender.
	for i, credit := range r.credits {
		if credit == nil || credit.spentBy == nil {
			continue
		}
		op := wire.NewOutPoint(r.Tx().Sha(), uint32(i))
		nextSpender, err := u.fetchPrevOutPointSpender(op)
		if err != nil {
			return err
		}
		log.Debugf("Transaction %v is part of a removed double spend "+
			"chain -- removing as well", nextSpender.tx.Sha())
		if err := s.removeConflict(nextSpender); err != nil {
			return err
		}
	}

	// If this tx spends any previous credits, set each unspent.
	for _, input := range r.Tx().MsgTx().TxIn {
		if err := u.deletePrevOutPointSpender(&input.PreviousOutPoint); err != nil {
			return err
		}

		// For all mined transactions with credits spent by this
		// conflicting transaction, remove from the bookkeeping maps
		// and set each previous record's credit as unspent.
		prevKey, err := s.unconfirmed.lookupSpentBlockOutPointKey(&input.PreviousOutPoint)
		if err == nil {
			if err := s.unconfirmed.deleteSpentBlockOutpoint(&input.PreviousOutPoint, prevKey); err != nil {
				return err
			}
			prev, err := s.lookupBlockTx(prevKey.BlockTxKey)
			if err != nil {
				return err
			}
			prev.credits[prevKey.OutputIndex].spentBy = nil
			continue
		}

		// For all unmined transactions with credits spent by this
		// conflicting transaction, remove from the unspent store's
		// spent tracking.
		//
		// Spend tracking is only handled by these maps, so there is
		// no need to modify the record and unset a spent-by pointer.
		if _, err := u.fetchPrevOutPointSpender(&input.PreviousOutPoint); err == nil {
			if err := u.deleteOutPointSpender(&input.PreviousOutPoint); err != nil {
				return err
			}
		}
	}

	if err := u.deleteTxRecord(r.Tx()); err != nil {
		return err
	}

	return nil
}

// UnspentOutputs returns all unspent received transaction outputs.
// The order is undefined.
func (s *Store) UnspentOutputs() ([]Credit, error) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	return s.unspentOutputs()
}

func (s *Store) unspentOutputs() ([]Credit, error) {
	type createdCredit struct {
		credit Credit
		err    error
	}

	unspentOutpoints := make(map[*wire.OutPoint]*BlockTxKey)
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		unspentOutpoints, err = fetchUnspentOutpoints(wtx)
		return err
	})
	if err != nil {
		return []Credit{}, err
	}

	creditChans := make([]chan createdCredit, len(unspentOutpoints))
	i := 0

	for op, key := range unspentOutpoints {
		creditChans[i] = make(chan createdCredit)
		go func(i int, key BlockTxKey, opIndex uint32) {
			r, err := s.lookupBlockTx(key)
			if err != nil {
				creditChans[i] <- createdCredit{err: err}
				return
			}

			opKey := BlockOutputKey{key, opIndex}
			_, err = s.unconfirmed.fetchBlockOutPointSpender(&opKey)
			if err == nil {
				close(creditChans[i])
				return
			}

			t := &TxRecord{key, r, s}
			c := Credit{t, opIndex}
			creditChans[i] <- createdCredit{credit: c}
		}(i, *key, op.Index)
		i++
	}

	unspent := make([]Credit, 0, len(unspentOutpoints))
	for _, c := range creditChans {
		cc, ok := <-c
		if !ok {
			continue
		}
		if cc.err != nil {
			return nil, cc.err
		}
		unspent = append(unspent, cc.credit)
	}

	records, err := s.unconfirmed.records()
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		for outputIndex, credit := range r.credits {
			if credit == nil || credit.spentBy != nil {
				continue
			}
			key := BlockTxKey{BlockHeight: -1}
			txRecord := &TxRecord{key, r, s}
			c := Credit{txRecord, uint32(outputIndex)}
			op := c.outPoint()
			_, err := s.unconfirmed.fetchPrevOutPointSpender(op)
			if err != nil {
				unspent = append(unspent, c)
			}
		}
	}

	return unspent, nil
}

// lookupUnspentOutput fetches the unspent block tx key corresponding to the
// given unspent outpoint.
func (s *Store) lookupUnspentOutput(op *wire.OutPoint) (*BlockTxKey, error) {
	var key *BlockTxKey
	err := s.namespace.View(func(wtx walletdb.Tx) error {
		var err error
		key, err = fetchUnspent(wtx, op)
		return err
	})
	if err != nil {
		return nil, maybeConvertDbError(err)
	}
	return key, nil
}

// deleteUnspentOutput deletes the given unspent outpoint from the unspent
// bucket.
func (s *Store) deleteUnspentOutput(op *wire.OutPoint) error {
	return s.namespace.Update(func(wtx walletdb.Tx) error {
		return deleteUnspent(wtx, op)
	})
}

// unspentTx is a type defined here so it can be sorted with the sort
// package.  It is used to provide a sorted range over all transactions
// in a block with unspent outputs.
type unspentTx struct {
	blockIndex int
	sliceIndex uint32
}

type creditSlice []Credit

func (s creditSlice) Len() int {
	return len(s)
}

func (s creditSlice) Less(i, j int) bool {
	switch {
	// If both credits are from the same tx, sort by output index.
	case s[i].Tx().Sha() == s[j].Tx().Sha():
		return s[i].OutputIndex < s[j].OutputIndex

	// If both transactions are unmined, sort by their received date.
	case s[i].BlockIndex == -1 && s[j].BlockIndex == -1:
		return s[i].received.Before(s[j].received)

	// Unmined (newer) txs always come last.
	case s[i].BlockIndex == -1:
		return false
	case s[j].BlockIndex == -1:
		return true

	// If both txs are mined in different blocks, sort by block height.
	case s[i].BlockHeight != s[j].BlockHeight:
		return s[i].BlockHeight < s[j].BlockHeight

	// Both txs share the same block, sort by block index.
	default:
		return s[i].BlockIndex < s[j].BlockIndex
	}
}

func (s creditSlice) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// SortedUnspentOutputs returns all unspent recevied transaction outputs.
// The order is first unmined transactions (sorted by receive date), then
// mined transactions in increasing number of confirmations.  Transactions
// in the same block (same number of confirmations) are sorted by block
// index in increasing order.  Credits (outputs) from the same transaction
// are sorted by output index in increasing order.
func (s *Store) SortedUnspentOutputs() ([]Credit, error) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	unspent, err := s.unspentOutputs()
	if err != nil {
		return []Credit{}, err
	}
	sort.Sort(sort.Reverse(creditSlice(unspent)))
	return unspent, nil
}

// confirmed checks whether a transaction at height txHeight has met
// minconf confirmations for a blockchain at height curHeight.
func confirmed(minconf int, txHeight, curHeight int32) bool {
	return confirms(txHeight, curHeight) >= int32(minconf)
}

// confirms returns the number of confirmations for a transaction in a
// block at height txHeight (or -1 for an unconfirmed tx) given the chain
// height curHeight.
func confirms(txHeight, curHeight int32) int32 {
	switch {
	case txHeight == -1, txHeight > curHeight:
		return 0
	default:
		return curHeight - txHeight + 1
	}
}

// Balance returns the spendable wallet balance (total value of all unspent
// transaction outputs) given a minimum of minConf confirmations, calculated
// at a current chain height of curHeight.  Coinbase outputs are only included
// in the balance if maturity has been reached.
func (s *Store) Balance(minConf int, chainHeight int32) (btcutil.Amount, error) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	return s.balance(minConf, chainHeight)
}

func (s *Store) balance(minConf int, chainHeight int32) (btcutil.Amount, error) {
	var bal btcutil.Amount

	// Shadow these functions to avoid repeating arguments unnecesarily.
	confirms := func(height int32) int32 {
		return confirms(height, chainHeight)
	}
	confirmed := func(height int32) bool {
		return confirmed(minConf, height, chainHeight)
	}

	blocks, err := s.blocks()
	if err != nil {
		return 0, err
	}
	for _, b := range blocks {
		if confirmed(b.Height) {
			bal += b.amountDeltas.Spendable
			if confirms(b.Height) >= blockchain.CoinbaseMaturity {
				bal += b.amountDeltas.Reward
			}
			continue
		}
		// If there are still blocks that contain debiting transactions,
		// decrement the balance if they spend credits meeting minConf
		// confirmations.
		records, err := s.records(b.Height)
		if err != nil {
			return bal, err
		}
		for _, r := range records {
			if r.debits == nil {
				continue
			}
			for _, prev := range r.debits.spends {
				if !confirmed(prev.BlockHeight) {
					continue
				}
				r, err := s.lookupBlockTx(prev.BlockTxKey)
				if err != nil {
					return 0, err
				}
				v := r.Tx().MsgTx().TxOut[prev.OutputIndex].Value
				bal -= btcutil.Amount(v)
			}
		}
	}

	// Unconfirmed transactions which spend previous credits debit from
	// the returned balance, even with minConf > 0.
	ops, err := s.unconfirmed.fetchSpentBlockOutPoints()
	if err != nil {
		return 0, err
	}
	for _, prev := range ops {
		if confirmed(prev.BlockHeight) {
			r, err := s.lookupBlockTx(prev.BlockTxKey)
			if err != nil {
				return 0, err
			}
			v := r.Tx().MsgTx().TxOut[prev.OutputIndex].Value
			bal -= btcutil.Amount(v)
		}
	}

	// If unconfirmed credits are included, tally them as well.
	if minConf == 0 {
		records, err := s.unconfirmed.records()
		if err != nil {
			return 0, err
		}
		for _, r := range records {
			for i, c := range r.credits {
				if c == nil {
					continue
				}
				if c.spentBy == nil {
					v := r.Tx().MsgTx().TxOut[i].Value
					bal += btcutil.Amount(v)
				}
			}
		}
	}

	return bal, nil
}

// Records returns a chronologically-ordered slice of all transaction records
// saved by the store.  This is sorted first by block height in increasing
// order, and then by transaction index for each tx in a block.
func (s *Store) Records() (records []*TxRecord) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	blocks, err := s.blocks()
	if err != nil {
		log.Errorf("Error fetching records: %v", err)
	}
	for _, b := range blocks {
		rs, _ := s.records(b.Height)
		for _, r := range rs {
			key := BlockTxKey{r.tx.Index(), b.Height}
			records = append(records, &TxRecord{key, r, s})
		}
	}

	// Unconfirmed records are saved unsorted, and must be sorted by
	// received date on the fly.
	rs, err := s.unconfirmed.records()
	if err != nil {
		log.Errorf("Error fetching records: %v", err)
	}
	unconfirmed := make([]*TxRecord, 0, len(rs))
	for _, r := range rs {
		key := BlockTxKey{BlockHeight: -1}
		unconfirmed = append(unconfirmed, &TxRecord{key, r, s})
	}
	sort.Sort(byReceiveDate(unconfirmed))
	records = append(records, unconfirmed...)

	return
}

// Implementation of sort.Interface to sort transaction records by their
// receive date.
type byReceiveDate []*TxRecord

func (r byReceiveDate) Len() int           { return len(r) }
func (r byReceiveDate) Less(i, j int) bool { return r[i].received.Before(r[j].received) }
func (r byReceiveDate) Swap(i, j int)      { r[i], r[j] = r[j], r[i] }

// Debits returns the debit record for the transaction, or a non-nil error if
// the transaction does not debit from any previous transaction credits.
func (t *TxRecord) Debits() (Debits, error) {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	if t.debits == nil {
		return Debits{}, errors.New("no debits")
	}
	return Debits{t}, nil
}

// Credits returns all credit records for this transaction's outputs that are or
// were spendable by wallet.
func (t *TxRecord) Credits() []Credit {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	credits := make([]Credit, 0, len(t.credits))
	for i, c := range t.credits {
		if c != nil {
			credits = append(credits, Credit{t, uint32(i)})
		}
	}
	return credits
}

// HasCredit returns whether the transaction output at the passed index is
// a wallet credit.
func (t *TxRecord) HasCredit(i int) bool {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	if len(t.credits) <= i {
		return false
	}
	return t.credits[i] != nil
}

// InputAmount returns the total amount debited from previous credits.
func (d Debits) InputAmount() btcutil.Amount {
	d.s.mtx.RLock()
	defer d.s.mtx.RUnlock()

	return d.txRecord.debits.amount
}

// OutputAmount returns the total amount of all outputs for a transaction.
func (t *TxRecord) OutputAmount(ignoreChange bool) btcutil.Amount {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	return t.outputAmount(ignoreChange)
}

func (t *TxRecord) outputAmount(ignoreChange bool) btcutil.Amount {
	a := btcutil.Amount(0)
	for i, txOut := range t.Tx().MsgTx().TxOut {
		if ignoreChange {
			switch cs := t.credits; {
			case i < len(cs) && cs[i] != nil && cs[i].change:
				continue
			}
		}
		a += btcutil.Amount(txOut.Value)
	}
	return a
}

// Fee returns the difference between the debited amount and the total
// transaction output.
func (d Debits) Fee() btcutil.Amount {
	return d.txRecord.debits.amount - d.outputAmount(false)
}

// Addresses parses the pubkey script, extracting all addresses for a
// standard script.
func (c Credit) Addresses(net *chaincfg.Params) (txscript.ScriptClass,
	[]btcutil.Address, int, error) {

	c.s.mtx.RLock()
	defer c.s.mtx.RUnlock()

	msgTx := c.Tx().MsgTx()
	pkScript := msgTx.TxOut[c.OutputIndex].PkScript
	return txscript.ExtractPkScriptAddrs(pkScript, net)
}

// Change returns whether the credit is the result of a change output.
func (c Credit) Change() bool {
	c.s.mtx.RLock()
	defer c.s.mtx.RUnlock()

	return c.txRecord.credits[c.OutputIndex].change
}

// Confirmed returns whether a transaction has reached some target number of
// confirmations, given the current best chain height.
func (t *TxRecord) Confirmed(target int, chainHeight int32) bool {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	return confirmed(target, t.BlockHeight, chainHeight)
}

// Confirmations returns the total number of confirmations a transaction has
// reached, given the current best chain height.
func (t *TxRecord) Confirmations(chainHeight int32) int32 {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	return confirms(t.BlockHeight, chainHeight)
}

// IsCoinbase returns whether the transaction is a coinbase.
func (t *TxRecord) IsCoinbase() bool {
	t.s.mtx.RLock()
	defer t.s.mtx.RUnlock()

	return t.isCoinbase()
}

func (t *TxRecord) isCoinbase() bool {
	return t.BlockHeight != -1 && t.BlockIndex == 0
}

// Amount returns the amount credited to the account from a transaction output.
func (c Credit) Amount() btcutil.Amount {
	c.s.mtx.RLock()
	defer c.s.mtx.RUnlock()

	return c.amount()
}

func (c Credit) amount() btcutil.Amount {
	msgTx := c.Tx().MsgTx()
	return btcutil.Amount(msgTx.TxOut[c.OutputIndex].Value)
}

// OutPoint returns the outpoint needed to include in a transaction input
// to spend this output.
func (c Credit) OutPoint() *wire.OutPoint {
	c.s.mtx.RLock()
	defer c.s.mtx.RUnlock()

	return c.outPoint()
}

func (c Credit) outPoint() *wire.OutPoint {
	return wire.NewOutPoint(c.Tx().Sha(), c.OutputIndex)
}

// outputKey creates and returns the block lookup key for this credit.
func (c Credit) outputKey() BlockOutputKey {
	return BlockOutputKey{
		BlockTxKey:  c.BlockTxKey,
		OutputIndex: c.OutputIndex,
	}
}

// Spent returns whether the transaction output is currently spent or not.
func (c Credit) Spent() bool {
	c.s.mtx.RLock()
	defer c.s.mtx.RUnlock()

	return c.txRecord.credits[c.OutputIndex].spentBy != nil
}

// TxOut returns the transaction output which this credit references.
func (c Credit) TxOut() *wire.TxOut {
	c.s.mtx.RLock()
	defer c.s.mtx.RUnlock()

	return c.Tx().MsgTx().TxOut[c.OutputIndex]
}

// Tx returns the underlying transaction.
func (r *txRecord) Tx() *btcutil.Tx {
	return r.tx
}

// isTxHashNotFoundErr returns whether or not the passed error is due to a
// missing tx record
func isTxHashNotFoundErr(err error) bool {
	merr, ok := err.(TxStoreError)
	return ok && merr.ErrorCode == ErrTxHashNotFound
}

// isMissingBlockErr returns whether or not the passed error is due to a
// missing block
func isMissingBlockErr(err error) bool {
	merr, ok := err.(TxStoreError)
	return ok && merr.ErrorCode == ErrMissingBlock
}

// isDuplicateInsertErr returns whether or not the passed error is due to a
// duplicate insert
func isDuplicateInsertErr(err error) bool {
	merr, ok := err.(TxStoreError)
	return ok && merr.ErrorCode == ErrDuplicateInsert
}

func Open(namespace walletdb.Namespace, net *chaincfg.Params) (*Store, error) {
	// Upgrade the manager to the latest version as needed.  This includes
	// the initial creation.
	if err := upgradeManager(namespace); err != nil {
		return nil, err
	}

	return &Store{
		namespace: namespace,
		unconfirmed: unconfirmedStore{
			namespace: namespace,
		},
		notificationLock: new(sync.Mutex),
	}, nil
}