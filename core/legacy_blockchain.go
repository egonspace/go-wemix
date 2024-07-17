package core

import (
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	cmath "github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
	"math/big"
)

func SubstituteProcessorToLegacy(blockchain *BlockChain) {
	blockchain.processor = NewStateProcessorLegacy(
		blockchain.processor.(*StateProcessor).config,
		blockchain.processor.(*StateProcessor).chain)
}

type StateProcessorLegacy struct {
	config *params.ChainConfig // Chain configuration options
	chain  *HeaderChain        // Canonical header chain
}

func NewStateProcessorLegacy(config *params.ChainConfig, chain *HeaderChain) *StateProcessorLegacy {
	return &StateProcessorLegacy{
		config: config,
		chain:  chain,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessorLegacy) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts    types.Receipts
		usedGas     = new(uint64)
		fees        = new(big.Int)
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		gp          = new(GasPool).AddGas(block.GasLimit())
	)
	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	blockContext := NewEVMBlockContext(header, p.chain, nil)
	vmenv := vm.NewEVM(blockContext, vm.TxContext{}, statedb, p.config, cfg)
	// Iterate over and process the individual transactions
	for i, tx := range block.Transactions() {
		msg, err := TransactionToMessage(tx, types.MakeSigner(p.config, header.Number, header.Time), header.BaseFee)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		statedb.SetTxContext(tx.Hash(), i)

		receipt, err := ApplyTransactionWithEVMLegacy(msg, p.config, gp, statedb, blockNumber, blockHash, tx, usedGas, fees, vmenv)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		receipts = append(receipts, receipt)
		allLogs = append(allLogs, receipt.Logs...)
	}
	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.chain.engine.Finalize(p.chain, header, statedb, block.Body())

	header.Fees = fees
	if block.Hash() != header.Hash() {
		return receipts, nil, 0, fmt.Errorf("remote block hash is different from being processed one locally (remote=%v, local=%v)", block.Header().Hash(), header.Hash())
	}

	return receipts, allLogs, *usedGas, nil
}

func ApplyTransactionWithEVMLegacy(msg *Message, config *params.ChainConfig, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, tx *types.Transaction, usedGas *uint64, fees *big.Int, evm *vm.EVM) (receipt *types.Receipt, err error) {
	if evm.Config.Tracer != nil && evm.Config.Tracer.OnTxStart != nil {
		evm.Config.Tracer.OnTxStart(evm.GetVMContext(), tx, msg.From)
		if evm.Config.Tracer.OnTxEnd != nil {
			defer func() {
				evm.Config.Tracer.OnTxEnd(receipt, err)
			}()
		}
	}
	// Create a new context to be used in the EVM environment.
	txContext := NewEVMTxContext(msg)
	evm.Reset(txContext, statedb)

	// Apply the transaction to the current state (included in the env).
	result, earnedFee, err := ApplyMessagLegacy(evm, msg, gp)
	if err != nil {
		return nil, err
	}

	// Update the state with pending changes.
	var root []byte
	if config.IsByzantium(blockNumber) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(config.IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas
	fees.Add(fees, earnedFee)

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt = &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: *usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	if tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(tx.BlobHashes()) * params.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = evm.Context.BlobBaseFee
	}

	// If the transaction created a contract, store the creation address in the receipt.
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	return receipt, err
}

func ApplyMessagLegacy(evm *vm.EVM, msg *Message, gp *GasPool) (*ExecutionResult, *big.Int, error) {
	return NewStateTransitionLegacy(evm, msg, gp).TransitionDbLegacy()
}

type LegacyStateTransition struct {
	gp           *GasPool
	msg          *Message
	gasRemaining uint64
	initialGas   uint64
	state        vm.StateDB
	evm          *vm.EVM
}

// NewStateTransition initialises and returns a new state transition object.
func NewStateTransitionLegacy(evm *vm.EVM, msg *Message, gp *GasPool) *LegacyStateTransition {
	return &LegacyStateTransition{
		gp:    gp,
		evm:   evm,
		msg:   msg,
		state: evm.StateDB,
	}
}

