// Copyright 2017 The go-ethereum Authors
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

package wpoa

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common/math"
	gov "github.com/ethereum/go-ethereum/consensus/wpoa/bind"
	"github.com/ethereum/go-ethereum/consensus/wpoa/metclient"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pkg/errors"
	"math/big"
	"math/rand"
	"sort"
	"sync"
	"time"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

// Ethash proof-of-work protocol constants.
var (
	FrontierBlockReward           = uint256.NewInt(5e+18) // Block reward in wei for successfully mining a block
	ByzantiumBlockReward          = uint256.NewInt(3e+18) // Block reward in wei for successfully mining a block upward from Byzantium
	ConstantinopleBlockReward     = uint256.NewInt(2e+18) // Block reward in wei for successfully mining a block upward from Constantinople
	maxUncles                     = 2                     // Maximum number of uncles allowed in a single block
	allowedFutureBlockTimeSeconds = int64(15)             // Max seconds from current time allowed for blocks, before they're considered future blocks
	defaultBriocheBlockReward     = big.NewInt(1e18)
	two256                        = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0))
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
var (
	errNotInitialized   = errors.New("not initialized")
	errOlderBlockTime   = errors.New("timestamp older than parent")
	errTooManyUncles    = errors.New("too many uncles")
	errDuplicateUncle   = errors.New("duplicate uncle")
	errUncleIsAncestor  = errors.New("uncle is ancestor")
	errDanglingUncle    = errors.New("uncle's parent is not ancestor")
	errUnauthorized     = errors.New("unauthorized block")
	errInvalidMixDigest = errors.New("invalid mix digest")
	errInvalidPoW       = errors.New("invalid proof-of-work")
	errNotWemixPoA      = errors.New("not the WEMIX consensus engine")
	errInvalidEnode     = errors.New("invalid enode")
)

type WemixPoA struct {
	stack *node.Node

	bootNodeId         string // allowed to generate block without admin contract
	nodeInfo           *p2p.NodeInfo
	bootAccount        common.Address
	rpcCli             *rpc.Client
	cli                *ethclient.Client
	coinbaseEnodeCache *sync.Map
	height2enode       *LruCache

	self *wemixNode

	rand                 *rand.Rand // Properly seeded random source for nonces
	lock                 sync.Mutex // Ensures thread safety for the in-memory caches and mining fields
	blockBuildParamsLock sync.Mutex
	blockBuildParams     *blockBuildParameters
}

type wemixNode struct {
	Name  string         `json:"name"`
	Enode string         `json:"enode"`
	Id    string         `json:"id"`
	Ip    string         `json:"ip"`
	Port  int            `json:"port"`
	Addr  common.Address `json:"addr"`

	Status string `json:"status"`
	Miner  bool   `json:"miner"`
}

// block build parameters for caching
type blockBuildParameters struct {
	height               uint64
	blockInterval        int64
	maxBaseFee           *big.Int
	gasLimit             *big.Int
	baseFeeMaxChangeRate int64
	gasTargetPercentage  int64
}

// cached governance data to derive miner's enode
type coinbaseEnodeEntry struct {
	modifiedBlock  *big.Int
	nodes          []*wemixNode
	coinbase2enode map[string][]byte // string(common.Address[:]) => []byte
	enode2index    map[string]int    // string([]byte) => int
}

type wemixMember struct {
	Staker common.Address `json:"address"`
	Reward common.Address `json:"reward"`
	Stake  *big.Int       `json:"stake"`
}

type rewardParameters struct {
	rewardAmount                                 *big.Int
	staker, ecoSystem, maintenance, feeCollector *common.Address
	members                                      []*wemixMember
	distributionMethod                           []*big.Int
	blocksPer                                    int64
}

type reward struct {
	Addr   common.Address `json:"addr"`
	Reward *big.Int       `json:"reward"`
}

type WemixInfo struct {
	ModifiedBlock             *big.Int
	BlockInterval             *big.Int
	BlocksPer                 *big.Int
	BlockReward               *big.Int
	MaxPriorityFeePerGas      *big.Int
	MaxBaseFee                *big.Int
	GasLimit                  *big.Int
	BaseFeeMaxChangeRate      *big.Int
	GasTargetPercentage       *big.Int
	DefaultBriocheBlockReward *big.Int
	Nodes                     []*wemixNode
}

