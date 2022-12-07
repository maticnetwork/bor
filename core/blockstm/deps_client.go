package blockstm

import (
	"bytes"
	"fmt"
	"net/http"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

func SetBlockDep(blockNum uint64, dep [][]uint64, baseUrl string) {
	encoded, _ := rlp.EncodeToBytes(dep)

	url := baseUrl + "/?block=" + fmt.Sprint(blockNum)

	_, err := http.Post(url, "application/octet-stream", bytes.NewBuffer(encoded))

	if err != nil {
		log.Warn("Failed to set block dependency", "block", blockNum, "err", err)
	}
}

func GetBlockDep(blockNum uint64, baseUrl string) [][]uint64 {
	url := baseUrl + "/?block=" + fmt.Sprint(blockNum)

	resp, err := http.Get(url)

	if err != nil {
		log.Warn("Failed to get block dependency", "block", blockNum, "err", err)
		return [][]uint64{}
	}

	defer resp.Body.Close()

	dep := [][]uint64{}
	rlp.Decode(resp.Body, &dep)

	return dep
}
