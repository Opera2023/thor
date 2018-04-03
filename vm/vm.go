package vm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/vm/evm"
	"github.com/vechain/thor/vm/statedb"
)

// Config is ref to evm.Config.
type Config evm.Config

// Output contains the execution return value.
type Output struct {
	Value           []byte
	Logs            []*Log
	LeftOverGas     uint64
	RefundGas       uint64
	Preimages       map[thor.Bytes32][]byte
	VMErr           error         // VMErr identify the execution result of the contract function, not evm function's err.
	ContractAddress *thor.Address // if create a new contract, or is nil.
}

// Log represents a contract log event. These events are generated by the LOG opcode and
// stored/indexed by the node.
type Log struct {
	// address of the contract that generated the event
	Address thor.Address
	// list of topics provided by the contract.
	Topics []thor.Bytes32
	// supplied by the contract, usually ABI-encoded
	Data []byte
}

// State to decouple with state.State
type State statedb.State

// ContractHook ref evm.ContractHook.
type ContractHook evm.ContractHook

// OnContractCreated ref evm.OnContractCreated
type OnContractCreated evm.OnContractCreated

// OnTransfer callback before transfer occur.
type OnTransfer func(sender, recipient thor.Address, amount *big.Int)

// VM is a facade for ethEvm.
type VM struct {
	evm        *evm.EVM
	statedb    *statedb.StateDB
	onTransfer OnTransfer
}

var chainConfig = &params.ChainConfig{
	ChainId:        big.NewInt(0),
	HomesteadBlock: big.NewInt(0),
	DAOForkBlock:   big.NewInt(0),
	DAOForkSupport: false,
	EIP150Block:    big.NewInt(0),
	EIP150Hash:     common.Hash{},
	EIP155Block:    big.NewInt(0),
	EIP158Block:    big.NewInt(0),
	ByzantiumBlock: big.NewInt(0),
	Ethash:         nil,
	Clique:         nil,
}

// Context for VM runtime.
type Context struct {
	Origin      thor.Address
	Beneficiary thor.Address
	BlockNumber uint32
	Time        uint64
	GasLimit    uint64
	GasPrice    *big.Int
	TxID        thor.Bytes32
	ClauseIndex uint32
	GetHash     func(uint32) thor.Bytes32
}

// The only purpose of this func separate definition is to be compatible with evm.context.
func canTransfer(db evm.StateDB, addr common.Address, amount *big.Int) bool {
	return db.GetBalance(addr).Cmp(amount) >= 0
}

// The only purpose of this func separate definition is to be compatible with evm.Context.
func transfer(db evm.StateDB, sender, recipient common.Address, amount *big.Int) {
	db.SubBalance(sender, amount)
	db.AddBalance(recipient, amount)
}

// New retutrns a new EVM . The returned EVM is not thread safe and should
// only ever be used *once*.
func New(ctx Context, state State, vmConfig Config) *VM {
	statedb := statedb.New(state)
	vm := &VM{statedb: statedb}
	evmCtx := evm.Context{
		CanTransfer: canTransfer,
		Transfer: func(db evm.StateDB, sender, recipient common.Address, amount *big.Int) {
			if vm.onTransfer != nil {
				vm.onTransfer(thor.Address(sender), thor.Address(recipient), amount)
			}
			transfer(db, sender, recipient, amount)
		},
		GetHash: func(n uint64) common.Hash {
			return common.Hash(ctx.GetHash(uint32(n)))
		},
		Difficulty: new(big.Int),

		Origin:      common.Address(ctx.Origin),
		Coinbase:    common.Address(ctx.Beneficiary),
		BlockNumber: new(big.Int).SetUint64(uint64(ctx.BlockNumber)),
		Time:        new(big.Int).SetUint64(ctx.Time),
		GasLimit:    ctx.GasLimit,
		GasPrice:    ctx.GasPrice,
		TxID:        ctx.TxID,
		ClauseIndex: ctx.ClauseIndex,
	}
	vm.evm = evm.NewEVM(evmCtx, statedb, chainConfig, evm.Config(vmConfig))
	return vm
}