func Info(engine consensus.Engine) (WemixInfo, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wpoa, ok := engine.(*WemixPoA)
	if !ok {
		return WemixInfo{}, errNotWemixPoA
	}

	block, err := wpoa.cli.HeaderByNumber(ctx, nil)
	if err != nil {
		return WemixInfo{}, err
	}

	contracts, err := wpoa.getRegGovEnvContracts(ctx, block.Number)
	if err != nil {
		return WemixInfo{}, err
	}

	opts := &bind.CallOpts{Context: ctx, BlockNumber: block.Number}
	result := WemixInfo{}
	result.ModifiedBlock, err = contracts.GovImp.ModifiedBlock(opts)
	if err != nil {
		return WemixInfo{}, err
	}

	result.BlockInterval, err = contracts.EnvStorageImp.GetBlockCreationTime(opts)
	if err != nil {
		return WemixInfo{}, err
	}

	result.BlocksPer, err = contracts.EnvStorageImp.GetBlocksPer(opts)
	if err != nil {
		return WemixInfo{}, err
	}

	result.BlockReward, err = contracts.EnvStorageImp.GetBlockRewardAmount(opts)
	if err != nil {
		return WemixInfo{}, err
	}

	result.MaxPriorityFeePerGas, err = contracts.EnvStorageImp.GetMaxPriorityFeePerGas(opts)
	if err != nil {
		return WemixInfo{}, err
	}

	result.MaxBaseFee, err = contracts.EnvStorageImp.GetMaxBaseFee(opts)
	if err != nil {
		return WemixInfo{}, err
	}

	result.GasLimit, result.BaseFeeMaxChangeRate, result.BaseFeeMaxChangeRate, err = contracts.EnvStorageImp.GetGasLimitAndBaseFee(opts)
	if err != nil {
		return WemixInfo{}, err
	}

	result.DefaultBriocheBlockReward = new(big.Int).Set(defaultBriocheBlockReward)

	nodes := make([]*wemixNode, 0)
	nodeLength, err := contracts.GovImp.GetNodeLength(opts)
	if err != nil {
		return WemixInfo{}, err
	}
	count := nodeLength.Int64()
	for i := int64(1); i <= count; i++ {
		node, err := contracts.GovImp.GetNode(opts, big.NewInt(i))
		if err != nil {
			return WemixInfo{}, err
		}
		member, err := contracts.GovImp.GetMember(opts, big.NewInt(i))
		if err != nil {
			return WemixInfo{}, err
		}

		sid := hex.EncodeToString(node.Enode)
		if len(sid) != 128 {
			return WemixInfo{}, errInvalidEnode
		}
		idv4, _ := toIdv4(sid)
		nodes = append(nodes, &wemixNode{
			Name:  string(node.Name),
			Enode: sid,
			Ip:    string(node.Ip),
			Id:    idv4,
			Port:  int(node.Port.Int64()),
			Addr:  member,
		})
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})
	result.Nodes = nodes
	return result, nil
}

func GetMacPriorityFeePerGas(engine consensus.Engine) *big.Int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defaultFee := big.NewInt(100 * params.GWei)
	wpoa, ok := engine.(*WemixPoA)
	if !ok {
		return defaultFee
	}

	block, err := wpoa.cli.HeaderByNumber(ctx, nil)
	if err != nil {
		return defaultFee
	}
	contracts, err := wpoa.getRegGovEnvContracts(ctx, block.Number)
	if err != nil {
		return defaultFee
	}

	opts := &bind.CallOpts{Context: ctx, BlockNumber: block.Number}
	maxPriorityFeePerGas, err := contracts.EnvStorageImp.GetMaxPriorityFeePerGas(opts)
	if err != nil {
		return defaultFee
	}
	return maxPriorityFeePerGas
}

func CalcBaseFee(engine consensus.Engine, config *params.ChainConfig, parent *types.Header) (*big.Int, error) {
	wpoa, ok := engine.(*WemixPoA)
	if !ok {
		return nil, errNotWemixPoA
	}
	return wpoa.CalcBaseFee(config, parent), nil
}

func CalcGasLimit(parentGasLimit, desiredLimit uint64) uint64 {
	if params.FixedGasLimit != 0 {
		return params.FixedGasLimit
	}
	if desiredLimit == 0 { // Wemix: governance is not initialized yet, inherit parent's gas limit
		return parentGasLimit
	}
	return desiredLimit
}

func (wpoa *WemixPoA) Init(stack *node.Node) {
	wpoa.stack = stack
	wpoa.rpcCli = stack.Attach()
	wpoa.cli = ethclient.NewClient(wpoa.rpcCli)
	wpoa.coinbaseEnodeCache = &sync.Map{}
	wpoa.height2enode = NewLruCache(10000, true)
}

