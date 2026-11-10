package core

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// A written block is served from the caches exactly as the database would serve it: the same
// block and body encoding, the same senders, and the same logs (stored fields only).
func TestWrittenBlockCacheMatchesDatabase(t *testing.T) {
	var (
		key, _ = crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
		addr   = crypto.PubkeyToAddress(key.PublicKey)
		gspec  = &Genesis{Config: params.TestChainConfig, Alloc: types.GenesisAlloc{addr: {Balance: big.NewInt(10000000000000000)}}}
		signer = types.LatestSigner(gspec.Config)
		// Init code that emits LOG0 with no data: PUSH1 0, PUSH1 0, LOG0, STOP.
		log0Code = common.Hex2Bytes("60006000a000")
	)
	db := rawdb.NewMemoryDatabase()
	chain, err := NewBlockChain(db, nil, gspec, ethash.NewFaker(), DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer chain.Stop()

	_, blocks, _ := GenerateChainWithGenesis(gspec, ethash.NewFaker(), 4, func(i int, gen *BlockGen) {
		add := func(tx *types.Transaction) {
			signed, err := types.SignTx(tx, signer, key)
			if err != nil {
				t.Fatal(err)
			}
			gen.AddTx(signed)
		}
		switch i {
		case 0: // no transactions
		case 1: // a LOG1 with data, then a transfer with no logs
			add(types.NewContractCreation(gen.TxNonce(addr), new(big.Int), 1000000, gen.header.BaseFee, logCode))
			add(types.NewTransaction(gen.TxNonce(addr), common.Address{1}, big.NewInt(1), 21000, gen.header.BaseFee, nil))
		case 2: // a LOG0 with no topics and no data
			add(types.NewContractCreation(gen.TxNonce(addr), new(big.Int), 1000000, gen.header.BaseFee, log0Code))
		case 3: // both kinds in one block
			add(types.NewContractCreation(gen.TxNonce(addr), new(big.Int), 1000000, gen.header.BaseFee, log0Code))
			add(types.NewContractCreation(gen.TxNonce(addr), new(big.Int), 1000000, gen.header.BaseFee, logCode))
		}
	})
	if _, err := chain.InsertChain(blocks); err != nil {
		t.Fatal(err)
	}

	sawLogs := 0
	for _, block := range blocks {
		hash, number := block.Hash(), block.NumberU64()

		cached, ok := chain.blockCache.Get(hash)
		if !ok {
			t.Fatalf("block %d not cached on write", number)
		}
		stored := rawdb.ReadBlock(db, hash, number)
		if !bytes.Equal(mustRLP(t, cached), mustRLP(t, stored)) {
			t.Errorf("block %d: cached encoding differs from the database's", number)
		}
		if (cached.Withdrawals() == nil) != (stored.Withdrawals() == nil) {
			t.Errorf("block %d: withdrawals nil-ness differs", number)
		}
		if cached.Size() != stored.Size() {
			t.Errorf("block %d: size %d, database %d", number, cached.Size(), stored.Size())
		}
		for i, tx := range cached.Transactions() {
			a, errA := types.Sender(signer, tx)
			b, errB := types.Sender(signer, stored.Transactions()[i])
			if a != b || errA != nil || errB != nil {
				t.Errorf("block %d tx %d: sender %v (%v), database %v (%v)", number, i, a, errA, b, errB)
			}
		}

		body, ok := chain.bodyCache.Get(hash)
		if !ok {
			t.Fatalf("block %d body not cached on write", number)
		}
		if !bytes.Equal(mustRLP(t, body), mustRLP(t, rawdb.ReadBody(db, hash, number))) {
			t.Errorf("block %d: cached body encoding differs from the database's", number)
		}

		logs, ok := chain.CachedLogs(hash)
		if !ok {
			t.Fatalf("block %d logs not cached on write", number)
		}
		storedLogs := rawdb.ReadLogs(db, hash, number)
		if got, want := mustJSON(t, logs), mustJSON(t, storedLogs); got != want {
			t.Errorf("block %d logs:\n cached   %s\n database %s", number, got, want)
		}
		for _, txLogs := range logs {
			sawLogs += len(txLogs)
		}

		// Readers fill derived fields in place: a second read must not see the first's writes.
		for _, txLogs := range logs {
			for _, l := range txLogs {
				l.BlockNumber, l.TxIndex, l.Index = 99, 99, 99
			}
		}
		again, _ := chain.CachedLogs(hash)
		if got, want := mustJSON(t, again), mustJSON(t, storedLogs); got != want {
			t.Errorf("block %d: a reader's writes leaked into the cache", number)
		}
	}
	if sawLogs != 4 {
		t.Fatalf("expected 4 logs across the chain, saw %d", sawLogs)
	}
	if _, ok := chain.CachedLogs(common.Hash{1}); ok {
		t.Fatal("an unknown block reported cached logs")
	}
}

func mustRLP(t *testing.T, v any) []byte {
	t.Helper()
	enc, err := rlp.EncodeToBytes(v)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	enc, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(enc)
}