// SetContractHook set the hook to hijack contract calls.
func (vm *VM) SetContractHook(hook ContractHook) {
	vm.evm.SetContractHook(evm.ContractHook(hook))
}

// SetOnContractCreated set callback to listen contract creation.
func (vm *VM) SetOnContractCreated(cb OnContractCreated) {
	vm.evm.SetOnContractCreated(evm.OnContractCreated(cb))
}

// SetOnTransfer set callback to listen token transfer.
// OnTransfer will be called before transfer occurred.
func (vm *VM) SetOnTransfer(cb OnTransfer) {
	vm.onTransfer = cb
}

// Cancel cancels any running EVM operation.
// This may be called concurrently and it's safe to be called multiple times.
func (vm *VM) Cancel() {
	vm.evm.Cancel()
}

// Call executes the contract associated with the addr with the given input as parameters.
// It also handles any necessary value transfer required and takes the necessary steps to
// create accounts and reverses the state in case of an execution error or failed value transfer.
func (vm *VM) Call(caller thor.Address, addr thor.Address, input []byte, gas uint64, value *big.Int) *Output {
	ret, leftOverGas, vmErr := vm.evm.Call(&vmContractRef{caller}, common.Address(addr), input, gas, value)
	logs, preimages := vm.extractStateDBOutputs()
	return &Output{ret, logs, leftOverGas, vm.statedb.GetRefund(), preimages, vmErr, nil}
}

// StaticCall executes the contract associated with the addr with the given input as parameters
// while disallowing any modifications to the state during the call.
//
// Opcodes that attempt to perform such modifications will result in exceptions instead of performing
// the modifications.
func (vm *VM) StaticCall(caller thor.Address, addr thor.Address, input []byte, gas uint64) *Output {
	ret, leftOverGas, vmErr := vm.evm.StaticCall(&vmContractRef{caller}, common.Address(addr), input, gas)
	logs, preimages := vm.extractStateDBOutputs()
	return &Output{ret, logs, leftOverGas, vm.statedb.GetRefund(), preimages, vmErr, nil}
}

// Create creates a new contract using code as deployment code.
func (vm *VM) Create(caller thor.Address, code []byte, gas uint64, value *big.Int) *Output {
	ret, contractAddr, leftOverGas, vmErr := vm.evm.Create(&vmContractRef{caller}, code, gas, value)
	contractAddress := thor.Address(contractAddr)
	logs, preimages := vm.extractStateDBOutputs()
	return &Output{ret, logs, leftOverGas, vm.statedb.GetRefund(), preimages, vmErr, &contractAddress}
}

// ChainConfig returns the evmironment's chain configuration
func (vm *VM) ChainConfig() *params.ChainConfig {
	return vm.evm.ChainConfig()
}

func (vm *VM) extractStateDBOutputs() (
	logs []*Log,
	preimages map[thor.Bytes32][]byte,
) {
	vm.statedb.GetOutputs(
		func(log *types.Log) bool {
			logs = append(logs, ethlogToLog(log))
			return true
		},
		func(key common.Hash, value []byte) bool {
			// create on-demand
			if preimages == nil {
				preimages = make(map[thor.Bytes32][]byte)
			}
			preimages[thor.Bytes32(key)] = value
			return true
		},
	)
	return
}

func ethlogToLog(ethlog *types.Log) *Log {
	var topics []thor.Bytes32
	if len(ethlog.Topics) > 0 {
		topics = make([]thor.Bytes32, 0, len(ethlog.Topics))
		for _, t := range ethlog.Topics {
			topics = append(topics, thor.Bytes32(t))
		}
	}
	return &Log{
		thor.Address(ethlog.Address),
		topics,
		ethlog.Data,
	}
}
