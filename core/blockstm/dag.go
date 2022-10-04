package blockstm

import (
	"fmt"
	"sort"
	"strings"

	"github.com/heimdalr/dag"

	"github.com/ethereum/go-ethereum/log"
)

type DAG struct {
	*dag.DAG
}

func HasReadDep(txFrom TxnOutput, txTo TxnInput) bool {
	reads := make(map[Key]bool)

	for _, v := range txTo {
		reads[v.Path] = true
	}

	for _, rd := range txFrom {
		if _, ok := reads[rd.Path]; ok {
			return true
		}
	}

	return false
}

func BuildDAG(deps TxnInputOutput) (d DAG) {
	d = DAG{dag.NewDAG()}
	ids := make(map[int]string)

	for i := len(deps.inputs) - 1; i > 0; i-- {
		txTo := deps.inputs[i]

		var txToId string

		if _, ok := ids[i]; ok {
			txToId = ids[i]
		} else {
			txToId, _ = d.AddVertex(i)
			ids[i] = txToId
		}

		for j := i - 1; j >= 0; j-- {
			txFrom := deps.allOutputs[j]

			if HasReadDep(txFrom, txTo) {
				var txFromId string
				if _, ok := ids[j]; ok {
					txFromId = ids[j]
				} else {
					txFromId, _ = d.AddVertex(j)
					ids[j] = txFromId
				}

				err := d.AddEdge(txFromId, txToId)
				if err != nil {
					log.Warn("Failed to add edge", "from", txFromId, "to", txToId, "err", err)
				}

				break // once we add a 'backward' dep we can't execute before that transaction so no need to proceed
			}
		}
	}

	return
}

func contains(s []int, e int) (bool, int) {
	for i, a := range s {
		if a == e {
			return true, i
		}
	}
	return false, -1
}

func removeIndex(s []int, index int) []int {
	return append(s[:index], s[index+1:]...)
}

func findAndRemove(s []int, e int) []int {
	boool, ind := contains(s, e)
	if boool {
		return removeIndex(s, ind)
	}

	return s
}

func GetDep(deps TxnInputOutput) map[int][]int {
	dependencies := map[int][]int{}

	for i := len(deps.inputs) - 1; i > 0; i-- {
		txTo := deps.inputs[i]

		for j := i - 1; j >= 0; j-- {
			txFrom := deps.allOutputs[j]

			if HasReadDep(txFrom, txTo) {
				dependencies[i] = append(dependencies[i], j)

				// for dependencies of txIdx k (where k > i)
				for k := i + 1; k <= len(deps.inputs)-1; k++ {
					// if dependencies[k] contains i & j
					boool1, _ := contains(dependencies[k], i)
					boool2, _ := contains(dependencies[k], j)

					if boool1 && boool2 {
						// remove j from dependencies[k]
						dependencies[k] = findAndRemove(dependencies[k], j)
					}
				}
			}
		}
	}

	return dependencies
}

func (d DAG) Report(out func(string)) {
	roots := make([]int, 0)
	rootIds := make([]string, 0)
	rootIdMap := make(map[int]string, len(d.GetRoots()))

	for k, i := range d.GetRoots() {
		roots = append(roots, i.(int))
		rootIdMap[i.(int)] = k
	}

	sort.Ints(roots)

	for _, i := range roots {
		rootIds = append(rootIds, rootIdMap[i])
	}

	fmt.Println(roots)

	makeStrs := func(ints []int) (ret []string) {
		for _, v := range ints {
			ret = append(ret, fmt.Sprint(v))
		}

		return
	}

	maxDesc := 0
	maxDeps := 0
	totalDeps := 0

	for k, v := range roots {
		ids := []int{v}
		desc, _ := d.GetDescendants(rootIds[k])

		for _, i := range desc {
			ids = append(ids, i.(int))
		}

		sort.Ints(ids)
		out(fmt.Sprintf("(%v) %v", len(ids), strings.Join(makeStrs(ids), "->")))

		if len(desc) > maxDesc {
			maxDesc = len(desc)
		}
	}

	numTx := len(d.DAG.GetVertices())
	out(fmt.Sprintf("max chain length: %v of %v (%v%%)", maxDesc+1, numTx,
		fmt.Sprintf("%.1f", float64(maxDesc+1)*100.0/float64(numTx))))
	out(fmt.Sprintf("max dep count: %v of %v (%v%%)", maxDeps, totalDeps,
		fmt.Sprintf("%.1f", float64(maxDeps)*100.0/float64(totalDeps))))
}
