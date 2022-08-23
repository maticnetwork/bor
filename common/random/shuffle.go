package random

import (
	"math/rand"
)

func Shuffle[T any](slice []T) {
	r := rand.New(CryptoSource{})

	r.Shuffle(len(slice), func(i, j int) {
		slice[i], slice[j] = slice[j], slice[i]
	})
}

// ShuffleMap returns a shuffled slice of keys to iterate by
func ShuffleMap[K comparable, V any](m map[K]V) []K {
	keys := make([]K, len(m))

	i := 0
	for key := range m {
		keys[i] = key
		i++
	}

	return keys
}