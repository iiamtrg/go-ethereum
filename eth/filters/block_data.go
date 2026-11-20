// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package filters

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// BlockDataQuery selects what a blockData notification carries besides the block's header. A log
// is selected when any of the three log criteria selects it; the logs come once each, in block
// order, as eth_getLogs by the block's hash would return the union of the matching queries.
type BlockDataQuery struct {
	// Topics selects every log whose topic 0 is one of these.
	Topics []common.Hash `json:"topics"`
	// Addresses selects every log emitted by one of these contracts.
	Addresses []common.Address `json:"addresses"`
	// Participants selects a log whose topic 0 is a participant's topic and whose topic 1 or
	// topic 2 is that participant's address, left-padded to 32 bytes.
	Participants []BlockDataParticipant `json:"participants"`
	// TransactionsTo selects every transaction sent to one of these addresses that succeeded
	// (receipt status 1), as eth_getBlockByHash(hash, true) serves it. A reverted transaction wrote
	// nothing, so it is left out: a transaction pushed is one whose writes happened.
	TransactionsTo []common.Address `json:"transactionsTo"`
}

// BlockDataParticipant is one BlockDataQuery participant.
type BlockDataParticipant struct {
	Topic   common.Hash    `json:"topic"`
	Address common.Address `json:"address"`
}

// BlockData is a block appended to the chain with the logs and transactions a BlockDataQuery
// selected, in block order.
type BlockData struct {
	Header       *types.Header
	Logs         []*types.Log
	Transactions []*types.Transaction
	TxIndexes    []uint64 // each of Transactions' position in the block
}

// blockDataCrit is a BlockDataQuery as sets, for the event loop to match each log and
// transaction of a block against.
type blockDataCrit struct {
	topics       map[common.Hash]struct{}
	addresses    map[common.Address]struct{}
	participants map[common.Hash]map[common.Hash]struct{} // topic 0 -> padded addresses
	txTo         map[common.Address]struct{}
}

func newBlockDataCrit(q BlockDataQuery) *blockDataCrit {
	c := &blockDataCrit{
		topics:       make(map[common.Hash]struct{}, len(q.Topics)),
		addresses:    make(map[common.Address]struct{}, len(q.Addresses)),
		participants: make(map[common.Hash]map[common.Hash]struct{}),
		txTo:         make(map[common.Address]struct{}, len(q.TransactionsTo)),
	}
	for _, t := range q.Topics {
		c.topics[t] = struct{}{}
	}
	for _, a := range q.Addresses {
		c.addresses[a] = struct{}{}
	}
	for _, p := range q.Participants {
		if c.participants[p.Topic] == nil {
			c.participants[p.Topic] = make(map[common.Hash]struct{})
		}
		c.participants[p.Topic][common.BytesToHash(p.Address.Bytes())] = struct{}{}
	}
	for _, a := range q.TransactionsTo {
		c.txTo[a] = struct{}{}
	}
	return c
}

// selects reports whether any of the query's log criteria selects l.
func (c *blockDataCrit) selects(l *types.Log) bool {
	if _, ok := c.addresses[l.Address]; ok {
		return true
	}
	if len(l.Topics) == 0 {
		return false
	}
	if _, ok := c.topics[l.Topics[0]]; ok {
		return true
	}
	if padded, ok := c.participants[l.Topics[0]]; ok {
		for _, t := range l.Topics[1:min(3, len(l.Topics))] {
			if _, ok := padded[t]; ok {
				return true
			}
		}
	}
	return false
}

// collect is the block of ev with what the query selects from it.
func (c *blockDataCrit) collect(ev core.ChainEvent) *BlockData {
	b := &BlockData{Header: ev.Header, Logs: []*types.Log{}}
	for _, receipt := range ev.Receipts {
		for _, l := range receipt.Logs {
			if c.selects(l) {
				b.Logs = append(b.Logs, l)
			}
		}
	}
	if len(c.txTo) > 0 {
		for i, tx := range ev.Transactions {
			if to := tx.To(); to != nil && ev.Receipts[i].Status == types.ReceiptStatusSuccessful {
				if _, ok := c.txTo[*to]; ok {
					b.Transactions = append(b.Transactions, tx)
					b.TxIndexes = append(b.TxIndexes, uint64(i))
				}
			}
		}
	}
	return b
}

// blockDataJSON is a blockData notification.
type blockDataJSON struct {
	Header       map[string]interface{}   `json:"header"`
	Logs         []*types.Log             `json:"logs"`
	Transactions []*ethapi.RPCTransaction `json:"transactions"`
}

// marshal is b as a notification: the header and the transactions as eth_getBlockByHash serves
// them, the logs as eth_getLogs does.
func (b *BlockData) marshal(config *params.ChainConfig) *blockDataJSON {
	hash := b.Header.Hash()
	txs := make([]*ethapi.RPCTransaction, len(b.Transactions))
	for i, tx := range b.Transactions {
		txs[i] = ethapi.NewRPCTransactionInBlock(tx, b.Header, hash, b.TxIndexes[i], config)
	}
	return &blockDataJSON{
		Header:       ethapi.RPCMarshalArbitrumHeader(b.Header, config),
		Logs:         b.Logs,
		Transactions: txs,
	}
}

// BlockData sends a notification for every block appended to the chain: its header, the logs
// `query` selects and the transactions it selects, from the chain event itself, so with no
// further read of the block.
func (api *FilterAPI) BlockData(ctx context.Context, query BlockDataQuery) (*rpc.Subscription, error) {
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}
	blocks := make(chan *BlockData)
	sub, err := api.events.SubscribeBlockData(query, blocks)
	if err != nil {
		return nil, err
	}
	rpcSub := notifier.CreateSubscription()
	config := api.sys.backend.ChainConfig()

	go func() {
		defer sub.Unsubscribe()
		for {
			select {
			case b := <-blocks:
				notifier.Notify(rpcSub.ID, b.marshal(config))
			case <-rpcSub.Err():
				return
			}
		}
	}()

	return rpcSub, nil
}
