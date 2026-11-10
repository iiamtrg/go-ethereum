// Copyright 2021 The go-ethereum Authors
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

// Package core implements the Ethereum consensus protocol.
package core

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
)

func (bc *BlockChain) generateGcprocRandOffset() time.Duration {
	if bc.cfg.TrieTimeLimitRandomOffset > 0 {
		return time.Duration(rand.Int64N(int64(bc.cfg.TrieTimeLimitRandomOffset)))
	}
	return 0
}

func (bc *BlockChain) FlushTrieDB(capLimit common.StorageSize) error {
	if bc.triedb.Scheme() == rawdb.PathScheme {
		return nil
	}

	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	if !bc.triegc.Empty() {
		_, triegcBlockNumber := bc.triegc.Peek()
		blockNumber := uint64(-triegcBlockNumber)

		header := bc.GetHeaderByNumber(blockNumber)
		if header == nil {
			log.Warn("Reorg in progress, trie commit postponed")
		} else {
			err := bc.triedb.Commit(header.Root, true)
			if err != nil {
				return err
			}

			bc.gcproc = 0
			bc.gcprocRandOffset = bc.generateGcprocRandOffset()
			bc.lastWrite = blockNumber
		}
	}

	err := bc.triedb.Cap(capLimit)
	if err != nil {
		return err
	}

	return nil
}

// WriteBlockAndSetHeadWithTime also counts processTime, which will cause intermittent TrieDirty cache writes
func (bc *BlockChain) WriteBlockAndSetHeadWithTime(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool, processTime time.Duration) (status WriteStatus, err error) {
	if !bc.chainmu.TryLock() {
		return NonStatTy, errChainStopped
	}
	defer bc.chainmu.Unlock()
	bc.gcproc += processTime
	return bc.writeBlockAndSetHead(block, receipts, logs, state, emitHeadEvent)
}

func (bc *BlockChain) ReorgToOldBlock(newHead *types.Block) error {
	if _, err := bc.SetCanonical(newHead); err != nil {
		return fmt.Errorf("error reorging to old block: %w", err)
	}
	return nil
}

func (bc *BlockChain) ClipToPostNitroGenesis(blockNum rpc.BlockNumber) (rpc.BlockNumber, rpc.BlockNumber) {
	currentBlock := rpc.BlockNumber(bc.CurrentBlock().Number.Uint64())
	nitroGenesis := rpc.BlockNumber(bc.Config().ArbitrumChainParams.GenesisBlockNum)
	if blockNum == rpc.LatestBlockNumber || blockNum == rpc.PendingBlockNumber {
		blockNum = currentBlock
	}
	if blockNum > currentBlock {
		blockNum = currentBlock
	}
	if blockNum < nitroGenesis {
		blockNum = nitroGenesis
	}
	return blockNum, currentBlock
}

func (bc *BlockChain) RecoverState(ctx context.Context, block *types.Block) error {
	if bc.HasState(block.Root()) {
		return nil
	}
	log.Warn("recovering block state", "num", block.Number(), "hash", block.Hash(), "root", block.Root())
	_, err := bc.recoverAncestors(ctx, block, false)
	return err
}

// writtenLogsCacheLimit keeps a written block's logs as long as the block and its body are kept, so
// a reader that has fallen behind the head (a burst after a sequencer stall) is served all three
// from memory, never the block from the cache and its logs from the database.
const writtenLogsCacheLimit = blockCacheLimit

// cacheWrittenBlock puts a block that has just been written and made head into the caches its
// first readers go through, so they are served from memory instead of the database:
//
//   - the block itself (GetBlock, eth_getBlockByHash/Number). It is the executed object, whose
//     transactions already hold their hashes and recovered senders, where a database read would
//     decode the body again and recover every sender for full transaction objects;
//   - its body (GetBody), which eth_getLogs reads to fill in the transaction hashes of matched logs;
//   - its logs (CachedLogs, eth_getLogs).
//
// All three are keyed by block hash, whose content never changes, so a reorg cannot make an entry
// wrong; SetHead purges them with the other caches.
func (bc *BlockChain) cacheWrittenBlock(block *types.Block, receipts []*types.Receipt) {
	hash := block.Hash()
	bc.blockCache.Add(hash, block)
	bc.bodyCache.Add(hash, block.Body())
	logs := make([][]*types.Log, len(receipts))
	for i, receipt := range receipts {
		logs[i] = storedLogFields(receipt.Logs)
	}
	bc.logsCache.Add(hash, logs)
}

// CachedLogs returns a written block's logs as rawdb.ReadLogs would read them back: per
// transaction, with only the stored fields (address, topics, data) set. Each call returns new Log
// values, since callers fill in the derived fields in place. ok is false when the block is not
// cached.
func (bc *BlockChain) CachedLogs(hash common.Hash) (logs [][]*types.Log, ok bool) {
	cached, ok := bc.logsCache.Get(hash)
	if !ok {
		return nil, false
	}
	logs = make([][]*types.Log, len(cached))
	for i, txLogs := range cached {
		logs[i] = storedLogFields(txLogs)
	}
	return logs, true
}

// storedLogFields copies the fields of each log that a receipt stores; the rest are derived on
// read. Topics and data are shared, as neither the writer nor any reader changes them.
func storedLogFields(logs []*types.Log) []*types.Log {
	out := make([]*types.Log, len(logs))
	for i, l := range logs {
		out[i] = &types.Log{Address: l.Address, Topics: l.Topics, Data: l.Data}
	}
	return out
}