// Author implements consensus.Engine, returning the header's coinbase as the
// proof-of-work verified author of the block.
func (wpoa *WemixPoA) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ethash engine.
func (wpoa *WemixPoA) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header) error {
	// Short circuit if the header is known, or its parent not
	number := header.Number.Uint64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	// Sanity checks passed, do a proper verification
	return wpoa.verifyHeader(chain, header, parent, false, time.Now().Unix())
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
func (wpoa *WemixPoA) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header) (chan<- struct{}, <-chan error) {
	if len(headers) == 0 {
		abort, results := make(chan struct{}), make(chan error, len(headers))
		for i := 0; i < len(headers); i++ {
			results <- nil
		}
		return abort, results
	}
	abort := make(chan struct{})
	results := make(chan error, len(headers))
	unixNow := time.Now().Unix()

	go func() {
		for i, header := range headers {
			var parent *types.Header
			if i == 0 {
				parent = chain.GetHeader(headers[0].ParentHash, headers[0].Number.Uint64()-1)
			} else if headers[i-1].Hash() == headers[i].ParentHash {
				parent = headers[i-1]
			}
			var err error
			if parent == nil {
				err = consensus.ErrUnknownAncestor
			} else {
				err = wpoa.verifyHeader(chain, header, parent, false, unixNow)
			}
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of the stock Ethereum ethash engine.
func (wpoa *WemixPoA) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	// Verify that there are at most 2 uncles included in this block
	if len(block.Uncles()) > maxUncles {
		return errTooManyUncles
	}
	if len(block.Uncles()) == 0 {
		return nil
	}
	// Gather the set of past uncles and ancestors
	uncles, ancestors := mapset.NewSet[common.Hash](), make(map[common.Hash]*types.Header)

	number, parent := block.NumberU64()-1, block.ParentHash()
	for i := 0; i < 7; i++ {
		ancestorHeader := chain.GetHeader(parent, number)
		if ancestorHeader == nil {
			break
		}
		ancestors[parent] = ancestorHeader
		// If the ancestor doesn't have any uncles, we don't have to iterate them
		if ancestorHeader.UncleHash != types.EmptyUncleHash {
			// Need to add those uncles to the banned list too
			ancestor := chain.GetBlock(parent, number)
			if ancestor == nil {
				break
			}
			for _, uncle := range ancestor.Uncles() {
				uncles.Add(uncle.Hash())
			}
		}
		parent, number = ancestorHeader.ParentHash, number-1
	}
	ancestors[block.Hash()] = block.Header()
	uncles.Add(block.Hash())

	// Verify each of the uncles that it's recent, but not an ancestor
	for _, uncle := range block.Uncles() {
		// Make sure every uncle is rewarded only once
		hash := uncle.Hash()
		if uncles.Contains(hash) {
			return errDuplicateUncle
		}
		uncles.Add(hash)

		// Make sure the uncle has a valid ancestry
		if ancestors[hash] != nil {
			return errUncleIsAncestor
		}
		if ancestors[uncle.ParentHash] == nil || uncle.ParentHash == block.ParentHash() {
			return errDanglingUncle
		}
		if err := wpoa.verifyHeader(chain, uncle, ancestors[uncle.ParentHash], true, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ethash engine.
// See YP section 4.3.4. "Block Header Validity"
func (wpoa *WemixPoA) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header, uncle bool, unixNow int64) error {
	// Ensure that the header's extra-data section is of a reasonable size
	if uint64(len(header.Extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra-data too long: %d > %d", len(header.Extra), params.MaximumExtraDataSize)
	}
	// Verify the header's timestamp
	if !uncle {
		if header.Time > uint64(unixNow+allowedFutureBlockTimeSeconds) {
			return consensus.ErrFutureBlock
		}
	}
	if header.Time <= parent.Time {
		return errOlderBlockTime
	}

	// WEMIX poa uses 1 for the difficulty
	expected := big.NewInt(1)
	if expected.Cmp(header.Difficulty) != 0 {
		return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty, expected)
	}
	// Verify that the gas limit is <= 2^63-1
	if header.GasLimit > params.MaxGasLimit {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, params.MaxGasLimit)
	}
	// Verify that the gasUsed is <= gasLimit
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}
	// Verify the block's gas usage and (if applicable) verify the base fee.
	if !chain.Config().IsLondon(header.Number) {
		// Verify BaseFee not present before EIP-1559 fork.
		if header.BaseFee != nil {
			return fmt.Errorf("invalid baseFee before fork: have %d, expected 'nil'", header.BaseFee)
		}
		if err := wpoa.VerifyGasLimit(parent.GasLimit, header.GasLimit); err != nil {
			return err
		}
	} else {
		_, _, _, _, gasTargetPercentage, err := wpoa.getBlockBuildParameters(parent.Number)
		if errors.Is(err, errNotInitialized) {
			return nil
		}
		if err := wpoa.VerifyDynamicGasHeader(chain.Config(), parent, header, uint64(gasTargetPercentage)); err != nil {
			// Verify the header's EIP-1559 attributes.
			return err
		}
	}
	// Verify that the block number is parent's +1
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}
	if chain.Config().IsShanghai(header.Number, header.Time) {
		return errors.New("ethash does not support shanghai fork")
	}
	// Verify the non-existence of withdrawalsHash.
	if header.WithdrawalsHash != nil {
		return fmt.Errorf("invalid withdrawalsHash: have %x, expected nil", header.WithdrawalsHash)
	}
	if chain.Config().IsCancun(header.Number, header.Time) {
		return errors.New("ethash does not support cancun fork")
	}
	// Verify the non-existence of cancun-specific header fields
	switch {
	case header.ExcessBlobGas != nil:
		return fmt.Errorf("invalid excessBlobGas: have %d, expected nil", header.ExcessBlobGas)
	case header.BlobGasUsed != nil:
		return fmt.Errorf("invalid blobGasUsed: have %d, expected nil", header.BlobGasUsed)
	case header.ParentBeaconRoot != nil:
		return fmt.Errorf("invalid parentBeaconRoot, have %#x, expected nil", header.ParentBeaconRoot)
	}

	// verify Wemix block seal
	if err := wpoa.verifySeal(header); err != nil {
		return err
	}

	// If all checks passed, validate any special fields for hard forks
	// WEMIX doesn't verify dao extra data
	// if err := misc.VerifyDAOHeaderExtraData(chain.Config(), header); err != nil {return err}
	// WEMIX: Check if it's generated and signed by a registered node
	if wpoa.verifyBlockSig(header.Number, header.Coinbase, header.MinerNodeId, header.Root, header.MinerNodeSig, chain.Config().IsPangyo(header.Number)) {
		return errUnauthorized
	}
	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
func (wpoa *WemixPoA) CalcDifficulty(consensus.ChainHeaderReader, uint64, *types.Header) *big.Int {
	return big.NewInt(params.FixedDifficulty)
}

// VerifyGaslimit verifies the header gas limit according increase/decrease
// in relation to the parent gas limit.
func (wpoa *WemixPoA) VerifyGasLimit(parentGasLimit, headerGasLimit uint64) error {
	// now WEMIX does not check block gas limit
	return nil
}

// Prepare implements consensus.Engine, initializing the difficulty field of a
// header to conform to the ethash protocol. The changes are done inline.
func (wpoa *WemixPoA) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Difficulty = big.NewInt(1)
	return nil
}

// Finalize implements consensus.Engine, accumulating the block and uncle rewards.
func (wpoa *WemixPoA) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body) {
	// Accumulate any block and uncle rewards
	wpoa.accumulateRewards(chain.Config(), state, header, body.Uncles)
}

