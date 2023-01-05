package whitelist

import (
	"encoding/hex"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/xsleonard/go-merkle"
	"golang.org/x/crypto/sha3"
)

// getRootHash returns the merkle root of the start to end block headers
func getRootHash(blockHeaders []*types.Header) (string, error) {

	length := uint64(len(blockHeaders))
	headers := make([][32]byte, nextPowerOfTwo(length))

	for i := 0; i < len(blockHeaders); i++ {
		blockHeader := blockHeaders[i]
		header := crypto.Keccak256(appendBytes32(
			blockHeader.Number.Bytes(),
			new(big.Int).SetUint64(blockHeader.Time).Bytes(),
			blockHeader.TxHash.Bytes(),
			blockHeader.ReceiptHash.Bytes(),
		))

		var arr [32]byte

		copy(arr[:], header)
		headers[i] = arr
	}

	tree := merkle.NewTreeWithOpts(merkle.TreeOptions{EnableHashSorting: false, DisableHashLeaves: true})
	if err := tree.Generate(convert(headers), sha3.NewLegacyKeccak256()); err != nil {
		return "", err
	}

	root := hex.EncodeToString(tree.Root().Hash)

	return root, nil
}

func appendBytes32(data ...[]byte) []byte {
	var result []byte

	for _, v := range data {
		paddedV := convertTo32(v)
		result = append(result, paddedV[:]...)
	}

	return result
}

func nextPowerOfTwo(n uint64) uint64 {
	if n == 0 {
		return 1
	}
	// http://graphics.stanford.edu/~seander/bithacks.html#RoundUpPowerOf2
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n |= n >> 32
	n++

	return n
}

func convertTo32(input []byte) (output [32]byte) {
	l := len(input)
	if l > 32 || l == 0 {
		return
	}

	copy(output[32-l:], input[:])

	return
}

func convert(input [][32]byte) [][]byte {
	output := make([][]byte, 0, len(input))

	for _, in := range input {
		newInput := make([]byte, len(in[:]))
		copy(newInput, in[:])
		output = append(output, newInput)
	}

	return output
}
