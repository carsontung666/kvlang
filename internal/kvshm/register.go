package kvshm

import "github.com/array2d/kvspace-go"

type engine uint32

const (
	engineArtBump engine = 1
	engineArtBox  engine = 2
	engineHashBox engine = 3
	engineTrieBox engine = 4
)

func init() {
	kvspace.Register("shm-art-bump", func(addr string) kvspace.KVSpace {
		return openNative(addr, engineArtBump)
	})
	kvspace.Register("shm-art-box", func(addr string) kvspace.KVSpace {
		return openNative(addr, engineArtBox)
	})
	kvspace.Register("shm-hash-box", func(addr string) kvspace.KVSpace {
		return openNative(addr, engineHashBox)
	})
	kvspace.Register("shm-trie-box", func(addr string) kvspace.KVSpace {
		return openNative(addr, engineTrieBox)
	})
}