// FinalizeAndAssemble implements consensus.Engine, accumulating the block and
// uncle rewards, setting the final state and assembling the block.
func (wpoa *WemixPoA) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, body *types.Body, receipts []*types.Receipt) (*types.Block, error) {
	if len(body.Withdrawals) > 0 {
		return nil, errors.New("ethash does not support withdrawals")
	}
	// Finalize block
	wpoa.Finalize(chain, header, state, body)

	// Assign the final state root to header.
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))

	// sign header.Root with node's private key
	coinbase, sig, err := wpoa.signBlock(header.Number, header.Root)
	if err != nil {
		return nil, err
	} else {
		header.Coinbase = coinbase
		header.MinerNodeSig = sig
	}

	// Header seems complete, assemble into a block and return
	return types.NewBlock(header, &types.Body{Transactions: body.Transactions, Uncles: body.Uncles}, receipts, trie.NewStackTrie(nil)), nil
}

func (wpoa *WemixPoA) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	wpoa.lock.Lock()
	if wpoa.rand == nil {
		seed, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt64))
		if err != nil {
			wpoa.lock.Unlock()
			return err
		}
		wpoa.rand = rand.New(rand.NewSource(seed.Int64()))
	}
	wpoa.lock.Unlock()
	abort := make(chan struct{})
	found := make(chan *types.Block)
	go wpoa.mine(block, 0, uint64(wpoa.rand.Int63()), abort, found)
	result := <-found
	select {
	case results <- result:
	default:
		log.Warn("Sealing result is not read by miner", "sealhash", wpoa.SealHash(block.Header()))
	}
	return nil
}

func (wpoa *WemixPoA) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return []rpc.API{}
}

func (wpoa *WemixPoA) Close() error {
	return nil
}

// mine is the actual proof-of-work miner that searches for a nonce starting from
// seed that results in correct final block difficulty.
func (wpoa *WemixPoA) mine(block *types.Block, id int, seed uint64, abort chan struct{}, found chan *types.Block) {
	// Extract some data from the header
	var (
		header = block.Header()
		hash   = wpoa.SealHash(header).Bytes()
		target = new(big.Int).Div(two256, header.Difficulty)
	)
	// Start generating random nonces until we abort or find a good one
	var (
		nonce     = seed
		powBuffer = new(big.Int)
	)
	log.Trace("Started ethash search for new nonces", "seed", seed)
search:
	for {
		select {
		case <-abort:
			// Mining terminated, update stats and abort
			log.Trace("Ethash nonce search aborted", "attempts", nonce-seed)
			break search

		default:
			// We don't have to update hash rate on every nonce, so update after after 2^X nonces
			// Compute the PoW value of this nonce
			var digest, result []byte
			digest, result = hashimeta(hash, nonce)
			if powBuffer.SetBytes(result).Cmp(target) <= 0 {
				// Correct nonce found, create a new header with it
				header = types.CopyHeader(header)
				header.Nonce = types.EncodeNonce(nonce)
				header.MixDigest = common.BytesToHash(digest)

				// Seal and return a block (if still needed)
				select {
				case found <- block.WithSeal(header):
					log.Trace("Ethash nonce found and reported", "attempts", nonce-seed, "nonce", nonce)
				case <-abort:
					log.Trace("Ethash nonce found but discarded", "attempts", nonce-seed, "nonce", nonce)
				}
				break search
			}
			nonce++
		}
	}
}

// SealHash returns the hash of a block prior to it being sealed.
func (wpoa *WemixPoA) SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()

	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	if header.WithdrawalsHash != nil {
		panic("withdrawal hash set on ethash")
	}
	if header.ExcessBlobGas != nil {
		panic("excess blob gas set on ethash")
	}
	if header.BlobGasUsed != nil {
		panic("blob gas used set on ethash")
	}
	if header.ParentBeaconRoot != nil {
		panic("parent beacon root set on ethash")
	}
	rlp.Encode(hasher, enc)
	hasher.Sum(hash[:0])
	return hash
}

// accumulateRewards credits the coinbase of the given block with the mining
// reward. The total reward consists of the static block reward and rewards for
// included uncles. The coinbase of each uncle block is also rewarded.
func (wpoa *WemixPoA) accumulateRewards(config *params.ChainConfig, stateDB *state.StateDB, header *types.Header, uncles []*types.Header) {
	rewards, err := wpoa.calculateRewards(
		config, header.Number, header.Fees,
		func(addr common.Address, amt *big.Int) {
			stateDB.AddBalance(addr, uint256.MustFromBig(amt), tracing.BalanceIncreaseRewardMineBlock)
		})
	if err == nil {
		header.Rewards = rewards
	} else {
		if errors.Is(err, errNotInitialized) {
			reward := new(big.Int)
			if header.Fees != nil {
				reward.Add(reward, header.Fees)
			}
			stateDB.AddBalance(header.Coinbase, uint256.MustFromBig(reward), tracing.BalanceIncreaseRewardMineBlock)
		}
	}
}

func (wpoa *WemixPoA) calculateRewards(config *params.ChainConfig, num, fees *big.Int, addBalance func(common.Address, *big.Int)) ([]byte, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rp, err := wpoa.getRewardParams(ctx, big.NewInt(num.Int64()-1))
	if err != nil {
		// all goes to the coinbase
		return nil, errNotInitialized
	}

	return wpoa.calculateRewardsWithParams(config, rp, num, fees, addBalance)
}

