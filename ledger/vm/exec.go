package vm

import (
	"math"
	"math/big"
	"time"

	"github.com/thetatoken/theta-protocol-ledger/common"
	"github.com/thetatoken/theta-protocol-ledger/ledger/state"
	"github.com/thetatoken/theta-protocol-ledger/ledger/types"
	"github.com/thetatoken/theta-protocol-ledger/ledger/vm/params"
)

// Execute executes the given smart contract
func Execute(tx *types.SmartContractTx, storeView *state.StoreView) (evmRet common.Bytes,
	contractAddr common.Address, gasUsed uint64, evmErr error) {
	context := Context{
		GasPrice:    tx.GasPrice,
		GasLimit:    tx.GasLimit,
		BlockNumber: new(big.Int).SetUint64(storeView.Height()),
		Time:        new(big.Int).SetInt64(time.Now().Unix()),
		Difficulty:  new(big.Int).SetInt64(0),
	}
	chainConfig := &params.ChainConfig{}
	config := Config{}
	evm := NewEVM(context, storeView, chainConfig, config)

	value := tx.From.Coins.TFuelWei
	if value == nil {
		value = big.NewInt(0)
	}
	gasLimit := tx.GasLimit
	fromAddr := tx.From.Address
	contractAddr = tx.To.Address
	createContract := (contractAddr == common.Address{})

	intrinsicGas, err := calculateIntrinsicGas(tx.Data, createContract)
	if err != nil {
		return common.Bytes{}, common.Address{}, 0, err
	}
	if intrinsicGas > gasLimit {
		return common.Bytes{}, common.Address{}, 0, ErrOutOfGas
	}

	var leftOverGas uint64
	remainingGas := gasLimit - intrinsicGas
	if createContract {
		code := tx.Data
		evmRet, contractAddr, leftOverGas, evmErr = evm.Create(AccountRef(fromAddr), code, remainingGas, value)
	} else {
		input := tx.Data
		evmRet, leftOverGas, evmErr = evm.Call(AccountRef(fromAddr), contractAddr, input, remainingGas, value)
	}

	if leftOverGas > gasLimit { // should not happen
		gasUsed = uint64(0)
	} else {
		gasUsed = gasLimit - leftOverGas
	}

	return evmRet, contractAddr, gasUsed, evmErr
}

// calculateIntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func calculateIntrinsicGas(data []byte, createContract bool) (uint64, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if createContract {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	// Bump the required gas by the amount of transactional data
	if len(data) > 0 {
		// Zero and non-zero bytes are priced differently
		var nz uint64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		// Make sure we don't exceed uint64 for all data combinations
		if (math.MaxUint64-gas)/params.TxDataNonZeroGas < nz {
			return 0, ErrOutOfGas
		}
		gas += nz * params.TxDataNonZeroGas

		z := uint64(len(data)) - nz
		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return 0, ErrOutOfGas
		}
		gas += z * params.TxDataZeroGas
	}
	return gas, nil
}
