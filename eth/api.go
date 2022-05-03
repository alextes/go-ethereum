// Copyright 2015 The go-ethereum Authors
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

package eth

import (
	"context"
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/issuance"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"math/big"
)

// EthereumAPI provides an API to access Ethereum full node-related information.
type EthereumAPI struct {
	e *Ethereum
}

// NewEthereumAPI creates a new Ethereum protocol API for full nodes.
func NewEthereumAPI(e *Ethereum) *EthereumAPI {
	return &EthereumAPI{e}
}

// Etherbase is the address that mining rewards will be sent to.
func (api *EthereumAPI) Etherbase() (common.Address, error) {
	return api.e.Etherbase()
}

// Coinbase is the address that mining rewards will be sent to (alias for Etherbase).
func (api *EthereumAPI) Coinbase() (common.Address, error) {
	return api.Etherbase()
}

// Hashrate returns the POW hashrate.
func (api *EthereumAPI) Hashrate() hexutil.Uint64 {
	return hexutil.Uint64(api.e.Miner().Hashrate())
}

// Issuance send a notification each time a new block is appended to the chain
// with various counters about Ether issuance: the state diff (if available),
// block and uncle subsidy, 1559 burn.
func (api *EthereumAPI) Issuance(ctx context.Context, from uint64) (*rpc.Subscription, error) {
	// If issuance tracking is not explcitly enabled, refuse to service this
	// endpoint. Although we could enable the simple calculations, it might
	// end up as an unexpected load on RPC providers, so let's not surprise.
	if !api.e.config.EnableIssuanceRecording {
		return nil, errors.New("issuance recording not enabled")
	}
	config := api.e.blockchain.Config()

	// Issuance recording enabled, create a subscription to stream through
	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}
	rpcSub := notifier.CreateSubscription()

	// Define an internal type for issuance notifications
	type issuanceNotification struct {
		Number   uint64      `json:"blockNumber"`
		Hash     common.Hash `json:"blockHash"`
		Issuance *big.Int    `json:"totalIssuance"`
		Subsidy  *big.Int    `json:"subsidyMining"`
		Uncles   *big.Int    `json:"subsidyUncles"`
		Burn     *big.Int    `json:"burnProtocol"`
		Destruct *big.Int    `json:"burnDestruct"`
	}

	// Define a method to convert a block into an issuance notification
	service := func(block *types.Block) {
		// Retrieve the state-crawled issuance - if available
		crawled := rawdb.ReadIssuance(api.e.chainDb, block.NumberU64(), block.Hash())

		// Calculate the subsidy from the block's contents
		subsidy, uncles, burn := issuance.Subsidy(block, config)

		// Calculate the difference between the "calculated" and "crawled" issuance
		var diff *big.Int
		if crawled != nil {
			diff = new(big.Int).Set(crawled)
			diff.Sub(diff, subsidy)
			diff.Sub(diff, uncles)
			diff.Add(diff, burn)
		}
		// Push the issuance to the user
		notifier.Notify(rpcSub.ID, &issuanceNotification{
			Number:   block.NumberU64(),
			Hash:     block.Hash(),
			Issuance: crawled,
			Subsidy:  subsidy,
			Uncles:   uncles,
			Burn:     burn,
			Destruct: diff,
		})
	}
	go func() {
		// Iterate over all blocks from the requested source up to head and push
		// out historical issuance values to the user. Checking the head after
		// each iteration is a bit heavy, but it's not really relevant compared
		// to pulling blocks from disk, so this keeps thing simpler to switch
		// from historicla blocks to live blocks.
		for number := from; number <= api.e.blockchain.CurrentBlock().Number.Uint64(); number++ {
			block := rawdb.ReadBlock(api.e.chainDb, rawdb.ReadCanonicalHash(api.e.chainDb, number), number)
			if block == nil {
				log.Error("Missing block for issuane reporting", "number", number)
				return
			}
			service(block)
		}
		// Subscribe to chain events and keep emitting issuances on all branches
		canonBlocks := make(chan core.ChainEvent)
		canonBlocksSub := api.e.blockchain.SubscribeChainEvent(canonBlocks)
		defer canonBlocksSub.Unsubscribe()

		sideBlocks := make(chan core.ChainSideEvent)
		sideBlocksSub := api.e.blockchain.SubscribeChainSideEvent(sideBlocks)
		defer sideBlocksSub.Unsubscribe()

		for {
			select {
			case event := <-canonBlocks:
				service(event.Block)
			case event := <-sideBlocks:
				service(event.Block)
			case <-rpcSub.Err():
				return
			case <-notifier.Closed():
				return
			}
		}
	}()
	return rpcSub, nil
}

// Mining returns an indication if this node is currently mining.
func (api *EthereumAPI) Mining() bool {
	return api.e.IsMining()
}