func (wpoa *WemixPoA) calculateRewardsWithParams(config *params.ChainConfig, rp *rewardParameters, num, fees *big.Int, addBalance func(common.Address, *big.Int)) (rewards []byte, err error) {
	if (rp.staker == nil && rp.ecoSystem == nil && rp.maintenance == nil) || len(rp.members) == 0 {
		// handle testnet block 94 rewards
		if rewards94 := wpoa.handleBlock94Rewards(num, rp); rewards94 != nil {
			if addBalance != nil {
				for _, i := range rewards94 {
					addBalance(i.Addr, i.Reward)
				}
			}
			rewards, err = json.Marshal(rewards94)
			return
		}
		err = errNotInitialized
		return
	}

	var blockReward *big.Int
	if config.IsBrioche(num) {
		blockReward = config.Brioche.GetBriocheBlockReward(defaultBriocheBlockReward, num)
	} else {
		// if the wemix chain is not on brioche hard fork, use the `rewardAmount` from gov contract
		blockReward = new(big.Int).Set(rp.rewardAmount)
	}

	// block reward
	// - not brioche chain: use `EnvStorageImp.getBlockRewardAmount()`
	// - brioche chain
	//   - config.Brioche.BlockReward != nil: config.Brioche.BlockReward
	//   - config.Brioche.BlockReward == nil: 1e18
	//   - apply halving for BlockReward
	rr, errr := wpoa.distributeRewards(num, rp, blockReward, fees)
	if errr != nil {
		err = errr
		return
	}

	if addBalance != nil {
		for _, i := range rr {
			addBalance(i.Addr, i.Reward)
		}
	}

	rewards, err = json.Marshal(rr)
	return
}

var testnetBlock94Rewards = []reward{
	{
		Addr:   common.HexToAddress("0x6f488615e6b462ce8909e9cd34c3f103994ab2fb"),
		Reward: new(big.Int).SetInt64(100000000000000000),
	},
	{
		Addr:   common.HexToAddress("0x6bd26c4a45e7d7cac2a389142f99f12e5713d719"),
		Reward: new(big.Int).SetInt64(250000000000000000),
	},
	{
		Addr:   common.HexToAddress("0x816e30b6c314ba5d1a67b1b54be944ce4554ed87"),
		Reward: new(big.Int).SetInt64(306213253695614752),
	},
}

func (wpoa *WemixPoA) handleBlock94Rewards(height *big.Int, rp *rewardParameters) []reward {
	if height.Int64() == 94 && len(rp.members) == 0 &&
		bytes.Equal(rp.staker[:], testnetBlock94Rewards[0].Addr[:]) &&
		bytes.Equal(rp.ecoSystem[:], testnetBlock94Rewards[1].Addr[:]) &&
		bytes.Equal(rp.maintenance[:], testnetBlock94Rewards[2].Addr[:]) {
		return testnetBlock94Rewards
	}
	return nil
}

func (wpoa *WemixPoA) getRegGovEnvContracts(ctx context.Context, height *big.Int) (*gov.GovContracts, error) {
	if ctx == nil {
		var cancel func()
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
	}
	opts := &bind.CallOpts{Context: ctx, BlockNumber: height}
	return gov.GetGovContractsByOwner(opts, wpoa.cli, wpoa.bootAccount)
}

func (wpoa *WemixPoA) getRewardParams(ctx context.Context, height *big.Int) (*rewardParameters, error) {
	rp := &rewardParameters{}
	contracts, err := wpoa.getRegGovEnvContracts(ctx, height)
	if err != nil {
		return nil, err
	}
	opts := &bind.CallOpts{Context: ctx, BlockNumber: height}

	rp.rewardAmount, err = contracts.EnvStorageImp.GetBlockRewardAmount(opts)
	if err != nil {
		return nil, err
	}

	distributionMethod1, distributionMethod2, distributionMethod3, distributionMethod4, err := contracts.EnvStorageImp.GetBlockRewardDistributionMethod(opts)
	if err != nil {
		return nil, err
	}
	rp.distributionMethod = []*big.Int{distributionMethod1, distributionMethod2, distributionMethod3, distributionMethod4}

	staker, err := contracts.Registry.GetContractAddress(opts, metclient.ToBytes32(gov.DOMAIN_Staking))
	if err != nil {
		return nil, errors.Wrap(err, gov.DOMAIN_Staking)
	}
	rp.staker = &staker

	ecoSystem, err := contracts.Registry.GetContractAddress(opts, metclient.ToBytes32(gov.DOMAIN_Ecosystem))
	if err != nil {
		return nil, errors.Wrap(err, gov.DOMAIN_Ecosystem)
	}
	rp.ecoSystem = &ecoSystem

	maintenance, err := contracts.Registry.GetContractAddress(opts, metclient.ToBytes32(gov.DOMAIN_Maintenance))
	if err != nil {
		return nil, errors.Wrap(err, gov.DOMAIN_Maintenance)
	}
	rp.maintenance = &maintenance

	feeCollector, err := contracts.Registry.GetContractAddress(opts, metclient.ToBytes32(gov.DOMAIN_FeeCollector))
	if err != nil {
		rp.feeCollector = nil
	} else {
		rp.feeCollector = &feeCollector
	}

	blocksPer, err := contracts.EnvStorageImp.GetBlocksPer(opts)
	if err != nil {
		return nil, err
	}
	rp.blocksPer = blocksPer.Int64()

	if countBig, err := contracts.GovImp.GetMemberLength(opts); err != nil {
		return nil, err
	} else {
		count := countBig.Int64()
		for i := int64(1); i <= count; i++ {
			index := big.NewInt(i)
			if member, err := contracts.GovImp.GetMember(opts, index); err != nil {
				return nil, err
			} else if reward, err := contracts.GovImp.GetReward(opts, index); err != nil {
				return nil, err
			} else if stake, err := contracts.StakingImp.LockedBalanceOf(opts, member); err != nil {
				return nil, err
			} else {
				rp.members = append(rp.members, &wemixMember{
					Staker: member,
					Reward: reward,
					Stake:  stake,
				})
			}
		}
	}
	return rp, nil
}

