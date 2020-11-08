package sniper

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/recoilme/sortedset"
	"github.com/spaolacci/murmur3"
	"github.com/tidwall/interval"
)

const dirMode = 0755
const fileMode = 0644
const sizeHead = 12
const deleted = 42 // flag for removed, tribute 2 dbf

//ErrCollision -  must not happen
var ErrCollision = errors.New("Error, hash collision")

// ErrFormat unexpected file format
var ErrFormat = errors.New("Error, unexpected file format")

// ErrNotFound key not found error
var ErrNotFound = errors.New("Error, key not found")

var counters sync.Map
var mutex = &sync.RWMutex{} //global mutex for counters and so on
//var chunkColCnt uint32      //chunks for collisions resolving

var expirechunk = 0 // numbeg chunk for next expiration

// Store struct
// data in store sharded by chunks
type Store struct {
	sync.RWMutex
	chunksCnt      int
	chunks         []chunk
	chunkColCnt    int
	dir            string
	syncInterval   time.Duration
	iv             interval.Interval
	expireInterval time.Duration
	expiv          interval.Interval
	ss             *sortedset.SortedSet
	//tree         *btreeset.BTreeSet
}

// OptStore is a store options
type OptStore func(*Store) error

// Dir - directory for database, default "."
func Dir(dir string) OptStore {
	return func(s *Store) error {
		if dir == "" {
			dir = "."
		}
		var err error
		// create dirs
		_, err = os.Stat(dir)
		if err != nil {
			// file not exists - create dirs if any
			if os.IsNotExist(err) {
				if dir != "." {
					err = os.MkdirAll(dir, os.FileMode(dirMode))
					if err != nil {
						return err
					}
				}
			} else {
				return err
			}
		}
		s.dir = dir
		return nil
	}
}

// ChunksCollision number chunks for collisions resolving,
// default is 4 (>1_000_000_000 of 8 bytes alphabet keys without collision errors)
// different keys may has same hash
// collision chunks needed for resolving this, without collisions errors
// if ChunkColCnt - zero, ErrCollision will return in case of collision
func ChunksCollision(chunks int) OptStore {
	return func(s *Store) error {
		s.chunkColCnt = chunks
		return nil
	}
}

//ChunksTotal - total chunks/shards, default 256
//Must be more then collision chunks
func ChunksTotal(chunks int) OptStore {
	return func(s *Store) error {
		s.chunksCnt = chunks
		return nil
	}
}

// SyncInterval - how often fsync do, default 0 - OS will do it
func SyncInterval(interv time.Duration) OptStore {
	return func(s *Store) error {
		s.syncInterval = interv
		if interv > 0 {
			s.iv = interval.Set(func(t time.Time) {
				for i := range s.chunks[:] {
					err := s.chunks[i].fsync()
					if err != nil {
						fmt.Printf("Error fsync:%s\n", err)
						//its critical error drive is broken
						panic(err)
					}
				}
			}, interv)
		}
		return nil
	}
}

// ExpireInterval - how often run key expiration process
// expire only one chunk
func ExpireInterval(interv time.Duration) OptStore {
	return func(s *Store) error {
		s.expireInterval = interv
		if interv > 0 {
			s.expiv = interval.Set(func(t time.Time) {
				t1 := time.Now().UnixNano()
				err := s.chunks[expirechunk].expirekeys()
				if err != nil {
					fmt.Printf("Error expire:%s\n", err)
				}
				t2 := time.Now().UnixNano()
				fmt.Printf("chuk expre %.6fs\n", float64(t2-t1)/1e9)
				expirechunk++
				if expirechunk >= s.chunksCnt {
					expirechunk = 0
				}
			}, interv)
		}
		return nil
	}
}

func hash(b []byte) uint32 {
	// TODO race, test and replace with https://github.com/spaolacci/murmur3/pull/28
	return murmur3.Sum32WithSeed(b, 0)
	/*
		convert to 24 bit hash if you need more memory, but add chunks for collisions
		//MASK_24 := uint32((1 << 24) - 1)
		//ss := h.Sum32()
		//hash := (ss >> 24) ^ (ss & MASK_24)
	*/
}

// Open return new store
// It will create 256 shards
// Each shard store keys and val size and address in map[uint32]uint32
//
// options, see https://gist.github.com/travisjeffery/8265ca411735f638db80e2e34bdbd3ae#gistcomment-3171484
// usage - Open(Dir("1"), SyncInterval(1*time.Second))
func Open(opts ...OptStore) (s *Store, err error) {
	s = &Store{}
	//default
	s.syncInterval = 0
	s.expireInterval = 0
	s.chunkColCnt = 4
	s.chunksCnt = 256
	// call option functions on instance to set options on it
	for _, opt := range opts {
		err := opt(s)
		// if the option func returns an error, add it to the list of errors
		if err != nil {
			return nil, err
		}
	}
	if s.chunksCnt-s.chunkColCnt < 1 {
		return nil, errors.New("chunksCnt must be more then chunkColCnt minimum on 1")
	}
	s.chunks = make([]chunk, s.chunksCnt)

	// create chuncks
	for i := range s.chunks[:] {

		err = s.chunks[i].init(fmt.Sprintf("%s/%d", s.dir, i))
		if err != nil {
			return nil, err
		}
	}
	s.ss = sortedset.New()
	return
}

// pack addr & size to addrSize
func addrSizeMarshal(addr uint32, size byte) addrSize {
	return addrSize{addr, size}
}

// unpack addr & size
func addrSizeUnmarshal(as addrSize) (addr, size uint32) {
	return as.addr, 1 << as.size
}

