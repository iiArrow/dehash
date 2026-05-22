package store

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/vmihailenco/msgpack/v5"

	"dehash/internal/model"
)

// Key prefix byte per hash type.
const (
	PrefixSHA1   byte = 0x01 // 20 raw bytes → 21-byte key
	PrefixSHA256 byte = 0x02 // 32 raw bytes → 33-byte key
	PrefixMD5    byte = 0x03 // 16 raw bytes → 17-byte key
	PrefixCRC32  byte = 0x04 //  4 raw bytes →  5-byte key
)

var ErrNotFound = errors.New("not found")
var ErrInvalidHash = errors.New("invalid hash")

type Store struct {
	db *pebble.DB
}

func Open(path string, readOnly bool) (*Store, error) {
	opts := &pebble.Options{
		ReadOnly: readOnly,
		Levels: []pebble.LevelOptions{
			{
				BlockSize:    32 * 1024,
				FilterPolicy: bloom.FilterPolicy(10),
				FilterType:   pebble.TableFilter,
			},
		},
		// Cache 512 MB of hot blocks in RAM.
		Cache: pebble.NewCache(512 << 20),
	}
	db, err := pebble.Open(path, opts)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the underlying pebble.DB for batch writes during ETL.
func (s *Store) DB() *pebble.DB {
	return s.db
}

// Lookup finds a hash in the store. Returns ErrNotFound if absent.
func (s *Store) Lookup(hash string) (*model.FileDetails, error) {
	key, err := HashToKey(hash)
	if err != nil {
		return nil, err
	}

	val, closer, err := s.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("db get: %w", err)
	}
	defer closer.Close()

	var d model.FileDetails
	if err := msgpack.Unmarshal(val, &d); err != nil {
		return nil, fmt.Errorf("deserialize: %w", err)
	}
	return &d, nil
}

// MakeKey builds a prefixed key from a prefix byte and raw hash bytes.
func MakeKey(prefix byte, raw []byte) []byte {
	key := make([]byte, 1+len(raw))
	key[0] = prefix
	copy(key[1:], raw)
	return key
}

// HashToKey normalises a hex hash string and returns its lookup key.
func HashToKey(hash string) ([]byte, error) {
	h := strings.ToLower(strings.TrimSpace(hash))
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
	var prefix byte
	switch len(raw) {
	case 4:
		prefix = PrefixCRC32
	case 16:
		prefix = PrefixMD5
	case 20:
		prefix = PrefixSHA1
	case 32:
		prefix = PrefixSHA256
	default:
		return nil, fmt.Errorf("%w: unexpected length %d bytes", ErrInvalidHash, len(raw))
	}
	return MakeKey(prefix, raw), nil
}