// distributeRewards divides the rewardAmount among members according to their
// stakes, and allocates rewards to staker, ecoSystem, and maintenance accounts.
func (wpoa *WemixPoA) distributeRewards(height *big.Int, rp *rewardParameters, blockReward *big.Int, fees *big.Int) ([]reward, error) {
	dm := new(big.Int)
	for i := 0; i < len(rp.distributionMethod); i++ {
		dm.Add(dm, rp.distributionMethod[i])
	}
	if dm.Int64() != 10000 {
		return nil, errNotInitialized
	}

	v10000 := big.NewInt(10000)
	minerAmount := new(big.Int).Set(blockReward)
	minerAmount.Div(minerAmount.Mul(minerAmount, rp.distributionMethod[0]), v10000)
	stakerAmount := new(big.Int).Set(blockReward)
	stakerAmount.Div(stakerAmount.Mul(stakerAmount, rp.distributionMethod[1]), v10000)
	ecoSystemAmount := new(big.Int).Set(blockReward)
	ecoSystemAmount.Div(ecoSystemAmount.Mul(ecoSystemAmount, rp.distributionMethod[2]), v10000)
	// the rest goes to maintenance
	maintenanceAmount := new(big.Int).Set(blockReward)
	maintenanceAmount.Sub(maintenanceAmount, minerAmount)
	maintenanceAmount.Sub(maintenanceAmount, stakerAmount)
	maintenanceAmount.Sub(maintenanceAmount, ecoSystemAmount)

	// if feeCollector is not specified, i.e. nil, fees go to maintenance
	if rp.feeCollector == nil {
		maintenanceAmount.Add(maintenanceAmount, fees)
	}

	var rewards []reward
	if n := len(rp.members); n > 0 {
		stakeTotal, equalStakes := big.NewInt(0), true
		for i := 0; i < n; i++ {
			if equalStakes && i < n-1 && rp.members[i].Stake.Cmp(rp.members[i+1].Stake) != 0 {
				equalStakes = false
			}
			stakeTotal.Add(stakeTotal, rp.members[i].Stake)
		}

		if equalStakes {
			v0, v1 := big.NewInt(0), big.NewInt(1)
			vn := big.NewInt(int64(n))
			b := new(big.Int).Set(minerAmount)
			d := new(big.Int)
			d.Div(b, vn)
			for i := 0; i < n; i++ {
				rewards = append(rewards, reward{
					Addr:   rp.members[i].Reward,
					Reward: new(big.Int).Set(d),
				})
			}
			d.Mul(d, vn)
			b.Sub(b, d)
			for ix := height.Int64() % int64(n); b.Cmp(v0) > 0; ix = (ix + 1) % int64(n) {
				rewards[ix].Reward.Add(rewards[ix].Reward, v1)
				b.Sub(b, v1)
			}
		} else {
			// rewards distributed according to stakes
			v0, v1 := big.NewInt(0), big.NewInt(1)
			remainder := new(big.Int).Set(minerAmount)
			for i := 0; i < n; i++ {
				memberReward := new(big.Int).Mul(minerAmount, rp.members[i].Stake)
				memberReward.Div(memberReward, stakeTotal)
				remainder.Sub(remainder, memberReward)
				rewards = append(rewards, reward{
					Addr:   rp.members[i].Reward,
					Reward: memberReward,
				})
			}
			for ix := height.Int64() % int64(n); remainder.Cmp(v0) > 0; ix = (ix + 1) % int64(n) {
				rewards[ix].Reward.Add(rewards[ix].Reward, v1)
				remainder.Sub(remainder, v1)
			}
		}
	}
	rewards = append(rewards, reward{
		Addr:   *rp.staker,
		Reward: stakerAmount,
	})
	rewards = append(rewards, reward{
		Addr:   *rp.ecoSystem,
		Reward: ecoSystemAmount,
	})
	rewards = append(rewards, reward{
		Addr:   *rp.maintenance,
		Reward: maintenanceAmount,
	})
	if rp.feeCollector != nil {
		rewards = append(rewards, reward{
			Addr:   *rp.feeCollector,
			Reward: fees,
		})
	}
	return rewards, nil
}

func (wpoa *WemixPoA) verifySeal(header *types.Header) error {
	digest, result := hashimeta(sealHash(header).Bytes(), header.Nonce.Uint64())
	// Verify the calculated values against the ones provided in the header
	if !bytes.Equal(header.MixDigest[:], digest) {
		return errInvalidMixDigest
	}
	target := new(big.Int).Div(two256, header.Difficulty)
	if new(big.Int).SetBytes(result).Cmp(target) > 0 {
		return errInvalidPoW
	}
	return nil
}

func sealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()

	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	rlp.Encode(hasher, enc)
	hasher.Sum(hash[:0])
	return hash
}

func hashimeta(hash []byte, nonce uint64) ([]byte, []byte) {
	// Combine header+nonce into a 64 byte seed
	seed := make([]byte, 40)
	copy(seed, hash)
	binary.LittleEndian.PutUint64(seed[32:], nonce)

	result := crypto.Keccak256(seed)
	return result, result
}

func (wpoa *WemixPoA) signBlock(height *big.Int, hash common.Hash) (common.Address, []byte, error) {
	prvKey := wpoa.stack.Server().PrivateKey
	sig, err := crypto.Sign(crypto.Keccak256(append(height.Bytes(), hash.Bytes()...)), prvKey)
	if err != nil {
		return common.Address{}, nil, err
	}

	if wpoa.nodeInfo == nil {
		nodeInfo, err := wpoa.getNodeInfo()
		if err != nil {
			log.Error("Failed to get node info", "error", err)
		} else {
			wpoa.nodeInfo = nodeInfo
		}
	}
	if wpoa.nodeInfo != nil && wpoa.nodeInfo.ID == wpoa.bootNodeId {
		return wpoa.bootAccount, sig, nil
	} else if wpoa.self != nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		num := new(big.Int).Sub(height, common.Big1)
		contracts, err := wpoa.getRegGovEnvContracts(ctx, num)
		if err != nil {
			return common.Address{}, nil, err
		}

		nodeId := crypto.FromECDSAPub(&prvKey.PublicKey)[1:]
		if addr, err := wpoa.enodeExists(ctx, height, contracts.GovImp, nodeId); err != nil {
			return common.Address{}, nil, err
		} else {
			return addr, sig, nil
		}
	} else {
		return common.Address{}, sig, errNotInitialized
	}
}

