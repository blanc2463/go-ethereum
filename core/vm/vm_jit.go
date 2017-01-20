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

package vm

/*

#include <evmjit.h>


static struct evm_instance* new_evmjit()
{
	// Declare exported Go functions. The are ABI compatible with C callbacks
	// but differ by type names and const pointers.
	void query(void*, size_t, int, void*);
	void update(size_t env, int key, void* arg1, void* arg2);
	long long call(void* env, int kind, long long gas, void* address,
		void* value, void* input, size_t input_size, void* output,
		size_t output_size);

	struct evm_factory factory = evmjit_get_factory();
	return factory.create((evm_query_fn)query, (evm_update_fn)update,
		(evm_call_fn)call);
}

static struct evm_result evm_execute(struct evm_instance* instance,
	struct evm_env* env, enum evm_mode mode, struct evm_uint256be code_hash,
	uint8_t const* code, size_t code_size, int64_t gas, uint8_t const* input,
	size_t input_size, struct evm_uint256be value)
{
	return instance->execute(instance, env, mode, code_hash, code, code_size,
		gas, input, input_size, value);
}

static void evm_release_result(struct evm_result* result)
{
	result->release(result);
}

#cgo CFLAGS:  -I/home/chfast/Projects/ethereum/evmjit/include
#cgo LDFLAGS: -levmjit-standalone -lstdc++ -lm -ldl -L/home/chfast/Projects/ethereum/evmjit/build/release-llvm/libevmjit
*/
import "C"

