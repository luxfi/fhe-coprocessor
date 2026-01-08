// Package tfhe provides a wrapper around the TFHE library for FHE operations.
package fhe

import (
	"context"
	"errors"
	"fmt"

	fhe "github.com/luxfi/fhe"
)

// Common errors.
var (
	ErrInvalidCiphertext = errors.New("invalid ciphertext")
	ErrOperationFailed   = errors.New("FHE operation failed")
	ErrKeyNotFound       = errors.New("key not found")
)

// OperationType defines supported FHE operations.
type OperationType uint8

const (
	OpAdd OperationType = iota
	OpSub
	OpMul
	OpNot
	OpAnd
	OpOr
	OpXor
	OpEq
	OpNe
	OpGt
	OpGe
	OpLt
	OpLe
	OpShl
	OpShr
	OpRotl
	OpRotr
	OpMin
	OpMax
	OpNeg
)

// Adapter wraps TFHE operations with context support.
type Adapter struct {
	serverKey *fhe.ServerKey
}

// NewAdapter creates a new TFHE adapter with the given server key.
func NewAdapter(serverKeyBytes []byte) (*Adapter, error) {
	sk, err := fhe.DeserializeServerKey(serverKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("deserialize server key: %w", err)
	}
	return &Adapter{serverKey: sk}, nil
}

// Ciphertext represents an encrypted value.
type Ciphertext struct {
	Data   []byte
	BitLen uint8
}

// Execute performs an FHE operation on one or two ciphertexts.
func (a *Adapter) Execute(ctx context.Context, op OperationType, lhs, rhs *Ciphertext) (*Ciphertext, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if lhs == nil {
		return nil, ErrInvalidCiphertext
	}

	lhsCt, err := fhe.DeserializeCiphertext(lhs.Data)
	if err != nil {
		return nil, fmt.Errorf("deserialize lhs: %w", err)
	}

	var result *fhe.Ciphertext

	switch op {
	case OpNot, OpNeg:
		result, err = a.executeUnary(op, lhsCt)
	default:
		if rhs == nil {
			return nil, ErrInvalidCiphertext
		}
		rhsCt, deserErr := fhe.DeserializeCiphertext(rhs.Data)
		if deserErr != nil {
			return nil, fmt.Errorf("deserialize rhs: %w", deserErr)
		}
		result, err = a.executeBinary(op, lhsCt, rhsCt)
	}

	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOperationFailed, err)
	}

	data, err := result.Serialize()
	if err != nil {
		return nil, fmt.Errorf("serialize result: %w", err)
	}

	return &Ciphertext{Data: data, BitLen: lhs.BitLen}, nil
}

func (a *Adapter) executeUnary(op OperationType, ct *fhe.Ciphertext) (*fhe.Ciphertext, error) {
	switch op {
	case OpNot:
		return a.serverKey.Not(ct)
	case OpNeg:
		return a.serverKey.Neg(ct)
	default:
		return nil, fmt.Errorf("unknown unary operation: %d", op)
	}
}

func (a *Adapter) executeBinary(op OperationType, lhs, rhs *fhe.Ciphertext) (*fhe.Ciphertext, error) {
	switch op {
	case OpAdd:
		return a.serverKey.Add(lhs, rhs)
	case OpSub:
		return a.serverKey.Sub(lhs, rhs)
	case OpMul:
		return a.serverKey.Mul(lhs, rhs)
	case OpAnd:
		return a.serverKey.And(lhs, rhs)
	case OpOr:
		return a.serverKey.Or(lhs, rhs)
	case OpXor:
		return a.serverKey.Xor(lhs, rhs)
	case OpEq:
		return a.serverKey.Eq(lhs, rhs)
	case OpNe:
		return a.serverKey.Ne(lhs, rhs)
	case OpGt:
		return a.serverKey.Gt(lhs, rhs)
	case OpGe:
		return a.serverKey.Ge(lhs, rhs)
	case OpLt:
		return a.serverKey.Lt(lhs, rhs)
	case OpLe:
		return a.serverKey.Le(lhs, rhs)
	case OpShl:
		return a.serverKey.Shl(lhs, rhs)
	case OpShr:
		return a.serverKey.Shr(lhs, rhs)
	case OpRotl:
		return a.serverKey.Rotl(lhs, rhs)
	case OpRotr:
		return a.serverKey.Rotr(lhs, rhs)
	case OpMin:
		return a.serverKey.Min(lhs, rhs)
	case OpMax:
		return a.serverKey.Max(lhs, rhs)
	default:
		return nil, fmt.Errorf("unknown binary operation: %d", op)
	}
}

// Decrypt decrypts a ciphertext using the client key (for testing only).
func Decrypt(clientKeyBytes []byte, ct *Ciphertext) (uint64, error) {
	ck, err := fhe.DeserializeClientKey(clientKeyBytes)
	if err != nil {
		return 0, fmt.Errorf("deserialize client key: %w", err)
	}

	ciphertext, err := fhe.DeserializeCiphertext(ct.Data)
	if err != nil {
		return 0, fmt.Errorf("deserialize ciphertext: %w", err)
	}

	return ck.Decrypt(ciphertext)
}

// Encrypt encrypts a plaintext value (for testing only).
func Encrypt(clientKeyBytes []byte, value uint64, bitLen uint8) (*Ciphertext, error) {
	ck, err := fhe.DeserializeClientKey(clientKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("deserialize client key: %w", err)
	}

	ct, err := ck.Encrypt(value, bitLen)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	data, err := ct.Serialize()
	if err != nil {
		return nil, fmt.Errorf("serialize: %w", err)
	}

	return &Ciphertext{Data: data, BitLen: bitLen}, nil
}