func (st *LegacyStateTransition) TransitionDbLegacy() (*ExecutionResult, *big.Int, error) {
	// First check this message satisfies all consensus rules before
	// applying the message. The rules include these clauses
	//
	// 1. the nonce of the message caller is correct
	// 2. caller has enough balance to cover transaction fee(gaslimit * gasprice)
	// 3. the amount of gas required is available in the block
	// 4. the purchased gas is enough to cover intrinsic usage
	// 5. there is no overflow when calculating intrinsic gas
	// 6. caller has enough balance to cover asset transfer for **topmost** call

	// Check clauses 1-3, buy gas if everything is correct
	if err := st.preCheck(); err != nil {
		return nil, nil, err
	}

	var (
		msg              = st.msg
		sender           = vm.AccountRef(msg.From)
		rules            = st.evm.ChainConfig().Rules(st.evm.Context.BlockNumber, st.evm.Context.Random != nil, st.evm.Context.Time)
		contractCreation = msg.To == nil
	)

	// Check clauses 4-5, subtract intrinsic gas if everything is correct
	gas, err := IntrinsicGas(msg.Data, msg.AccessList, contractCreation, rules.IsHomestead, rules.IsIstanbul, rules.IsShanghai)
	if err != nil {
		return nil, nil, err
	}
	if st.gasRemaining < gas {
		return nil, new(big.Int), fmt.Errorf("%w: have %d, want %d", ErrIntrinsicGas, st.gasRemaining, gas)
	}
	if t := st.evm.Config.Tracer; t != nil && t.OnGasChange != nil {
		t.OnGasChange(st.gasRemaining, st.gasRemaining-gas, tracing.GasChangeTxIntrinsicGas)
	}
	st.gasRemaining -= gas

	if rules.IsEIP4762 {
		st.evm.AccessEvents.AddTxOrigin(msg.From)

		if targetAddr := msg.To; targetAddr != nil {
			st.evm.AccessEvents.AddTxDestination(*targetAddr, msg.Value.Sign() != 0)
		}
	}

	// Check clause 6
	value, overflow := uint256.FromBig(msg.Value)
	if overflow {
		return nil, nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From.Hex())
	}
	if !value.IsZero() && !st.evm.Context.CanTransfer(st.state, msg.From, value) {
		return nil, nil, fmt.Errorf("%w: address %v", ErrInsufficientFundsForTransfer, msg.From.Hex())
	}

	// Check whether the init code size has been exceeded.
	if rules.IsShanghai && contractCreation && len(msg.Data) > params.MaxInitCodeSize {
		return nil, nil, fmt.Errorf("%w: code size %v limit %v", ErrMaxInitCodeSizeExceeded, len(msg.Data), params.MaxInitCodeSize)
	}

	// Execute the preparatory steps for state transition which includes:
	// - prepare accessList(post-berlin)
	// - reset transient storage(eip 1153)
	st.state.Prepare(rules, msg.From, st.evm.Context.Coinbase, msg.To, vm.ActivePrecompiles(rules), msg.AccessList)

	var (
		ret   []byte
		vmerr error // vm errors do not effect consensus and are therefore not assigned to err
	)
	if contractCreation {
		ret, _, st.gasRemaining, vmerr = st.evm.Create(sender, msg.Data, st.gasRemaining, value)
	} else {
		// Increment the nonce for the next transaction
		st.state.SetNonce(msg.From, st.state.GetNonce(sender.Address())+1)
		ret, st.gasRemaining, vmerr = st.evm.Call(sender, st.to(), msg.Data, st.gasRemaining, value)
	}

	var gasRefund uint64
	if !rules.IsLondon {
		// Before EIP-3529: refunds were capped to gasUsed / 2
		gasRefund = st.refundGas(params.RefundQuotient)
	} else {
		// After EIP-3529: refunds are capped to gasUsed / 5
		gasRefund = st.refundGas(params.RefundQuotientEIP3529)
	}
	effectiveTip := msg.GasPrice
	if rules.IsLondon {
		effectiveTip = cmath.BigMin(msg.GasTipCap, new(big.Int).Sub(msg.GasFeeCap, st.evm.Context.BaseFee))
	}

	fee := new(big.Int).Mul(new(big.Int).SetUint64(st.gasUsed()), effectiveTip)

	return &ExecutionResult{
		UsedGas:     st.gasUsed(),
		RefundedGas: gasRefund,
		Err:         vmerr,
		ReturnData:  ret,
	}, fee, nil
}