import (
	"fmt"
	"math/big"
	"reflect"
	"sync"
	"unsafe"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

type EVMJIT struct {
	jit  *C.struct_evm_instance
	env  *EVM
}

type EVMCContext struct {
	contract *Contract
	env      *EVM
}

func NewJit(env *EVM, cfg Config) *EVMJIT {
	// FIXME: Destroy the jit later.
	return &EVMJIT{C.new_evmjit(), env}
}


var contextMap = make(map[uintptr]*EVMCContext)
var contextMapMu sync.Mutex

func pinCtx(ctx *EVMCContext) uintptr {
	contextMapMu.Lock()

	// Find empty slot in the map starting from the map length.
	id := uintptr(len(contextMap))
	for contextMap[id] != nil {
		id++
	}
	contextMap[id] = ctx
	contextMapMu.Unlock()
	return id
}

func unpinCtx(id uintptr) {
	contextMapMu.Lock()
	delete(contextMap, id)
	contextMapMu.Unlock()
}

func getCtx(id uintptr) *EVMCContext {
	contextMapMu.Lock()
	defer contextMapMu.Unlock()
	return contextMap[id]
}

func HashToEvmc(hash common.Hash) C.struct_evm_uint256be {
	return C.struct_evm_uint256be{bytes: *(*[32]C.uint8_t)(unsafe.Pointer(&hash[0]))}
}

func BigToEvmc(i *big.Int) C.struct_evm_uint256be {
	return HashToEvmc(common.BigToHash(i))
}

func assert(cond bool, msg string) {
	if !cond {
		panic(fmt.Sprintf("Assertion failure! %v", msg))
	}
}

type MemoryRef struct {
	ptr *C.uint8_t
	len int
}

func GoByteSlice(data unsafe.Pointer, size C.size_t) []byte {
	var sliceHeader reflect.SliceHeader
	sliceHeader.Data = uintptr(data)
	sliceHeader.Len = int(size)
	sliceHeader.Cap = int(size)
	return *(*[]byte)(unsafe.Pointer(&sliceHeader))
}

//export query
func query(pResult unsafe.Pointer, ctxIdx uintptr, key int32, pArg unsafe.Pointer) {
	// Represent the result memory as Go slice of 32 bytes.
	result := GoByteSlice(pResult, 32)
	// Or as pointer to int64.
	pInt64Result := (*int64)(pResult)

	// Get the execution context.
	ctx := getCtx(ctxIdx)

	switch key {
	case C.EVM_SLOAD:
		arg := *(*[32]byte)(pArg)
		val := ctx.env.StateDB.GetState(ctx.contract.Address(), arg)
		copy(result, val[:])
		// fmt.Printf("EVMJIT SLOAD %x : %x\n", arg, result)
	case C.EVM_ADDRESS:
		addr := ctx.contract.Address()
		copy(result[12:], addr[:])
	case C.EVM_CALLER:
		addr := ctx.contract.Caller()
		copy(result[12:], addr[:])
	case C.EVM_ORIGIN:
		copy(result[12:], ctx.env.Origin[:])
	case C.EVM_GAS_PRICE:
		val := common.BigToHash(ctx.env.GasPrice)
		copy(result, val[:])
	case C.EVM_COINBASE:
		copy(result[12:], ctx.env.Coinbase[:])
	case C.EVM_DIFFICULTY:
		val := common.BigToHash(ctx.env.Difficulty)
		copy(result, val[:])
	case C.EVM_GAS_LIMIT:
		*pInt64Result = ctx.env.GasLimit.Int64()
	case C.EVM_NUMBER:
		*pInt64Result = ctx.env.BlockNumber.Int64()
	case C.EVM_TIMESTAMP:
		*pInt64Result = ctx.env.Time.Int64()
	case C.EVM_CODE_BY_ADDRESS:
		arg := GoByteSlice(pArg, 32)
		var addr common.Address
		copy(addr[:], arg[12:])
		code := ctx.env.StateDB.GetCode(addr)
		pResAsMemRef := (*MemoryRef)(pResult)
		pResAsMemRef.ptr = ptr(code)
		pResAsMemRef.len = len(code)
		// fmt.Printf("EXTCODE %x : %d\n", addr, pResAsMemRef.len)
	case C.EVM_CODE_SIZE:
		arg := GoByteSlice(pArg, 32)
		var addr common.Address
		copy(addr[:], arg[12:])
		pInt64Result := (*int64)(pResult)
		*pInt64Result = int64(ctx.env.StateDB.GetCodeSize(addr))
		// fmt.Printf("EXTCODESIZE %x : %d\n", addr, *pInt64Result)
	case C.EVM_BALANCE:
		arg := GoByteSlice(pArg, 32)
		var addr common.Address
		copy(addr[:], arg[12:])
		balance := ctx.env.StateDB.GetBalance(addr)
		val := common.BigToHash(balance)
		copy(result, val[:])
	case C.EVM_BLOCKHASH:
		n := *(*int64)(pArg)
		b := ctx.env.BlockNumber.Int64()
		a := b - 256
		var hash common.Hash
		if n >= a && n < b {
			hash = ctx.env.GetHash(uint64(n))
		}
		copy(result, hash[:])
		// fmt.Printf("BLOCKHASH %x : %x (%d, %d, %d)\n", result, hash, n, a, b)
	case C.EVM_ACCOUNT_EXISTS:
		arg := GoByteSlice(pArg, 32)
		var addr common.Address
		copy(addr[:], arg[12:])
		eip158 := ctx.env.ChainConfig().IsEIP158(ctx.env.BlockNumber)
		var exist int64
		if eip158 {
			if !ctx.env.StateDB.Empty(addr) {
				exist = 1
			}
		} else if ctx.env.StateDB.Exist(addr) {
			exist = 1
		}
		*pInt64Result = exist
		// fmt.Printf("EXISTS? %x : %v\n", addr, exist)
	case C.EVM_CALL_DEPTH:
		*pInt64Result = int64(ctx.env.depth - 1)

	default:
		panic(fmt.Sprintf("Unhandled EVM-C query %d\n", key))
	}
}

//export update
func update(ctxIdx uintptr, key int32, pArg1 unsafe.Pointer, pArg2 unsafe.Pointer) {
	ctx := getCtx(ctxIdx)

	switch key {
	case C.EVM_SSTORE:
		key := *(*[32]byte)(pArg1)
		newVal := *(*[32]byte)(pArg2)
		oldVal := ctx.env.StateDB.GetState(ctx.contract.Address(), key)
		ctx.env.StateDB.SetState(ctx.contract.Address(), key, newVal)
		if !common.EmptyHash(oldVal) && common.EmptyHash(newVal) {
			ctx.env.StateDB.AddRefund(params.SstoreRefundGas)
		}
		// fmt.Printf("EVMJIT STORE %x : %x [%x, %d]\n", arg1, arg2, ctx.contract.Address(), int(uintptr(pEnv)))
	case C.EVM_LOG:
		dataRef := (*MemoryRef)(pArg1)
		topicsRef := (*MemoryRef)(pArg2)
		data := C.GoBytes(unsafe.Pointer(dataRef.ptr), C.int(dataRef.len))
		// FIXME: Avoid double copy of topics.
		tData := C.GoBytes(unsafe.Pointer(topicsRef.ptr), C.int(topicsRef.len))
		nTopics := topicsRef.len / 32
		topics := make([]common.Hash, nTopics)
		for i := 0; i < nTopics; i++ {
			copy(topics[i][:], tData[i*32:(i+1)*32])
		}
		ctx.env.StateDB.AddLog(&types.Log{
			Address: ctx.contract.Address(),
			Topics:  topics,
			Data:    data,
			BlockNumber: ctx.env.BlockNumber.Uint64(),
		})
	case C.EVM_SELFDESTRUCT:
		arg := GoByteSlice(pArg1, 32)
		db := ctx.env.StateDB
		addr := ctx.contract.Address()
		if !db.HasSuicided(addr) {
			db.AddRefund(params.SuicideRefundGas)
		}
		balance := db.GetBalance(addr)
		beneficiary := common.BytesToAddress(arg[12:])
		db.AddBalance(beneficiary, balance)
		db.Suicide(addr)
	}
}

//export call
func call(
	pCtx unsafe.Pointer,
	kind int32,
	gas int64,
	pAddr unsafe.Pointer,
	pValue unsafe.Pointer,
	pInput unsafe.Pointer,
	inputSize C.size_t,
	pOutput unsafe.Pointer,
	outputSize C.size_t) int64 {

	ctx := getCtx(uintptr(pCtx))
	address := *(*[20]byte)(pAddr)
	value := (*(*common.Hash)(pValue)).Big()
	input := GoByteSlice(pInput, inputSize)
	output := GoByteSlice(pOutput, outputSize)
	bigGas := new(big.Int).SetInt64(gas)

	// TODO: For some reason C.EVM_CALL_FAILURE "constant" is not visible
	// by go linker. This probably should be reported as cgo bug.
	const callFailureFlag int64 = -(1 << 63);

	switch kind {
	case C.EVM_CALL:
		// fmt.Printf("CALL(gas %d, %x)\n", bigGas, address)
		ctx.contract.Gas.SetInt64(0)
		ret, err := ctx.env.Call(ctx.contract, address, input, bigGas, value)
		gasLeft := ctx.contract.Gas.Int64()
		assert(gasLeft <= gas, fmt.Sprintf("%d <= %d", gasLeft, gas))
		// fmt.Printf("Gas left %d\n", gasLeft)
		if err == nil {
			copy(output, ret)
			assert(gasLeft <= gas, fmt.Sprintf("%d <= %d", gasLeft, gas))
			return gasLeft
		}
		return gasLeft | callFailureFlag
	case C.EVM_CALLCODE:
		// fmt.Printf("CALLCODE(gas %d, %x, value %d)\n", bigGas, address, value)
		ctx.contract.Gas.SetInt64(0)
		ret, err := ctx.env.CallCode(ctx.contract, address, input, bigGas, value)
		gasLeft := ctx.contract.Gas.Int64()
		assert(gasLeft <= gas, fmt.Sprintf("%d <= %d", gasLeft, gas))
		// fmt.Printf("Gas left %d\n", gasLeft)
		if err == nil {
			copy(output, ret)
			return gasLeft
		} else {
			// fmt.Printf("Error: %v\n", err)
		}
		return gasLeft | callFailureFlag
	case C.EVM_DELEGATECALL:
		// fmt.Printf("DELEGATECALL(gas %d, %x)\n", bigGas, address)
		ctx.contract.Gas.SetInt64(0)
		ret, err := ctx.env.DelegateCall(ctx.contract, address, input, bigGas)
		gasLeft := ctx.contract.Gas.Int64()
		assert(gasLeft <= gas, fmt.Sprintf("%d <= %d", gasLeft, gas))
		// fmt.Printf("Gas left %d\n", gasLeft)
		if err == nil {
			copy(output, ret)
			return gasLeft
		}
		return gasLeft | callFailureFlag
	case C.EVM_CREATE:
		// fmt.Printf("DELEGATECALL(gas %d, %x)\n", bigGas, address)
		ctx.contract.Gas.SetInt64(0)
		_, addr, err := ctx.env.Create(ctx.contract, input, bigGas, value)
		gasLeft := ctx.contract.Gas.Int64()
		assert(gasLeft <= gas, fmt.Sprintf("%d <= %d", gasLeft, gas))
		if (ctx.env.ChainConfig().IsHomestead(ctx.env.BlockNumber) && err == ErrCodeStoreOutOfGas) ||
			(err != nil && err != ErrCodeStoreOutOfGas) {
			return callFailureFlag
		} else {
			copy(output, addr[:])
			return gasLeft
		}
	}
	return 0
}

func ptr(bytes []byte) *C.uint8_t {
	header := *(*reflect.SliceHeader)(unsafe.Pointer(&bytes))
	return (*C.uint8_t)(unsafe.Pointer(header.Data))
}

func getMode(env *EVM) C.enum_evm_mode {
	n := env.BlockNumber
	if env.ChainConfig().IsEIP158(n) {
		return C.EVM_CLEARING
	}
	if env.ChainConfig().IsEIP150(n) {
		return C.EVM_ANTI_DOS
	}
	if env.ChainConfig().IsHomestead(n) {
		return C.EVM_HOMESTEAD
	}
	return C.EVM_FRONTIER
}

func (evm *EVMJIT) Run(contract *Contract, input []byte) (ret []byte, err error) {
	evm.env.depth++
	defer func() { evm.env.depth-- }()

	if contract.CodeAddr != nil {
		if p := PrecompiledContracts[*contract.CodeAddr]; p != nil {
			return RunPrecompiledContract(p, input, contract)
		}
	}

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	code := contract.Code
	codePtr := (*C.uint8_t)(unsafe.Pointer(&code[0]))
	codeSize := C.size_t(len(code))
	codeHash := HashToEvmc(crypto.Keccak256Hash(code))
	gas := C.int64_t(contract.Gas.Int64())
	inputPtr := ptr(input)
	inputLen := C.size_t(len(input))
	value := BigToEvmc(contract.value)
	mode := getMode(evm.env)
	// fmt.Printf("EVMJIT pre Run (gas %d %d mode: %d, env: %d) %x\n", contract.Gas, gas, mode, env, evm.contract.Address())

	// Create context for this execution.
	ctxId := pinCtx(&EVMCContext{contract, evm.env})

	r := C.evm_execute(evm.jit, unsafe.Pointer(ctxId), mode, codeHash, codePtr, codeSize, gas, inputPtr, inputLen, value)

	unpinCtx(ctxId)

	// fmt.Printf("EVMJIT Run %d %d %x\n", r.code, r.gas_left, evm.contract.Address())
	if r.gas_left > gas {
		panic("OOPS")
	}
	contract.Gas.SetInt64(int64(r.gas_left))
	// fmt.Printf("Gas left: %d\n", contract.Gas)
	output := C.GoBytes(unsafe.Pointer(r.output_data), C.int(r.output_size))

	if r.code != 0 {
		// EVMJIT does not informs about the kind of the EVM expection.
		err = ErrOutOfGas
	}

	C.evm_release_result(&r)
	return output, err
}