func (wpoa *WemixPoA) verifyBlockSig(height *big.Int, coinbase common.Address, nodeId []byte, hash common.Hash, sig []byte, checkMinerLimit bool) bool {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// get nodeid from the coinbase
	num := new(big.Int).Sub(height, common.Big1)
	contracts, err := wpoa.getRegGovEnvContracts(ctx, num)
	if err != nil {
		return err == errNotInitialized || errors.Is(err, ethereum.NotFound)
	} else if count, err := contracts.GovImp.GetMemberLength(&bind.CallOpts{Context: ctx, BlockNumber: num}); err != nil || count.Sign() == 0 {
		return err == errNotInitialized || count.Sign() == 0
	}
	gov := contracts.GovImp
	// if minerNodeId is given, i.e. present in block header, use it,
	// otherwise, derive it from the codebase
	var data []byte
	if len(nodeId) == 0 {
		nodeId, err = wpoa.coinbaseExists(ctx, height, gov, &coinbase)
		if err != nil || len(nodeId) == 0 {
			return false
		}
		data = append(height.Bytes(), hash.Bytes()...)
		data = crypto.Keccak256(data)
	} else {
		if _, err := wpoa.enodeExists(ctx, height, gov, nodeId); err != nil {
			return false
		}
		data = hash.Bytes()
	}
	pubKey, err := crypto.Ecrecover(data, sig)
	if err != nil || len(pubKey) < 1 || !bytes.Equal(nodeId, pubKey[1:]) {
		return false
	}
	// check miner limit
	if !checkMinerLimit {
		return true
	}
	ok, err := wpoa.verifyMinerLimit(ctx, height, gov, &coinbase, nodeId)
	return err == nil && ok
}

func (wpoa *WemixPoA) getCoinbaseEnodeCache(ctx context.Context, height *big.Int, gov *gov.GovImp) (*coinbaseEnodeEntry, error) {
	opts := &bind.CallOpts{Context: ctx, BlockNumber: height}
	modifiedBlock, err := gov.ModifiedBlock(opts)
	if err != nil {
		return nil, err
	}
	if modifiedBlock.Sign() == 0 {
		return nil, errNotInitialized
	}

	// if found in cache, use it
	if e, ok := wpoa.coinbaseEnodeCache.Load(modifiedBlock.Int64()); ok {
		return e.(*coinbaseEnodeEntry), nil
	}
	// otherwise, load it from the governance
	var (
		count       *big.Int
		addr        common.Address
		name, enode []byte
		e           = &coinbaseEnodeEntry{
			modifiedBlock:  modifiedBlock,
			coinbase2enode: map[string][]byte{},
			enode2index:    map[string]int{},
		}
	)
	if count, err = gov.GetNodeLength(opts); err != nil {
		return nil, err
	}
	for i := int64(1); i <= count.Int64(); i++ {
		ix := big.NewInt(i)
		if addr, err = gov.GetReward(opts, ix); err != nil {
			return nil, err
		}

		if output, err := gov.GetNode(opts, ix); err != nil {
			return nil, err
		} else {
			name, enode = output.Name, output.Enode
		}

		idv4, _ := toIdv4(hex.EncodeToString(enode))
		e.nodes = append(e.nodes, &wemixNode{
			Name:  string(name),
			Enode: string(enode), // note that this is not in hex unlike wemixAdmin
			Id:    idv4,
			Addr:  addr,
		})
		e.coinbase2enode[string(addr[:])] = enode
		e.enode2index[string(enode)] = int(i) // 1-based, not 0-based
	}
	wpoa.coinbaseEnodeCache.Store(modifiedBlock.Int64(), e)
	return e, nil
}

func toIdv4(id string) (string, error) {
	if len(id) == 64 {
		return id, nil
	} else if len(id) == 128 {
		idv4, err := enode.ParseV4(fmt.Sprintf("enode://%v@127.0.0.1:8589", id))
		if err != nil {
			return "", err
		} else {
			return idv4.ID().String(), nil
		}
	} else {
		return "", fmt.Errorf("invalid V5 Identifier")
	}
}

// returns coinbase's enode if exists in governance at given height - 1
func (wpoa *WemixPoA) coinbaseExists(ctx context.Context, height *big.Int, gov *gov.GovImp, coinbase *common.Address) ([]byte, error) {
	e, err := wpoa.getCoinbaseEnodeCache(ctx, new(big.Int).Sub(height, common.Big1), gov)
	if err != nil {
		return nil, err
	}
	enode, ok := e.coinbase2enode[string(coinbase[:])]
	if !ok {
		return nil, nil
	}
	return enode, nil
}

// returns true if enode exists in governance at given height-1
func (wpoa *WemixPoA) enodeExists(ctx context.Context, height *big.Int, gov *gov.GovImp, enode []byte) (common.Address, error) {
	e, err := wpoa.getCoinbaseEnodeCache(ctx, new(big.Int).Sub(height, common.Big1), gov)
	if err != nil {
		return common.Address{}, err
	}
	ix, ok := e.enode2index[string(enode)]
	if !ok {
		return common.Address{}, ethereum.NotFound
	}
	return e.nodes[ix-1].Addr, nil
}