func (s *Store) idx(h uint32) uint32 {
	return uint32((int(h) % (s.chunksCnt - s.chunkColCnt)) + s.chunkColCnt)
}

// Set - store key and val in shard
// max packet size is 2^19, 512kb (524288)
// packet size = len(key) + len(val) + 8
func (s *Store) Set(k, v []byte, expire uint32) (err error) {
	h := hash(k)
	idx := s.idx(h)
	err = s.chunks[idx].set(k, v, h, expire)
	if err == ErrCollision {
		for i := 0; i < int(s.chunkColCnt); i++ {
			err = s.chunks[i].set(k, v, h, expire)
			if err == ErrCollision {
				continue
			}
			break
		}
	}
	return
}

// Get - return val by key
func (s *Store) Get(k []byte) (v []byte, err error) {
	h := hash(k)
	idx := s.idx(h)
	v, _, err = s.chunks[idx].get(k, h)
	if err == ErrCollision {
		for i := 0; i < int(s.chunkColCnt); i++ {
			v, _, err = s.chunks[i].get(k, h)
			if err == ErrCollision || err == ErrNotFound {
				continue
			}
			break
		}
	}
	return
}

// Count return count keys
func (s *Store) Count() (cnt int) {
	for i := range s.chunks[:] {
		cnt += s.chunks[i].count()
	}
	return
}

// Close - close related chunks
func (s *Store) Close() (err error) {
	errStr := ""
	if s.syncInterval > 0 {
		s.iv.Clear()
	}
	if s.expireInterval > 0 {
		s.expiv.Clear()
	}
	for i := range s.chunks[:] {
		err = s.chunks[i].close()
		if err != nil {
			errStr += err.Error() + "\r\n"
			return
		}
	}
	if errStr != "" {
		return errors.New(errStr)
	}
	return
}

// DeleteStore - remove directory with files
func DeleteStore(dir string) error {
	return os.RemoveAll(dir)
}

// FileSize returns the total size of the disk storage used by the DB.
func (s *Store) FileSize() (fs int64, err error) {
	for i := range s.chunks[:] {
		is, err := s.chunks[i].fileSize()
		if err != nil {
			return -1, err
		}
		fs += is
	}
	return
}

// Delete - delete item by key
func (s *Store) Delete(k []byte) (isDeleted bool, err error) {
	h := hash(k)
	idx := s.idx(h)
	isDeleted, err = s.chunks[idx].delete(k, h)
	if err == ErrCollision {
		for i := 0; i < int(s.chunkColCnt); i++ {
			isDeleted, err = s.chunks[i].delete(k, h)
			if err == ErrCollision || err == ErrNotFound {
				continue
			}
			break
		}
	}
	return
}

// Incr - Incr item by uint64
// inited with zero
func (s *Store) Incr(k []byte, v uint64) (uint64, error) {
	h := hash(k)
	idx := s.idx(h)
	return s.chunks[idx].incrdecr(k, h, v, true)
}

// Decr - Decr item by uint64
// inited with zero
func (s *Store) Decr(k []byte, v uint64) (uint64, error) {
	h := hash(k)
	idx := s.idx(h)
	return s.chunks[idx].incrdecr(k, h, v, false)
}

// Backup is very stupid now. It remove files with same name with bak extension
// and create new backup files
func (s *Store) Backup() (err error) {
	for i := range s.chunks[:] {
		err = s.chunks[i].backup()
		if err != nil {
			return
		}
	}
	return
}

// Expire - remove expired keys from all chunks
func (s *Store) Expire() (err error) {
	for i := range s.chunks[:] {
		err = s.chunks[i].expirekeys()
		if err != nil {
			return
		}
	}
	return
}

func readUint32(b []byte) uint32 {
	_ = b[3]
	return uint32(b[3]) | uint32(b[2])<<8 | uint32(b[1])<<16 | uint32(b[0])<<24
}

func appendUint32(b []byte, x uint32) []byte {
	a := [4]byte{
		byte(x >> 24),
		byte(x >> 16),
		byte(x >> 8),
		byte(x),
	}
	return append(b, a[:]...)
}

// Bucket - create new bucket for storing keys with same prefix in memory index
func (s *Store) Bucket(name string) (*sortedset.BucketStore, error) {
	// store all buckets in [buckets] key
	bKey := []byte("[buckets]")
	val, err := s.Get(bKey)
	if err == ErrNotFound {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	buckets := string(val)
	var isExists bool
	for _, bucket := range strings.Split(buckets, ",") {
		if bucket == name {
			isExists = true
			break
		}
	}
	if !isExists {
		if buckets != "" {
			buckets += ","
		}
		buckets += name
		err = s.Set(bKey, []byte(buckets), 0)
		if err != nil {
			return nil, err
		}
	}
	return sortedset.Bucket(s.ss, name), nil
}

// Put - store key and val with Set
// And add key in index (backed by sortedset)
func (s *Store) Put(bucket *sortedset.BucketStore, k, v []byte) (err error) {
	key := []byte(bucket.Name)
	key = append(key, k...)
	err = s.Set(key, v, 0)
	if err == nil {
		bucket.Put(string(k))
	}
	return
}

// Keys will return keys stored with Put method
// Params: key prefix ("" - return all keys)
// Limit - 0, all
// Offset - 0, zero offset
// Keys will be without prefix and in descending order
func (s *Store) Keys(bucket *sortedset.BucketStore, limit, offset int) []string {
	return bucket.Keys(limit, offset)
}
