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
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	bdTopic       = common.HexToHash("0x01")             // selected by topic 0
	bdOtherTopic  = common.HexToHash("0x09")             // selected by nothing
	bdPartTopic   = common.HexToHash("0x02")             // a participant's topic
	bdParticipant = common.HexToAddress("0xee")          // a participant's address
	bdAnon        = common.HexToAddress("0xaa")          // selected by address
	bdTarget      = common.HexToAddress("0x77")          // transactions to it are selected
	bdPadded      = common.BytesToHash(bdParticipant[:]) // the participant in a topic
	bdQuery       = BlockDataQuery{
		Topics:         []common.Hash{bdTopic},
		Addresses:      []common.Address{bdAnon},
		Participants:   []BlockDataParticipant{{Topic: bdPartTopic, Address: bdParticipant}},
		TransactionsTo: []common.Address{bdTarget},
	}
)

func bdLog(address common.Address, topics ...common.Hash) *types.Log {
	return &types.Log{Address: address, Topics: topics}
}

// blockDataEvent is a block of four transactions: to the target, to another address, a contract
// creation, and to the target again. It returns the logs and transaction indexes the query selects.
func blockDataEvent() (core.ChainEvent, []*types.Log, []uint64) {
	other := common.HexToAddress("0x55")
	selected := []*types.Log{
		bdLog(other, bdTopic),                             // topic 0
		bdLog(bdAnon),                                     // address, anonymous
		bdLog(other, bdPartTopic, bdPadded),               // participant in topic 1
		bdLog(other, bdPartTopic, bdOtherTopic, bdPadded), // participant in topic 2
		bdLog(other, bdTopic, bdPadded),                   // topic 0 again, last tx
	}
	receipts := []*types.Receipt{
		{Logs: []*types.Log{selected[0], bdLog(other, bdOtherTopic)}},
		{Logs: []*types.Log{
			selected[1],
			selected[2],
			selected[3],
			bdLog(other, bdPartTopic, bdOtherTopic, bdOtherTopic, bdPadded), // participant in topic 3
			bdLog(other, bdOtherTopic, bdPadded),                            // participant under another topic
			bdLog(other),                                                    // anonymous, other address
		}},
		{},
		{Logs: []*types.Log{selected[4]}},
	}
	tx := func(nonce uint64, to *common.Address) *types.Transaction {
		return types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1), Gas: 21000, To: to})
	}
	txs := []*types.Transaction{tx(0, &bdTarget), tx(1, &other), tx(2, nil), tx(3, &bdTarget)}
	header := &types.Header{Number: big.NewInt(5), Difficulty: big.NewInt(1), BaseFee: big.NewInt(1)}
	return core.ChainEvent{Header: header, Receipts: receipts, Transactions: txs}, selected, []uint64{0, 3}
}

// A blockData subscription delivers the event's header, the logs any criterion selects, once each
// and in block order, and the transactions sent to the declared addresses with their indexes.
func TestBlockDataSelectsInBlockOrder(t *testing.T) {
	t.Parallel()
	backend, sys := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
	api := NewFilterAPI(sys)
	blocks := make(chan *BlockData)
	sub, err := api.events.SubscribeBlockData(bdQuery, blocks)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	ev, wantLogs, wantIndexes := blockDataEvent()
	backend.chainFeed.Send(ev)
	var got *BlockData
	select {
	case got = <-blocks:
	case <-time.After(time.Second):
		t.Fatal("no blockData for the chain event")
	}
	if got.Header != ev.Header {
		t.Error("the header is not the event's")
	}
	if len(got.Logs) != len(wantLogs) {
		t.Fatalf("got %d logs, want %d", len(got.Logs), len(wantLogs))
	}
	for i := range wantLogs {
		if got.Logs[i] != wantLogs[i] {
			t.Errorf("log %d: got %+v, want %+v", i, got.Logs[i], wantLogs[i])
		}
	}
	if len(got.Transactions) != len(wantIndexes) {
		t.Fatalf("got %d transactions, want %d", len(got.Transactions), len(wantIndexes))
	}
	for i, index := range wantIndexes {
		if got.TxIndexes[i] != index || got.Transactions[i] != ev.Transactions[index] {
			t.Errorf("transaction %d: got index %d, want %d", i, got.TxIndexes[i], index)
		}
	}

	// A block nothing is selected from still delivers its header.
	empty := core.ChainEvent{Header: &types.Header{Number: big.NewInt(6)}}
	backend.chainFeed.Send(empty)
	select {
	case got = <-blocks:
	case <-time.After(time.Second):
		t.Fatal("no blockData for a block with nothing selected")
	}
	if got.Header != empty.Header || len(got.Logs) != 0 || len(got.Transactions) != 0 {
		t.Errorf("got %+v for an empty block", got)
	}
}

// The subscription is served as eth_subscribe("blockData", query), one notification per block
// carrying header, logs and transactions.
func TestBlockDataOverRPC(t *testing.T) {
	t.Parallel()
	backend, sys := newTestFilterSystem(rawdb.NewMemoryDatabase(), Config{})
	server := rpc.NewServer()
	defer server.Stop()
	if err := server.RegisterName("eth", NewFilterAPI(sys)); err != nil {
		t.Fatal(err)
	}
	client := rpc.DialInProc(server)
	defer client.Close()

	notifications := make(chan json.RawMessage)
	sub, err := client.EthSubscribe(context.Background(), notifications, "blockData", bdQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()

	ev, wantLogs, wantIndexes := blockDataEvent()
	backend.chainFeed.Send(ev)
	var raw json.RawMessage
	select {
	case raw = <-notifications:
	case err := <-sub.Err():
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("no notification for the chain event")
	}
	var got struct {
		Header struct {
			Hash   common.Hash `json:"hash"`
			Number string      `json:"number"`
		} `json:"header"`
		Logs         []json.RawMessage `json:"logs"`
		Transactions []struct {
			Hash             common.Hash `json:"hash"`
			TransactionIndex string      `json:"transactionIndex"`
			BlockHash        common.Hash `json:"blockHash"`
		} `json:"transactions"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	if got.Header.Hash != ev.Header.Hash() || got.Header.Number != "0x5" {
		t.Errorf("header: got %+v", got.Header)
	}
	if len(got.Logs) != len(wantLogs) {
		t.Errorf("got %d logs, want %d", len(got.Logs), len(wantLogs))
	}
	if len(got.Transactions) != len(wantIndexes) {
		t.Fatalf("got %d transactions, want %d", len(got.Transactions), len(wantIndexes))
	}
	for i, index := range wantIndexes {
		tx := got.Transactions[i]
		if tx.Hash != ev.Transactions[index].Hash() || tx.BlockHash != ev.Header.Hash() ||
			tx.TransactionIndex != (map[uint64]string{0: "0x0", 3: "0x3"})[index] {
			t.Errorf("transaction %d: got %+v", i, tx)
		}
	}
}