func (wpoa *WemixPoA) verifyMinerLimit(ctx context.Context, height *big.Int, gov *gov.GovImp, coinbase *common.Address, enode []byte) (bool, error) {
	// parent block number
	prev := new(big.Int).Sub(height, common.Big1)
	e, err := wpoa.getCoinbaseEnodeCache(ctx, prev, gov)
	if err != nil {
		return false, err
	}
	// if count <= 2, not enforced
	if len(e.nodes) <= 2 {
		return true, nil
	}
	// if enode is not given, derive it from the coinbase
	if len(enode) == 0 {
		enode2, ok := e.coinbase2enode[string(coinbase[:])]
		if !ok {
			return false, nil
		}
		enode = enode2
	}
	var miners [][]byte
	// the enode should not appear within the last (member count / 2) blocks
	limit := len(e.nodes) / 2
	if limit > int(height.Int64()-e.modifiedBlock.Int64()-1) {
		limit = int(height.Int64() - e.modifiedBlock.Int64() - 1)
	}
	for h := new(big.Int).Set(prev); limit > 0; h, limit = h.Sub(h, common.Big1), limit-1 {
		blockMinerEnode, err := wpoa.getBlockMiner(ctx, wpoa.cli, e, h)
		if err != nil {
			return false, err
		}
		miners = append(miners, blockMinerEnode[:])
		if bytes.Equal(enode[:], blockMinerEnode[:]) {
			return false, nil
		}
	}
	return true, nil
}

// return block's miner node id
func (wpoa *WemixPoA) getBlockMiner(ctx context.Context, cli *ethclient.Client, entry *coinbaseEnodeEntry, height *big.Int) ([]byte, error) {
	// if already cached, use it
	if enode := wpoa.height2enode.Get(height.Uint64()); enode != nil {
		return enode.([]byte), nil
	}
	block, err := cli.HeaderByNumber(ctx, height)
	if err != nil {
		return nil, err
	}
	if len(block.MinerNodeId) == 0 {
		enode, ok := entry.coinbase2enode[string(block.Coinbase[:])]
		if !ok {
			return nil, nil
		}
		wpoa.height2enode.Put(height.Uint64(), enode)
		return enode, nil
	} else {
		if _, ok := entry.enode2index[string(block.MinerNodeId)]; !ok {
			return nil, nil
		}
		wpoa.height2enode.Put(height.Uint64(), block.MinerNodeId)
		return block.MinerNodeId, nil
	}
}

func (wpoa *WemixPoA) getNodeInfo() (*p2p.NodeInfo, error) {
	var nodeInfo *p2p.NodeInfo
	ctx, cancel := context.WithCancel(context.Background())
	err := wpoa.rpcCli.CallContext(ctx, &nodeInfo, "admin_nodeInfo")
	cancel()
	if err != nil {
		log.Error("Cannot get node info", "error", err)
	}
	return nodeInfo, err
}

func (wpoa *WemixPoA) getBlockBuildParameters(height *big.Int) (blockInterval int64, maxBaseFee, gasLimit *big.Int, baseFeeMaxChangeRate, gasTargetPercentage int64, err error) {
	err = errNotInitialized

	wpoa.blockBuildParamsLock.Lock()
	if wpoa.blockBuildParams != nil && wpoa.blockBuildParams.height == height.Uint64() {
		// use cached
		blockInterval = wpoa.blockBuildParams.blockInterval
		maxBaseFee = wpoa.blockBuildParams.maxBaseFee
		gasLimit = wpoa.blockBuildParams.gasLimit
		baseFeeMaxChangeRate = wpoa.blockBuildParams.baseFeeMaxChangeRate
		gasTargetPercentage = wpoa.blockBuildParams.gasTargetPercentage
		wpoa.blockBuildParamsLock.Unlock()
		err = nil
		return
	}
	wpoa.blockBuildParamsLock.Unlock()

	// default values
	blockInterval = 15
	maxBaseFee = big.NewInt(0)
	gasLimit = big.NewInt(0)
	baseFeeMaxChangeRate = 0
	gasTargetPercentage = 100

	if wpoa.self == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		env *gov.EnvStorageImp
		gov *gov.GovImp
	)
	if contracts, err2 := wpoa.getRegGovEnvContracts(ctx, height); err2 != nil {
		err = errNotInitialized
		return
	} else {
		env, gov = contracts.EnvStorageImp, contracts.GovImp
	}

	opts := &bind.CallOpts{Context: ctx, BlockNumber: height}
	if count, err2 := gov.GetMemberLength(opts); err2 != nil || count.Sign() == 0 {
		err = errNotInitialized
		return
	}
	if v, err2 := env.GetBlockCreationTime(opts); err2 != nil {
		err = errNotInitialized
		return
	} else {
		blockInterval = v.Int64()
	}

	if GasLimit, BaseFeeMaxChangeRate, GasTargetPercentage, err2 := env.GetGasLimitAndBaseFee(opts); err2 != nil {
		err = errNotInitialized
		return
	} else {
		gasLimit = GasLimit
		baseFeeMaxChangeRate = BaseFeeMaxChangeRate.Int64()
		gasTargetPercentage = GasTargetPercentage.Int64()
	}

	if maxBaseFee, err = env.GetMaxBaseFee(opts); err != nil {
		err = errNotInitialized
		return
	}

	// cache it
	wpoa.blockBuildParamsLock.Lock()
	wpoa.blockBuildParams = &blockBuildParameters{
		height:               height.Uint64(),
		blockInterval:        blockInterval,
		maxBaseFee:           maxBaseFee,
		gasLimit:             gasLimit,
		baseFeeMaxChangeRate: baseFeeMaxChangeRate,
		gasTargetPercentage:  gasTargetPercentage,
	}
	wpoa.blockBuildParamsLock.Unlock()
	err = nil
	return
}
