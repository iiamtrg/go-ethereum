// Copyright 2026, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE.md

package arbitrum

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/arbitrum_types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

type jumpDestTestArb struct{ bc *core.BlockChain }

func (a jumpDestTestArb) PublishTransaction(context.Context, *types.Transaction, *arbitrum_types.ConditionalOptions) error {
	return nil
}
func (a jumpDestTestArb) BlockChain() *core.BlockChain { return a.bc }
func (a jumpDestTestArb) ArbNode() interface{}         { return nil }

// jumpCode jumps to dest. Byte 4 is a JUMPDEST byte inside PUSH1 data, so only dest 5
// is a valid destination.
//
//	0: PUSH1 dest  2: JUMP  3: PUSH1 0x5b  5: JUMPDEST  6: STOP
func jumpCode(dest byte) []byte {
	return []byte{byte(vm.PUSH1), dest, byte(vm.JUMP), byte(vm.PUSH1), byte(vm.JUMPDEST), byte(vm.JUMPDEST), byte(vm.STOP)}
}

// The EVM of a call shares the BlockChain's JUMPDEST cache, and replacing an account's
// code, as a state override does, gets the analysis of the new code rather than the
// cached one of the old.
func TestGetEVMSharesJumpDestCache(t *testing.T) {
	gspec := &core.Genesis{Config: params.TestChainConfig, BaseFee: big.NewInt(params.InitialBaseFee)}
	bc, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), nil, gspec, ethash.NewFaker(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Stop()
	backend := &APIBackend{b: &Backend{arb: jumpDestTestArb{bc}}}

	statedb, err := state.New(types.EmptyRootHash, state.NewDatabaseForTesting())
	if err != nil {
		t.Fatal(err)
	}
	addr := common.Address{0x10}
	blockCtx := vm.BlockContext{
		CanTransfer: func(vm.StateDB, common.Address, *uint256.Int) bool { return true },
		Transfer:    func(vm.StateDB, common.Address, common.Address, *uint256.Int, *params.Rules) {},
		BlockNumber: common.Big0,
	}
	call := func() error {
		evm := backend.GetEVM(context.Background(), statedb, bc.CurrentHeader(), &vm.Config{}, &blockCtx)
		if evm.JumpDests() != bc.JumpDestCache() {
			t.Fatal("EVM does not use the BlockChain's JUMPDEST cache")
		}
		_, _, _, err := evm.Call(common.Address{}, addr, nil, vm.NewGasBudget(100_000, 0), new(uint256.Int))
		return err
	}

	valid, intoPushData := jumpCode(5), jumpCode(4)
	statedb.SetCode(addr, valid, tracing.CodeChangeUnspecified)
	if err := call(); err != nil {
		t.Fatalf("valid jump: %v", err)
	}
	if _, ok := bc.JumpDestCache().Load(crypto.Keccak256Hash(valid)); !ok {
		t.Fatal("analysis of the called code not kept in the BlockChain's cache")
	}
	statedb.SetCode(addr, intoPushData, tracing.CodeChangeUnspecified)
	if err := call(); !errors.Is(err, vm.ErrInvalidJump) {
		t.Fatalf("jump into push data after replacing the code: got %v, want %v", err, vm.ErrInvalidJump)
	}
}
