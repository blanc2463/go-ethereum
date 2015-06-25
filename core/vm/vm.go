package vm

import (
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

var t int

// Vm implements VirtualMachine
type Vm struct {
	env Environment

	err error
	// For logging
	debug bool

	BreakPoints []int64
	Stepping    bool
	Fn          string

	Recoverable bool

	// Will be called before the vm returns
	After func(*Context, error)
}

// New returns a new Virtual Machine
func New(env Environment) *Vm {
	return &Vm{env: env, debug: Debug, Recoverable: true}
}

// Run loops and evaluates the contract's code with the given input data
func (self *Vm) Run(context *Context, input []byte) (ret []byte, err error) {
	self.env.SetDepth(self.env.Depth() + 1)
	defer self.env.SetDepth(self.env.Depth() - 1)
	defer func() {
		if r := recover(); r != nil {
			StdErrFormat(self.env.StructLogs())
			ret = nil
			err = fmt.Errorf("%v", r)
		}
	}()

	var (
		caller = context.caller
		code   = context.Code
		value  = context.value
		price  = context.Price

		op       OpCode                 // current opcode
		codehash = crypto.FnvHash(code) // codehash is used when doing jump dest caching
		mem      = NewMemory()          // bound memory
		stack    = newstack()           // local stack
		statedb  = self.env.State()     // current state
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC to be uint256. Pratically much less so feasible.
		pc        = uint64(0)  // program counter
		ppc       = uint64(0)  // previous program counter
		csegment  *segment     // current code segment
		csegments codeSegments // program segments
		nopay     uint64       = 0

		addop = func(op operation) {
			if csegment != nil && !DisableSegmentation {
				csegment.ops = append(csegment.ops, op)
			}
		}

		// jump evaluates and checks whether the given jump destination is a valid one
		// if valid move the `pc` otherwise return an error.
		jump = func(from uint64, to *big.Int) error {
			if !context.jumpdests.has(codehash, code, to) {
				nop := context.GetOp(to.Uint64())
				return fmt.Errorf("invalid jump destination (%v) %v", nop, to)
			}

			pc = to.Uint64()

			return nil
		}

		newMemSize *big.Int
		cost       *big.Int
	)
	if c, ok := segments.Get(codehash); ok {
		csegments = c.(codeSegments)
	} else {
		csegments = make(codeSegments)
		segments.Add(codehash, csegments)
	}

	// User defer pattern to check for an error and, based on the error being nil or not, use all gas and return.
	defer func() {
		if err != nil {
			self.log(pc, op, context.Gas, cost, mem, stack, context, err)

			// In case of a VM exception (known exceptions) all gas consumed (panics NOT included).
			context.UseGas(context.Gas)

			ret = context.Return(nil)
		}

		if !DisableSegmentation && csegment != nil && csegment.cend == 0 {
			if err != nil {
				// delete this tracking segment. We couldn't fully determine the stats
				delete(csegments, csegment.cstart)
			} else {
				csegment.cend = pc
				csegment.ssize = int(math.Abs(math.Min(float64(stack.len()-csegment.ssize), 0)))
			}
		}
	}()

	if context.CodeAddr != nil {
		if p := Precompiled[context.CodeAddr.Str()]; p != nil {
			return self.RunPrecompiled(p, input, context)
		}
	}

	// Don't bother with the execution if there's no code.
	if len(code) == 0 {
		return context.Return(nil), nil
	}

	for {
		cost = new(big.Int)
		// Get the current operation location of pc
		op = context.GetOp(pc)
		if !DisableSegmentation {
			if isDynamic(op) {
				//fmt.Println("encountered dyn")
				if csegment != nil && csegment.cend == 0 {
					//fmt.Println("closing segment with", op, "@", ppc, csegment.gas)
					// mark end of segment bound
					csegment.cend = ppc
					// set the next op for continuation
					csegment.next = pc
					// write out the required required elements on the stack. i.e. the amount of items
					// required outside of the segment itself.
					// 0: P, 1
					// 1: J, 2
					// 2: D      -+
					// 3: P, 2    |- M,0 = M,0 pushes 1. Requires 2. 1 outside bounds of M,0
					// 4: ADD    -+
					// 5: J, 2
					csegment.ssize = int(math.Abs(math.Min(float64(stack.len()-csegment.ssize), 0)))
				}
				csegment = nil
				nopay = 0
			} else if csegment == nil && pc != 0 {
				// check if a segment is available to us and set it.
				if seg, ok := csegments[pc]; ok {
					//fmt.Println(pc, "using seg", seg)
					nopay = seg.cend

					// make sure there's enough stack items to complete the segment
					if err := stack.require(seg.ssize); err != nil {
						return context.Return(nil), err
					}

					cost.Set(seg.gas)
					// use required gas for this segment
					if !context.UseGas(seg.gas) {
						context.UseGas(context.Gas)

						return context.Return(nil), OutOfGasError{}
					}
					csegment = seg

					/*
						for _, operation := range seg.ops {
							operation.fn(stack, operation.data)
						}

						pc = seg.next
						continue
					*/
				} else {
					//fmt.Println("creating", pc)
					// allocate a new segment for tracking information
					csegment = &segment{pc, 0, 0, 0, new(big.Int), stack.len(), nil}
					csegments[pc] = csegment
					nopay = 0
				}
			}
		}
		// set previous program counter
		ppc = pc

		//fmt.Println(pc, nopay, pc > nopay, pc == 0)
		if pc > nopay || pc == 0 || DisableSegmentation {
			// calculate the new memory size and gas price for the current executing opcode
			newMemSize, cost, err = self.calculateGasAndSize(context, caller, op, statedb, mem, stack)
			if err != nil {
				return nil, err
			}

			if csegment != nil {
				csegment.gas.Add(csegment.gas, cost)
			}

			// Use the calculated gas. When insufficient gas is present, use all gas and return an
			// Out Of Gas error
			if !context.UseGas(cost) {
				context.UseGas(context.Gas)

				return context.Return(nil), OutOfGasError{}
			}

			// Resize the memory calculated previously
			mem.Resize(newMemSize.Uint64())

		}
		// Add a log message
		self.log(pc, op, context.Gas, cost, mem, stack, context, nil)

		// The base for all big integer arithmetic
		base := new(big.Int)

		switch op {
		case ADD:
			x, y := stack.pop(), stack.pop()

			base.Add(x, y)

			U256(base)

			addop(operation{opAdd, nil})

			// pop result back on the stack
			stack.push(base)
		case SUB:
			x, y := stack.pop(), stack.pop()

			base.Sub(x, y)

			U256(base)

			// pop result back on the stack
			stack.push(base)
		case MUL:
			x, y := stack.pop(), stack.pop()

			res := new(big.Int).Mul(x, y)

			U256(res)

			// pop result back on the stack
			stack.push(res)
		case DIV:
			x, y := stack.pop(), stack.pop()

			if y.Cmp(common.Big0) != 0 {
				base.Div(x, y)
			}

			U256(base)

			// pop result back on the stack
			stack.push(base)
		case SDIV:
			x, y := S256(stack.pop()), S256(stack.pop())

			if y.Cmp(common.Big0) == 0 {
				base.Set(common.Big0)
			} else {
				n := new(big.Int)
				if new(big.Int).Mul(x, y).Cmp(common.Big0) < 0 {
					n.SetInt64(-1)
				} else {
					n.SetInt64(1)
				}

				base.Div(x.Abs(x), y.Abs(y)).Mul(base, n)

				U256(base)
			}

			stack.push(base)
		case MOD:
			x, y := stack.pop(), stack.pop()

			if y.Cmp(common.Big0) == 0 {
				base.Set(common.Big0)
			} else {
				base.Mod(x, y)
			}

			U256(base)

			stack.push(base)
		case SMOD:
			x, y := S256(stack.pop()), S256(stack.pop())

			if y.Cmp(common.Big0) == 0 {
				base.Set(common.Big0)
			} else {
				n := new(big.Int)
				if x.Cmp(common.Big0) < 0 {
					n.SetInt64(-1)
				} else {
					n.SetInt64(1)
				}

				base.Mod(x.Abs(x), y.Abs(y)).Mul(base, n)

				U256(base)
			}

			stack.push(base)

		case EXP:
			x, y := stack.pop(), stack.pop()

			base.Exp(x, y, Pow256)

			U256(base)

			stack.push(base)

			addop(operation{opExp, nil})
		case SIGNEXTEND:
			back := stack.pop()
			if back.Cmp(big.NewInt(31)) < 0 {
				bit := uint(back.Uint64()*8 + 7)
				num := stack.pop()
				mask := new(big.Int).Lsh(common.Big1, bit)
				mask.Sub(mask, common.Big1)
				if common.BitTest(num, int(bit)) {
					num.Or(num, mask.Not(mask))
				} else {
					num.And(num, mask)
				}

				num = U256(num)

				stack.push(num)
			}
		case NOT:
			stack.push(U256(new(big.Int).Not(stack.pop())))
		case LT:
			x, y := stack.pop(), stack.pop()

			// x < y
			if x.Cmp(y) < 0 {
				stack.push(common.BigTrue)
			} else {
				stack.push(common.BigFalse)
			}
		case GT:
			x, y := stack.pop(), stack.pop()

			// x > y
			if x.Cmp(y) > 0 {
				stack.push(common.BigTrue)
			} else {
				stack.push(common.BigFalse)
			}

		case SLT:
			x, y := S256(stack.pop()), S256(stack.pop())

			// x < y
			if x.Cmp(S256(y)) < 0 {
				stack.push(common.BigTrue)
			} else {
				stack.push(common.BigFalse)
			}
		case SGT:
			x, y := S256(stack.pop()), S256(stack.pop())

			// x > y
			if x.Cmp(y) > 0 {
				stack.push(common.BigTrue)
			} else {
				stack.push(common.BigFalse)
			}

		case EQ:
			x, y := stack.pop(), stack.pop()

			// x == y
			if x.Cmp(y) == 0 {
				stack.push(common.BigTrue)
			} else {
				stack.push(common.BigFalse)
			}
		case ISZERO:
			x := stack.pop()
			if x.Cmp(common.BigFalse) > 0 {
				stack.push(common.BigFalse)
			} else {
				stack.push(common.BigTrue)
			}

		case AND:
			x, y := stack.pop(), stack.pop()

			stack.push(base.And(x, y))
		case OR:
			x, y := stack.pop(), stack.pop()

			stack.push(base.Or(x, y))
		case XOR:
			x, y := stack.pop(), stack.pop()

			stack.push(base.Xor(x, y))
		case BYTE:
			th, val := stack.pop(), stack.pop()

			if th.Cmp(big.NewInt(32)) < 0 {
				byt := big.NewInt(int64(common.LeftPadBytes(val.Bytes(), 32)[th.Int64()]))

				base.Set(byt)
			} else {
				base.Set(common.BigFalse)
			}

			stack.push(base)
		case ADDMOD:
			x := stack.pop()
			y := stack.pop()
			z := stack.pop()

			if z.Cmp(Zero) > 0 {
				add := new(big.Int).Add(x, y)
				base.Mod(add, z)

				base = U256(base)
			}

			stack.push(base)
		case MULMOD:
			x := stack.pop()
			y := stack.pop()
			z := stack.pop()

			if z.Cmp(Zero) > 0 {
				mul := new(big.Int).Mul(x, y)
				base.Mod(mul, z)

				U256(base)
			}

			stack.push(base)

		case SHA3:
			offset, size := stack.pop(), stack.pop()
			data := crypto.Sha3(mem.GetPtr(offset.Int64(), size.Int64()))

			stack.push(common.BigD(data))

		case ADDRESS:
			stack.push(common.Bytes2Big(context.Address().Bytes()))

		case BALANCE:
			addr := common.BigToAddress(stack.pop())
			balance := statedb.GetBalance(addr)

			stack.push(balance)

		case ORIGIN:
			origin := self.env.Origin()

			stack.push(origin.Big())

		case CALLER:
			caller := context.caller.Address()
			stack.push(common.Bytes2Big(caller.Bytes()))

		case CALLVALUE:
			stack.push(value)

		case CALLDATALOAD:
			data := getData(input, stack.pop(), common.Big32)

			stack.push(common.Bytes2Big(data))
		case CALLDATASIZE:
			l := int64(len(input))
			stack.push(big.NewInt(l))

		case CALLDATACOPY:
			var (
				mOff = stack.pop()
				cOff = stack.pop()
				l    = stack.pop()
			)
			data := getData(input, cOff, l)

			mem.Set(mOff.Uint64(), l.Uint64(), data)

		case CODESIZE, EXTCODESIZE:
			var code []byte
			if op == EXTCODESIZE {
				addr := common.BigToAddress(stack.pop())

				code = statedb.GetCode(addr)
			} else {
				code = context.Code
			}

			l := big.NewInt(int64(len(code)))
			stack.push(l)

		case CODECOPY, EXTCODECOPY:
			var code []byte
			if op == EXTCODECOPY {
				addr := common.BigToAddress(stack.pop())
				code = statedb.GetCode(addr)
			} else {
				code = context.Code
			}

			var (
				mOff = stack.pop()
				cOff = stack.pop()
				l    = stack.pop()
			)

			codeCopy := getData(code, cOff, l)

			mem.Set(mOff.Uint64(), l.Uint64(), codeCopy)

		case GASPRICE:
			stack.push(context.Price)

		case BLOCKHASH:
			num := stack.pop()

			n := new(big.Int).Sub(self.env.BlockNumber(), common.Big257)
			if num.Cmp(n) > 0 && num.Cmp(self.env.BlockNumber()) < 0 {
				stack.push(self.env.GetHash(num.Uint64()).Big())
			} else {
				stack.push(common.Big0)
			}

		case COINBASE:
			coinbase := self.env.Coinbase()

			stack.push(coinbase.Big())

		case TIMESTAMP:
			time := self.env.Time()

			stack.push(big.NewInt(time))

		case NUMBER:
			number := self.env.BlockNumber()

			stack.push(U256(number))

		case DIFFICULTY:
			difficulty := self.env.Difficulty()

			stack.push(difficulty)

		case GASLIMIT:

			stack.push(self.env.GasLimit())

		case PUSH1, PUSH2, PUSH3, PUSH4, PUSH5, PUSH6, PUSH7, PUSH8, PUSH9, PUSH10, PUSH11, PUSH12, PUSH13, PUSH14, PUSH15, PUSH16, PUSH17, PUSH18, PUSH19, PUSH20, PUSH21, PUSH22, PUSH23, PUSH24, PUSH25, PUSH26, PUSH27, PUSH28, PUSH29, PUSH30, PUSH31, PUSH32:
			size := uint64(op - PUSH1 + 1)
			byts := getData(code, new(big.Int).SetUint64(pc+1), new(big.Int).SetUint64(size))

			addop(operation{opPush, byts})

			// push value to stack
			stack.push(common.Bytes2Big(byts))
			pc += size

		case POP:
			stack.pop()
		case DUP1, DUP2, DUP3, DUP4, DUP5, DUP6, DUP7, DUP8, DUP9, DUP10, DUP11, DUP12, DUP13, DUP14, DUP15, DUP16:
			n := int(op - DUP1 + 1)
			stack.dup(n)

		case SWAP1, SWAP2, SWAP3, SWAP4, SWAP5, SWAP6, SWAP7, SWAP8, SWAP9, SWAP10, SWAP11, SWAP12, SWAP13, SWAP14, SWAP15, SWAP16:
			n := int(op - SWAP1 + 2)
			stack.swap(n)

		case LOG0, LOG1, LOG2, LOG3, LOG4:
			n := int(op - LOG0)
			topics := make([]common.Hash, n)
			mStart, mSize := stack.pop(), stack.pop()
			for i := 0; i < n; i++ {
				topics[i] = common.BigToHash(stack.pop())
			}

			data := mem.Get(mStart.Int64(), mSize.Int64())
			log := state.NewLog(context.Address(), topics, data, self.env.BlockNumber().Uint64())
			self.env.AddLog(log)

		case MLOAD:
			offset := stack.pop()
			val := common.BigD(mem.GetPtr(offset.Int64(), 32))
			stack.push(val)

		case MSTORE:
			// pop value of the stack
			mStart, val := stack.pop(), stack.pop()
			mem.Set(mStart.Uint64(), 32, common.BigToBytes(val, 256))

		case MSTORE8:
			off, val := stack.pop().Int64(), stack.pop().Int64()

			mem.store[off] = byte(val & 0xff)

		case SLOAD:
			loc := common.BigToHash(stack.pop())
			val := statedb.GetState(context.Address(), loc).Big()
			stack.push(val)

		case SSTORE:
			loc := common.BigToHash(stack.pop())
			val := stack.pop()

			statedb.SetState(context.Address(), loc, common.BigToHash(val))

		case JUMP:
			if err := jump(pc, stack.pop()); err != nil {
				return nil, err
			}

			continue
		case JUMPI:
			pos, cond := stack.pop(), stack.pop()

			if cond.Cmp(common.BigTrue) >= 0 {
				if err := jump(pc, pos); err != nil {
					return nil, err
				}

				continue
			}

		case JUMPDEST:
		case PC:
			stack.push(new(big.Int).SetUint64(pc))
		case MSIZE:
			stack.push(big.NewInt(int64(mem.Len())))
		case GAS:
			if t > 1 {
				//return context.Return(nil), nil
			}
			t++
			stack.push(context.Gas)

		case CREATE:

			var (
				value        = stack.pop()
				offset, size = stack.pop(), stack.pop()
				input        = mem.Get(offset.Int64(), size.Int64())
				gas          = new(big.Int).Set(context.Gas)
				addr         common.Address
			)

			context.UseGas(context.Gas)
			ret, suberr, ref := self.env.Create(context, input, gas, price, value)
			if suberr != nil {
				stack.push(common.BigFalse)

			} else {
				// gas < len(ret) * CreateDataGas == NO_CODE
				dataGas := big.NewInt(int64(len(ret)))
				dataGas.Mul(dataGas, params.CreateDataGas)
				if context.UseGas(dataGas) {
					ref.SetCode(ret)
				}
				addr = ref.Address()

				stack.push(addr.Big())

			}

		case CALL, CALLCODE:
			gas := stack.pop()
			// pop gas and value of the stack.
			addr, value := stack.pop(), stack.pop()
			value = U256(value)
			// pop input size and offset
			inOffset, inSize := stack.pop(), stack.pop()
			// pop return size and offset
			retOffset, retSize := stack.pop(), stack.pop()

			address := common.BigToAddress(addr)

			// Get the arguments from the memory
			args := mem.Get(inOffset.Int64(), inSize.Int64())

			if len(value.Bytes()) > 0 {
				gas.Add(gas, params.CallStipend)
			}

			var (
				ret []byte
				err error
			)
			if op == CALLCODE {
				ret, err = self.env.CallCode(context, address, args, gas, price, value)
			} else {
				ret, err = self.env.Call(context, address, args, gas, price, value)
			}

			if err != nil {
				stack.push(common.BigFalse)

			} else {
				stack.push(common.BigTrue)

				mem.Set(retOffset.Uint64(), retSize.Uint64(), ret)
			}

		case RETURN:
			offset, size := stack.pop(), stack.pop()
			ret := mem.GetPtr(offset.Int64(), size.Int64())

			return context.Return(ret), nil
		case SUICIDE:
			receiver := statedb.GetOrNewStateObject(common.BigToAddress(stack.pop()))
			balance := statedb.GetBalance(context.Address())

			receiver.AddBalance(balance)

			statedb.Delete(context.Address())

			fallthrough
		case STOP: // Stop the context

			return context.Return(nil), nil
		default:

			return nil, fmt.Errorf("Invalid opcode %x", op)
		}

		pc++

	}
}

// calculateGasAndSize calculates the required given the opcode and stack items calculates the new memorysize for
// the operation. This does not reduce gas or resizes the memory.
func (self *Vm) calculateGasAndSize(context *Context, caller ContextRef, op OpCode, statedb *state.StateDB, mem *Memory, stack *stack) (*big.Int, *big.Int, error) {
	var (
		gas                 = new(big.Int)
		newMemSize *big.Int = new(big.Int)
	)
	err := baseCheck(op, stack, gas)
	if err != nil {
		return nil, nil, err
	}

	// stack Check, memory resize & gas phase
	switch op {
	case SWAP1, SWAP2, SWAP3, SWAP4, SWAP5, SWAP6, SWAP7, SWAP8, SWAP9, SWAP10, SWAP11, SWAP12, SWAP13, SWAP14, SWAP15, SWAP16:
		n := int(op - SWAP1 + 2)
		err := stack.require(n)
		if err != nil {
			return nil, nil, err
		}
		gas.Set(GasFastestStep)
	case DUP1, DUP2, DUP3, DUP4, DUP5, DUP6, DUP7, DUP8, DUP9, DUP10, DUP11, DUP12, DUP13, DUP14, DUP15, DUP16:
		n := int(op - DUP1 + 1)
		err := stack.require(n)
		if err != nil {
			return nil, nil, err
		}
		gas.Set(GasFastestStep)
	case LOG0, LOG1, LOG2, LOG3, LOG4:
		n := int(op - LOG0)
		err := stack.require(n + 2)
		if err != nil {
			return nil, nil, err
		}

		mSize, mStart := stack.data[stack.len()-2], stack.data[stack.len()-1]

		gas.Add(gas, params.LogGas)
		gas.Add(gas, new(big.Int).Mul(big.NewInt(int64(n)), params.LogTopicGas))
		gas.Add(gas, new(big.Int).Mul(mSize, params.LogDataGas))

		newMemSize = calcMemSize(mStart, mSize)
	case EXP:
		gas.Add(gas, new(big.Int).Mul(big.NewInt(int64(len(stack.data[stack.len()-2].Bytes()))), params.ExpByteGas))
	case SSTORE:
		err := stack.require(2)
		if err != nil {
			return nil, nil, err
		}

		var g *big.Int
		y, x := stack.data[stack.len()-2], stack.data[stack.len()-1]
		val := statedb.GetState(context.Address(), common.BigToHash(x))

		// This checks for 3 scenario's and calculates gas accordingly
		// 1. From a zero-value address to a non-zero value         (NEW VALUE)
		// 2. From a non-zero value address to a zero-value address (DELETE)
		// 3. From a nen-zero to a non-zero                         (CHANGE)
		if common.EmptyHash(val) && !common.EmptyHash(common.BigToHash(y)) {
			// 0 => non 0
			g = params.SstoreSetGas
		} else if !common.EmptyHash(val) && common.EmptyHash(common.BigToHash(y)) {
			statedb.Refund(params.SstoreRefundGas)

			g = params.SstoreClearGas
		} else {
			// non 0 => non 0 (or 0 => 0)
			g = params.SstoreClearGas
		}
		gas.Set(g)
	case SUICIDE:
		if !statedb.IsDeleted(context.Address()) {
			statedb.Refund(params.SuicideRefundGas)
		}
	case MLOAD:
		newMemSize = calcMemSize(stack.peek(), u256(32))
	case MSTORE8:
		newMemSize = calcMemSize(stack.peek(), u256(1))
	case MSTORE:
		newMemSize = calcMemSize(stack.peek(), u256(32))
	case RETURN:
		newMemSize = calcMemSize(stack.peek(), stack.data[stack.len()-2])
	case SHA3:
		newMemSize = calcMemSize(stack.peek(), stack.data[stack.len()-2])

		words := toWordSize(stack.data[stack.len()-2])
		gas.Add(gas, words.Mul(words, params.Sha3WordGas))
	case CALLDATACOPY:
		newMemSize = calcMemSize(stack.peek(), stack.data[stack.len()-3])

		words := toWordSize(stack.data[stack.len()-3])
		gas.Add(gas, words.Mul(words, params.CopyGas))
	case CODECOPY:
		newMemSize = calcMemSize(stack.peek(), stack.data[stack.len()-3])

		words := toWordSize(stack.data[stack.len()-3])
		gas.Add(gas, words.Mul(words, params.CopyGas))
	case EXTCODECOPY:
		newMemSize = calcMemSize(stack.data[stack.len()-2], stack.data[stack.len()-4])

		words := toWordSize(stack.data[stack.len()-4])
		gas.Add(gas, words.Mul(words, params.CopyGas))

	case CREATE:
		newMemSize = calcMemSize(stack.data[stack.len()-2], stack.data[stack.len()-3])
	case CALL, CALLCODE:
		gas.Add(gas, stack.data[stack.len()-1])

		if op == CALL {
			if self.env.State().GetStateObject(common.BigToAddress(stack.data[stack.len()-2])) == nil {
				gas.Add(gas, params.CallNewAccountGas)
			}
		}

		if len(stack.data[stack.len()-3].Bytes()) > 0 {
			gas.Add(gas, params.CallValueTransferGas)
		}

		x := calcMemSize(stack.data[stack.len()-6], stack.data[stack.len()-7])
		y := calcMemSize(stack.data[stack.len()-4], stack.data[stack.len()-5])

		newMemSize = common.BigMax(x, y)
	}

	if newMemSize.Cmp(common.Big0) > 0 {
		newMemSizeWords := toWordSize(newMemSize)
		newMemSize.Mul(newMemSizeWords, u256(32))

		if newMemSize.Cmp(u256(int64(mem.Len()))) > 0 {
			oldSize := toWordSize(big.NewInt(int64(mem.Len())))
			pow := new(big.Int).Exp(oldSize, common.Big2, Zero)
			linCoef := new(big.Int).Mul(oldSize, params.MemoryGas)
			quadCoef := new(big.Int).Div(pow, params.QuadCoeffDiv)
			oldTotalFee := new(big.Int).Add(linCoef, quadCoef)

			pow.Exp(newMemSizeWords, common.Big2, Zero)
			linCoef = new(big.Int).Mul(newMemSizeWords, params.MemoryGas)
			quadCoef = new(big.Int).Div(pow, params.QuadCoeffDiv)
			newTotalFee := new(big.Int).Add(linCoef, quadCoef)

			fee := new(big.Int).Sub(newTotalFee, oldTotalFee)
			gas.Add(gas, fee)
		}
	}

	return newMemSize, gas, nil
}

// RunPrecompile runs and evaluate the output of a precompiled contract defined in contracts.go
func (self *Vm) RunPrecompiled(p *PrecompiledAccount, input []byte, context *Context) (ret []byte, err error) {
	gas := p.Gas(len(input))
	if context.UseGas(gas) {
		ret = p.Call(input)

		return context.Return(ret), nil
	} else {
		return nil, OutOfGasError{}
	}
}

// log emits a log event to the environment for each opcode encountered. This is not to be confused with the
// LOG* opcode.
func (self *Vm) log(pc uint64, op OpCode, gas, cost *big.Int, memory *Memory, stack *stack, context *Context, err error) {
	if Debug {
		mem := make([]byte, len(memory.Data()))
		copy(mem, memory.Data())
		stck := make([]*big.Int, len(stack.Data()))
		for i, item := range stack.Data() {
			stck[i] = new(big.Int).Set(item)
		}

		object := context.self.(*state.StateObject)
		storage := make(map[common.Hash][]byte)
		object.EachStorage(func(k, v []byte) {
			storage[common.BytesToHash(k)] = v
		})

		self.env.AddStructLog(StructLog{pc, op, new(big.Int).Set(gas), new(big.Int).Set(cost), mem, stck, storage, err})
	}
}

// Environment returns the current workable state of the VM
func (self *Vm) Env() Environment {
	return self.env
}
