package tx

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/sha3"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	lru "github.com/hashicorp/golang-lru"
	"github.com/vechain/thor/metric"
	"github.com/vechain/thor/thor"
	"github.com/vechain/thor/vm/evm"
)

const signerCacheSize = 1024

var (
	signerCache, _ = lru.New(signerCacheSize)
	invalidTxID    = thor.BytesToBytes32(math.MaxBig256.Bytes())
)

// Transaction is an immutable tx type.
type Transaction struct {
	body body

	cache struct {
		signingHash atomic.Value
		signer      atomic.Value
		id          atomic.Value
		size        atomic.Value
	}
}

// body describes details of a tx.
type body struct {
	ChainTag     byte
	Nonce        uint64
	BlockRef     uint64
	Clauses      []*Clause
	GasPriceCoef uint8
	Gas          uint64
	DependsOn    *thor.Bytes32 `rlp:"nil"`
	Reserved     []interface{}
	Signature    []byte
}

// ChainTag returns chain tag.
func (t *Transaction) ChainTag() byte {
	return t.body.ChainTag
}

// Nonce returns nonce value.
func (t *Transaction) Nonce() uint64 {
	return t.body.Nonce
}

// BlockRef returns block reference, which is first 8 bytes of block hash.
func (t *Transaction) BlockRef() (br BlockRef) {
	binary.BigEndian.PutUint64(br[:], t.body.BlockRef)
	return
}

// ID returns id of tx.
// ID = hash(signingHash, signer).
// It returns invalidTxID if signer not available.
func (t *Transaction) ID() (id thor.Bytes32) {
	if cached := t.cache.id.Load(); cached != nil {
		return cached.(thor.Bytes32)
	}
	defer func() { t.cache.id.Store(id) }()

	signer, err := t.Signer()
	if err != nil {
		return invalidTxID
	}
	return t.makeID(signer)
}

func (t *Transaction) makeID(signer thor.Address) (id thor.Bytes32) {
	hw := sha3.NewKeccak256()
	hw.Write(t.SigningHash().Bytes())
	hw.Write(signer.Bytes())
	hw.Sum(id[:0])
	return
}

func idToWork(id thor.Bytes32) *big.Int {
	result := new(big.Int).SetBytes(id[:])
	return result.Div(math.MaxBig256, result)
}

// ProvedWork returns proved work of this tx.
// It returns 0, if tx is not signed.
func (t *Transaction) ProvedWork() *big.Int {
	_, err := t.Signer()
	if err != nil {
		return &big.Int{}
	}
	return idToWork(t.ID())
}

// EvaluateWork try to compute work when tx signer assumed.
func (t *Transaction) EvaluateWork(signer thor.Address) *big.Int {
	return idToWork(t.makeID(signer))
}

// SigningHash returns hash of tx excludes signature.
func (t *Transaction) SigningHash() (hash thor.Bytes32) {
	if cached := t.cache.signingHash.Load(); cached != nil {
		return cached.(thor.Bytes32)
	}
	defer func() { t.cache.signingHash.Store(hash) }()

	hw := sha3.NewKeccak256()
	rlp.Encode(hw, []interface{}{
		t.body.ChainTag,
		t.body.Nonce,
		t.body.BlockRef,
		t.body.Clauses,
		t.body.GasPriceCoef,
		t.body.Gas,
		t.body.DependsOn,
		t.body.Reserved,
	})
	hw.Sum(hash[:0])
	return
}

// GasPriceCoef returns gas price coef.
// gas price = bgp + bgp * gpc / 255.
func (t *Transaction) GasPriceCoef() uint8 {
	return t.body.GasPriceCoef
}

// Gas returns gas provision for this tx.
func (t *Transaction) Gas() uint64 {
	return t.body.Gas
}

// Clauses returns caluses in tx.
func (t *Transaction) Clauses() []*Clause {
	return append([]*Clause(nil), t.body.Clauses...)
}

// DependsOn returns depended tx hash.
func (t *Transaction) DependsOn() *thor.Bytes32 {
	if t.body.DependsOn == nil {
		return nil
	}
	cpy := *t.body.DependsOn
	return &cpy
}

// Signature returns signature.
func (t *Transaction) Signature() []byte {
	return append([]byte(nil), t.body.Signature...)
}

// Signer extract signer of tx from signature.
func (t *Transaction) Signer() (signer thor.Address, err error) {
	if cached := t.cache.signer.Load(); cached != nil {
		return cached.(thor.Address), nil
	}
	defer func() {
		if err == nil {
			t.cache.signer.Store(signer)
		}
	}()

	hw := sha3.NewKeccak256()
	rlp.Encode(hw, &t)
	var hash thor.Bytes32
	hw.Sum(hash[:0])

	if v, ok := signerCache.Get(hash); ok {
		return v.(thor.Address), nil
	}
	defer func() {
		if err == nil {
			signerCache.Add(hash, signer)
		}
	}()
	pub, err := crypto.SigToPub(t.SigningHash().Bytes(), t.body.Signature)
	if err != nil {
		return thor.Address{}, err
	}
	signer = thor.Address(crypto.PubkeyToAddress(*pub))
	return
}

// WithSignature create a new tx with signature set.
func (t *Transaction) WithSignature(sig []byte) *Transaction {
	newTx := Transaction{
		body: t.body,
	}
	// copy sig
	newTx.body.Signature = append([]byte(nil), sig...)
	return &newTx
}

