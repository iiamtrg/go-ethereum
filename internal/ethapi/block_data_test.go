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

package ethapi

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/internal/blocktest"
	"github.com/ethereum/go-ethereum/params"
)

// The blockData subscription's header and transactions are field for field what
// eth_getBlockByHash(hash, true) serves for the same Arbitrum block.
func TestBlockDataPartsMatchGetBlockByHash(t *testing.T) {
	t.Parallel()
	config := *params.TestChainConfig
	config.ArbitrumChainParams = params.ArbitrumChainParams{EnableArbOS: true}

	header := &types.Header{
		Number:     big.NewInt(100),
		Difficulty: big.NewInt(1),
		BaseFee:    big.NewInt(10_000_000),
		Time:       1_700_000_000,
		GasLimit:   1 << 50,
	}
	types.HeaderInfo{
		SendRoot:           common.HexToHash("0x1234"),
		SendCount:          7,
		L1BlockNumber:      24_000_000,
		ArbOSFormatVersion: params.ArbosVersion_40,
	}.UpdateHeaderWithInfo(header)

	key, _ := crypto.GenerateKey()
	signer := types.LatestSigner(&config)
	to := common.HexToAddress("0x11")
	var txs []*types.Transaction
	for i := uint64(0); i < 3; i++ {
		var inner types.TxData = &types.DynamicFeeTx{
			ChainID: config.ChainID, Nonce: i, GasTipCap: big.NewInt(0), GasFeeCap: big.NewInt(20_000_000),
			Gas: 100_000, To: &to, Value: big.NewInt(0), Data: []byte{0xab, byte(i)},
		}
		if i == 1 {
			inner = &types.LegacyTx{Nonce: i, GasPrice: big.NewInt(20_000_000), Gas: 100_000, To: &to, Data: []byte{0xcd}}
		}
		txs = append(txs, types.MustSignNewTx(key, signer, inner))
	}
	block := types.NewBlock(header, &types.Body{Transactions: txs}, nil, blocktest.NewHasher())

	want := RPCMarshalBlock(block, true, true, &config)
	if _, ok := want["l1BlockNumber"]; !ok {
		t.Fatal("the reference block carries no Arbitrum fields: the test does not exercise them")
	}
	toJSON := func(v interface{}) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	got := RPCMarshalArbitrumHeader(block.Header(), &config)
	for _, bodyOnly := range []string{"size", "transactions", "uncles", "withdrawals"} {
		delete(want, bodyOnly)
	}
	wantTxs := RPCMarshalBlock(block, true, true, &config)["transactions"].([]interface{})
	if toJSON(got) != toJSON(want) {
		t.Errorf("header differs from eth_getBlockByHash\n got %s\nwant %s", toJSON(got), toJSON(want))
	}
	for i, tx := range block.Transactions() {
		got := NewRPCTransactionInBlock(tx, block.Header(), block.Hash(), uint64(i), &config)
		if toJSON(got) != toJSON(wantTxs[i]) {
			t.Errorf("transaction %d differs from eth_getBlockByHash\n got %s\nwant %s", i, toJSON(got), toJSON(wantTxs[i]))
		}
	}
}