func (st *LegacyStateTransition) to() common.Address {
	if st.msg == nil || st.msg.To == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To
}

func (st *LegacyStateTransition) preCheck() error {
	// Only check transactions that are not fake
	msg := st.msg
	if !msg.SkipAccountChecks {
		// Make sure this transaction's nonce is correct.
		stNonce := st.state.GetNonce(msg.From)
		if msgNonce := msg.Nonce; stNonce < msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooHigh,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce > msgNonce {
			return fmt.Errorf("%w: address %v, tx: %d state: %d", ErrNonceTooLow,
				msg.From.Hex(), msgNonce, stNonce)
		} else if stNonce+1 < stNonce {
			return fmt.Errorf("%w: address %v, nonce: %d", ErrNonceMax,
				msg.From.Hex(), stNonce)
		}
		// Make sure the sender is an EOA
		codeHash := st.state.GetCodeHash(msg.From)
		if codeHash != (common.Hash{}) && codeHash != types.EmptyCodeHash {
			return fmt.Errorf("%w: address %v, codehash: %s", ErrSenderNoEOA,
				msg.From.Hex(), codeHash)
		}
	}
	// Make sure that transaction gasFeeCap is greater than the baseFee (post london)
	if st.evm.ChainConfig().IsLondon(st.evm.Context.BlockNumber) {
		// Skip the checks if gas fields are zero and baseFee was explicitly disabled (eth_call)
		skipCheck := st.evm.Config.NoBaseFee && msg.GasFeeCap.BitLen() == 0 && msg.GasTipCap.BitLen() == 0
		if !skipCheck {
			if l := msg.GasFeeCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxFeePerGas bit length: %d", ErrFeeCapVeryHigh,
					msg.From.Hex(), l)
			}
			if l := msg.GasTipCap.BitLen(); l > 256 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas bit length: %d", ErrTipVeryHigh,
					msg.From.Hex(), l)
			}
			if msg.GasFeeCap.Cmp(msg.GasTipCap) < 0 {
				return fmt.Errorf("%w: address %v, maxPriorityFeePerGas: %s, maxFeePerGas: %s", ErrTipAboveFeeCap,
					msg.From.Hex(), msg.GasTipCap, msg.GasFeeCap)
			}
			// This will panic if baseFee is nil, but basefee presence is verified
			// as part of header validation.
			if msg.GasFeeCap.Cmp(st.evm.Context.BaseFee) < 0 {
				return fmt.Errorf("%w: address %v, maxFeePerGas: %s, baseFee: %s", ErrFeeCapTooLow,
					msg.From.Hex(), msg.GasFeeCap, st.evm.Context.BaseFee)
			}
		}
	}
	// Check the blob version validity
	if msg.BlobHashes != nil {
		// The to field of a blob tx type is mandatory, and a `BlobTx` transaction internally
		// has it as a non-nillable value, so any msg derived from blob transaction has it non-nil.
		// However, messages created through RPC (eth_call) don't have this restriction.
		if msg.To == nil {
			return ErrBlobTxCreate
		}
		if len(msg.BlobHashes) == 0 {
			return ErrMissingBlobHashes
		}
		for i, hash := range msg.BlobHashes {
			if !kzg4844.IsValidVersionedHash(hash[:]) {
				return fmt.Errorf("blob %d has invalid hash version", i)
			}
		}
	}
	return st.buyGas()
}