// HasReservedFields returns if there're reserved fields.
// Reserved fields are for backward compatibility purpose.
func (t *Transaction) HasReservedFields() bool {
	return len(t.body.Reserved) > 0
}

// EncodeRLP implements rlp.Encoder
func (t *Transaction) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &t.body)
}

// DecodeRLP implements rlp.Decoder
func (t *Transaction) DecodeRLP(s *rlp.Stream) error {
	var body body
	if err := s.Decode(&body); err != nil {
		return err
	}
	*t = Transaction{
		body: body,
	}
	return nil
}

// Size returns size in bytes when RLP encoded.
func (t *Transaction) Size() metric.StorageSize {
	if cached := t.cache.size.Load(); cached != nil {
		return cached.(metric.StorageSize)
	}
	var size metric.StorageSize
	rlp.Encode(&size, t)
	t.cache.size.Store(size)
	return size
}

// IntrinsicGas returns intrinsic gas of tx.
func (t *Transaction) IntrinsicGas() (uint64, error) {
	if len(t.body.Clauses) == 0 {
		return thor.TxGas + thor.ClauseGas, nil
	}

	var total = thor.TxGas
	var overflow bool
	for _, c := range t.body.Clauses {
		gas, err := dataGas(c.body.Data)
		if err != nil {
			return 0, err
		}
		total, overflow = math.SafeAdd(total, gas)
		if overflow {
			return 0, evm.ErrOutOfGas
		}

		var cgas uint64
		if c.body.To == nil {
			// contract creation
			cgas = thor.ClauseGasContractCreation
		} else {
			cgas = thor.ClauseGas
		}

		total, overflow = math.SafeAdd(total, cgas)
		if overflow {
			return 0, evm.ErrOutOfGas
		}
	}
	return total, nil
}

// GasPrice returns gas price.
// gasPrice = baseGasPrice + baseGasPrice * gasPriceCoef / 255
func (t *Transaction) GasPrice(baseGasPrice *big.Int) *big.Int {
	x := big.NewInt(int64(t.body.GasPriceCoef))
	x.Mul(x, baseGasPrice)
	x.Div(x, big.NewInt(int64(math.MaxUint8)))
	return x.Add(x, baseGasPrice)
}

// OverallGasPrice calculate overall gas price.
// overallGasPrice = gasPrice + baseGasPrice * wgas/gas.
func (t *Transaction) OverallGasPrice(baseGasPrice *big.Int, headBlockNum uint32, getBlockID func(uint32) thor.Bytes32) *big.Int {
	gasPrice := t.GasPrice(baseGasPrice)
	if t.measureDelay(headBlockNum, getBlockID) > thor.MaxTxWorkDelay {
		return gasPrice
	}

	work := t.ProvedWork()
	wgas := workToGas(work, headBlockNum)
	if wgas > t.body.Gas {
		wgas = t.body.Gas
	}

	x := new(big.Int).SetUint64(wgas)
	x.Mul(x, baseGasPrice)
	x.Div(x, new(big.Int).SetUint64(t.body.Gas))
	return x.Add(x, gasPrice)
}

// measureDelay measure tx delay count in blocks, according to head block number.
func (t *Transaction) measureDelay(headBlockNum uint32, getBlockID func(uint32) thor.Bytes32) uint32 {
	ref := t.BlockRef()
	refNum := ref.Number()
	if refNum >= headBlockNum {
		return math.MaxUint32
	}

	if headBlockNum-refNum > thor.MaxTxWorkDelay {
		return math.MaxUint32
	}

	id := getBlockID(refNum)
	if bytes.HasPrefix(id[:], ref[:]) {
		return headBlockNum - refNum
	}
	return math.MaxUint32
}

func (t *Transaction) String() string {
	var (
		from      string
		br        BlockRef
		dependsOn string
	)
	signer, err := t.Signer()
	if err != nil {
		from = "N/A"
	} else {
		from = signer.String()
	}

	binary.BigEndian.PutUint64(br[:], t.body.BlockRef)
	if t.body.DependsOn == nil {
		dependsOn = "nil"
	} else {
		dependsOn = t.body.DependsOn.String()
	}

	return fmt.Sprintf(`
	Tx(%v, %v)
	From:			%v
	Clauses:		%v
	GasPriceCoef:	%v
	Gas:			%v
	ChainTag:		%v
	BlockRef:		%v-%x
	DependsOn:		%v
	ReservedFields:	%v
	Nonce:			%v
	Signature:		0x%x
`, t.ID(), t.Size(), from, t.body.Clauses, t.body.GasPriceCoef, t.body.Gas,
		t.body.ChainTag, br.Number(), br[4:], dependsOn, t.body.Reserved, t.body.Nonce, t.body.Signature)
}

// see core.IntrinsicGas
func dataGas(data []byte) (uint64, error) {
	if len(data) == 0 {
		return 0, nil
	}
	var z, nz uint64
	for _, byt := range data {
		if byt == 0 {
			z++
		} else {
			nz++
		}
	}
	zgas, overflow := math.SafeMul(params.TxDataZeroGas, z)
	if overflow {
		return 0, evm.ErrOutOfGas
	}
	nzgas, overflow := math.SafeMul(params.TxDataNonZeroGas, nz)
	if overflow {
		return 0, evm.ErrOutOfGas
	}

	gas, overflow := math.SafeAdd(zgas, nzgas)
	if overflow {
		return 0, evm.ErrOutOfGas
	}
	return gas, nil
}