func (st *LegacyStateTransition) buyGas() error {
	if st.msg.FeePayer == nil || *st.msg.FeePayer == st.msg.From {
		mgval := new(big.Int).SetUint64(st.msg.GasLimit)
		mgval.Mul(mgval, st.msg.GasPrice)
		balanceCheck := new(big.Int).Set(mgval)
		if st.msg.GasFeeCap != nil {
			balanceCheck.SetUint64(st.msg.GasLimit)
			balanceCheck = balanceCheck.Mul(balanceCheck, st.msg.GasFeeCap)
		}
		balanceCheck.Add(balanceCheck, st.msg.Value)

		balanceCheckU256, overflow := uint256.FromBig(balanceCheck)
		if overflow {
			return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
		}
		if have, want := st.state.GetBalance(st.msg.From), balanceCheckU256; have.Cmp(want) < 0 {
			return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From.Hex(), have, want)
		}
		if err := st.gp.SubGas(st.msg.GasLimit); err != nil {
			return err
		}

		if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil {
			st.evm.Config.Tracer.OnGasChange(0, st.msg.GasLimit, tracing.GasChangeTxInitialBalance)
		}
		st.gasRemaining = st.msg.GasLimit

		st.initialGas = st.msg.GasLimit
		mgvalU256, _ := uint256.FromBig(mgval)
		st.state.SubBalance(st.msg.From, mgvalU256, tracing.BalanceDecreaseGasBuy)
		return nil
	} else {
		if !st.evm.ChainConfig().IsApplepie(st.evm.Context.BlockNumber) {
			return fmt.Errorf("%w: fee delegation type not supported", ErrTxTypeNotSupported)
		}

		mgval := new(big.Int).SetUint64(st.msg.GasLimit)
		mgval.Mul(mgval, st.msg.GasPrice)
		feeCheck := new(big.Int).Set(mgval)
		if st.msg.GasFeeCap != nil {
			feeCheck.SetUint64(st.msg.GasLimit)
			feeCheck = feeCheck.Mul(feeCheck, st.msg.GasFeeCap)
		}
		valCheck := new(big.Int).Set(st.msg.Value)

		feeCheckU256, overflow := uint256.FromBig(feeCheck)
		if overflow {
			return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
		}
		valCheckU256, overflow := uint256.FromBig(valCheck)
		if overflow {
			return fmt.Errorf("%w: address %v required balance exceeds 256 bits", ErrInsufficientFunds, st.msg.From.Hex())
		}
		if have, want := st.state.GetBalance(*st.msg.FeePayer), feeCheckU256; have.Cmp(want) < 0 {
			return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.FeePayer.Hex(), have, want)
		}
		if have, want := st.state.GetBalance(st.msg.From), valCheckU256; have.Cmp(want) < 0 {
			return fmt.Errorf("%w: address %v have %v want %v", ErrInsufficientFunds, st.msg.From.Hex(), have, want)
		}
		if err := st.gp.SubGas(st.msg.GasLimit); err != nil {
			return err
		}

		if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil {
			st.evm.Config.Tracer.OnGasChange(0, st.msg.GasLimit, tracing.GasChangeTxInitialBalance)
		}
		st.gasRemaining = st.msg.GasLimit

		st.initialGas = st.msg.GasLimit
		mgvalU256, _ := uint256.FromBig(mgval)
		st.state.SubBalance(*st.msg.FeePayer, mgvalU256, tracing.BalanceDecreaseGasBuy)
		return nil
	}
}

func (st *LegacyStateTransition) refundGas(refundQuotient uint64) uint64 {
	// Apply refund counter, capped to a refund quotient
	refund := st.gasUsed() / refundQuotient
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}

	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && refund > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining, st.gasRemaining+refund, tracing.GasChangeTxRefunds)
	}

	st.gasRemaining += refund

	// Return ETH for remaining gas, exchanged at the original rate.
	remaining := uint256.NewInt(st.gasRemaining)
	remaining.Mul(remaining, uint256.MustFromBig(st.msg.GasPrice))

	if st.msg.FeePayer != nil {
		st.state.AddBalance(*st.msg.FeePayer, remaining, tracing.BalanceIncreaseGasReturn)
	} else {
		st.state.AddBalance(st.msg.From, remaining, tracing.BalanceIncreaseGasReturn)
	}

	if st.evm.Config.Tracer != nil && st.evm.Config.Tracer.OnGasChange != nil && st.gasRemaining > 0 {
		st.evm.Config.Tracer.OnGasChange(st.gasRemaining, 0, tracing.GasChangeTxLeftOverReturned)
	}

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	st.gp.AddGas(st.gasRemaining)

	return refund
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *LegacyStateTransition) gasUsed() uint64 {
	return st.initialGas - st.gasRemaining
}
