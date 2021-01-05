package bitcask

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	art "github.com/plar/go-adaptive-radix-tree"
	"github.com/prologic/bitcask/flock"
	"github.com/prologic/bitcask/internal"
	"github.com/prologic/bitcask/internal/config"
	"github.com/prologic/bitcask/internal/data"
	"github.com/prologic/bitcask/internal/data/codec"
	"github.com/prologic/bitcask/internal/index"
	"github.com/prologic/bitcask/internal/metadata"
	"github.com/prologic/bitcask/scripts/migrations"
	log "github.com/sirupsen/logrus"
)

const (
	lockfile     = "lock"
	ttlIndexFile = "ttl_index"
)

var (
	// ErrKeyNotFound is the error returned when a key is not found
	ErrKeyNotFound = errors.New("error: key not found")

	// ErrKeyTooLarge is the error returned for a key that exceeds the
	// maximum allowed key size (configured with WithMaxKeySize).
	ErrKeyTooLarge = errors.New("error: key too large")

	// ErrKeyExpired is the error returned when a key is queried which has
	// already expired (due to ttl)
	ErrKeyExpired = errors.New("error: key expired")

	// ErrEmptyKey is the error returned for a value with an empty key.
	ErrEmptyKey = errors.New("error: empty key")

	// ErrValueTooLarge is the error returned for a value that exceeds the
	// maximum allowed value size (configured with WithMaxValueSize).
	ErrValueTooLarge = errors.New("error: value too large")

	// ErrChecksumFailed is the error returned if a key/value retrieved does
	// not match its CRC checksum
	ErrChecksumFailed = errors.New("error: checksum failed")

	// ErrDatabaseLocked is the error returned if the database is locked
	// (typically opened by another process)
	ErrDatabaseLocked = errors.New("error: database locked")

	ErrInvalidVersion = errors.New("error: invalid db version")

	// ErrMergeInProgress is the error returned if merge is called when already a merge
	// is in progress
	ErrMergeInProgress = errors.New("error: merge already in progress")
)

// Bitcask is a struct that represents a on-disk LSM and WAL data structure
// and in-memory hash of key/value pairs as per the Bitcask paper and seen
// in the Riak database.
type Bitcask struct {
	mu sync.RWMutex

	*flock.Flock

	config     *config.Config
	options    []Option
	path       string
	curr       data.Datafile
	datafiles  map[int]data.Datafile
	trie       art.Tree
	indexer    index.Indexer
	ttlIndexer index.Indexer
	ttlIndex   art.Tree
	metadata   *metadata.MetaData
	isMerging  bool
}

// Stats is a struct returned by Stats() on an open Bitcask instance
type Stats struct {
	Datafiles int
	Keys      int
	Size      int64
}

// Stats returns statistics about the database including the number of
// data files, keys and overall size on disk of the data
func (b *Bitcask) Stats() (stats Stats, err error) {
	if stats.Size, err = internal.DirSize(b.path); err != nil {
		return
	}

	b.mu.RLock()
	stats.Datafiles = len(b.datafiles)
	stats.Keys = b.trie.Size()
	b.mu.RUnlock()

	return
}

// Close closes the database and removes the lock. It is important to call
// Close() as this is the only way to cleanup the lock held by the open
// database.
func (b *Bitcask) Close() error {
	b.mu.RLock()
	defer func() {
		b.mu.RUnlock()
		b.Flock.Unlock()
	}()

	return b.close()
}

func (b *Bitcask) close() error {
	if err := b.saveIndexes(); err != nil {
		return err
	}

	b.metadata.IndexUpToDate = true
	if err := b.saveMetadata(); err != nil {
		return err
	}

	for _, df := range b.datafiles {
		if err := df.Close(); err != nil {
			return err
		}
	}

	return b.curr.Close()
}

// Sync flushes all buffers to disk ensuring all data is written
func (b *Bitcask) Sync() error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if err := b.saveMetadata(); err != nil {
		return err
	}

	return b.curr.Sync()
}

// Get fetches value for a key
func (b *Bitcask) Get(key []byte) ([]byte, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	e, err := b.get(key)
	if err != nil {
		return nil, err
	}
	return e.Value, nil
}

// Has returns true if the key exists in the database, false otherwise.
func (b *Bitcask) Has(key []byte) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, found := b.trie.Search(key)
	if found {
		return !b.isExpired(key)
	}
	return found
}

// Put stores the key and value in the database.
func (b *Bitcask) Put(key, value []byte, options ...PutOptions) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if b.config.MaxKeySize > 0 && uint32(len(key)) > b.config.MaxKeySize {
		return ErrKeyTooLarge
	}
	if b.config.MaxValueSize > 0 && uint64(len(value)) > b.config.MaxValueSize {
		return ErrValueTooLarge
	}
	var feature Feature
	for _, opt := range options {
		if err := opt(&feature); err != nil {
			return err
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	offset, n, err := b.put(key, value, feature)
	if err != nil {
		return err
	}

	if b.config.Sync {
		if err := b.curr.Sync(); err != nil {
			return err
		}
	}

	// in case of successful `put`, IndexUpToDate will be always be false
	b.metadata.IndexUpToDate = false

	if oldItem, found := b.trie.Search(key); found {
		b.metadata.ReclaimableSpace += oldItem.(internal.Item).Size
	}

	item := internal.Item{FileID: b.curr.FileID(), Offset: offset, Size: n}
	b.trie.Insert(key, item)
	if feature.Expiry != nil {
		b.ttlIndex.Insert(key, *feature.Expiry)
	}

	return nil
}

// Delete deletes the named key.
func (b *Bitcask) Delete(key []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.delete(key)
}

// delete deletes the named key. If the key doesn't exist or an I/O error
// occurs the error is returned.
func (b *Bitcask) delete(key []byte) error {
	_, _, err := b.put(key, []byte{}, Feature{})
	if err != nil {
		return err
	}
	if item, found := b.trie.Search(key); found {
		b.metadata.ReclaimableSpace += item.(internal.Item).Size + codec.MetaInfoSize + int64(len(key))
	}
	b.trie.Delete(key)
	b.ttlIndex.Delete(key)

	return nil
}

// DeleteAll deletes all the keys. If an I/O error occurs the error is returned.
func (b *Bitcask) DeleteAll() (err error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	b.trie.ForEach(func(node art.Node) bool {
		_, _, err = b.put(node.Key(), []byte{}, Feature{})
		if err != nil {
			return false
		}
		item, _ := b.trie.Search(node.Key())
		b.metadata.ReclaimableSpace += item.(internal.Item).Size + codec.MetaInfoSize + int64(len(node.Key()))
		return true
	})
	b.trie = art.New()
	b.ttlIndex = art.New()

	return
}

// Scan performs a prefix scan of keys matching the given prefix and calling
// the function `f` with the keys found. If the function returns an error
// no further keys are processed and the first error returned.
func (b *Bitcask) Scan(prefix []byte, f func(key []byte) error) (err error) {
	b.trie.ForEachPrefix(prefix, func(node art.Node) bool {
		// Skip the root node
		if len(node.Key()) == 0 {
			return true
		}

		if err = f(node.Key()); err != nil {
			return false
		}
		return true
	})
	return
}

// Len returns the total number of keys in the database
func (b *Bitcask) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.trie.Size()
}

// Keys returns all keys in the database as a channel of keys
func (b *Bitcask) Keys() chan []byte {
	ch := make(chan []byte)
	go func() {
		b.mu.RLock()
		defer b.mu.RUnlock()

		for it := b.trie.Iterator(); it.HasNext(); {
			node, _ := it.Next()
			if b.isExpired(node.Key()) {
				continue
			}
			ch <- node.Key()
		}
		close(ch)
	}()

	return ch
}

// RunGC deletes all expired keys
func (b *Bitcask) RunGC() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runGC()
}

// runGC deletes all keys that are expired
// caller function should take care of the locking when calling this method
func (b *Bitcask) runGC() (err error) {
	b.ttlIndex.ForEach(func(node art.Node) (cont bool) {
		if !b.isExpired(node.Key()) {
			return true
		}
		if err = b.delete(node.Key()); err != nil {
			return false
		}
		return true
	})
	return
}

// Fold iterates over all keys in the database calling the function `f` for
// each key. If the function returns an error, no further keys are processed
// and the error returned.
func (b *Bitcask) Fold(f func(key []byte) error) (err error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	b.trie.ForEach(func(node art.Node) bool {
		if err = f(node.Key()); err != nil {
			return false
		}
		return true
	})

	return
}

// get retrieves the value of the given key
func (b *Bitcask) get(key []byte) (internal.Entry, error) {
	var df data.Datafile

	value, found := b.trie.Search(key)
	if !found {
		return internal.Entry{}, ErrKeyNotFound
	}
	if expired := b.isExpired(key); expired {
		_ = b.delete(key) // we don't care if it doesnt succeed
		return internal.Entry{}, ErrKeyExpired
	}

	item := value.(internal.Item)

	if item.FileID == b.curr.FileID() {
		df = b.curr
	} else {
		df = b.datafiles[item.FileID]
	}

	e, err := df.ReadAt(item.Offset, item.Size)
	if err != nil {
		return internal.Entry{}, err
	}

	checksum := crc32.ChecksumIEEE(e.Value)
	if checksum != e.Checksum {
		return internal.Entry{}, ErrChecksumFailed
	}

	return e, nil
}

// put inserts a new (key, value). Both key and value are valid inputs.
func (b *Bitcask) put(key, value []byte, feature Feature) (int64, int64, error) {
	size := b.curr.Size()
	if size >= int64(b.config.MaxDatafileSize) {
		err := b.curr.Close()
		if err != nil {
			return -1, 0, err
		}

		id := b.curr.FileID()

		df, err := data.NewDatafile(b.path, id, true, b.config.MaxKeySize, b.config.MaxValueSize, b.config.FileFileModeBeforeUmask)
		if err != nil {
			return -1, 0, err
		}

		b.datafiles[id] = df

		id = b.curr.FileID() + 1
		curr, err := data.NewDatafile(b.path, id, false, b.config.MaxKeySize, b.config.MaxValueSize, b.config.FileFileModeBeforeUmask)
		if err != nil {
			return -1, 0, err
		}
		b.curr = curr
		err = b.saveIndexes()
		if err != nil {
			return -1, 0, err
		}
	}

	e := internal.NewEntry(key, value, feature.Expiry)
	return b.curr.Write(e)
}

// closeCurrentFile closes current datafile and makes it read only.
func (b *Bitcask) closeCurrentFile() error {
	err := b.curr.Close()
	if err != nil {
		return err
	}
	id := b.curr.FileID()
	df, err := data.NewDatafile(b.path, id, true, b.config.MaxKeySize, b.config.MaxValueSize, b.config.FileFileModeBeforeUmask)
	if err != nil {
		return err
	}

	b.datafiles[id] = df
	return nil
}

// openNewWritableFile opens new datafile for writing data
func (b *Bitcask) openNewWritableFile() error {
	id := b.curr.FileID() + 1
	curr, err := data.NewDatafile(b.path, id, false, b.config.MaxKeySize, b.config.MaxValueSize, b.config.FileFileModeBeforeUmask)
	if err != nil {
		return err
	}
	b.curr = curr
	return nil
}

func (b *Bitcask) Reopen() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.reopen()
}

// reopen reloads a bitcask object with index and datafiles
// caller of this method should take care of locking
func (b *Bitcask) reopen() error {
	datafiles, lastID, err := loadDatafiles(b.path, b.config.MaxKeySize, b.config.MaxValueSize, b.config.FileFileModeBeforeUmask)
	if err != nil {
		return err
	}
	t, ttlIndex, err := loadIndexes(b, datafiles, lastID)
	if err != nil {
		return err
	}

	curr, err := data.NewDatafile(b.path, lastID, false, b.config.MaxKeySize, b.config.MaxValueSize, b.config.FileFileModeBeforeUmask)
	if err != nil {
		return err
	}

	b.trie = t
	b.curr = curr
	b.ttlIndex = ttlIndex
	b.datafiles = datafiles

	return nil
}

// Merge merges all datafiles in the database. Old keys are squashed
// and deleted keys removes. Duplicate key/value pairs are also removed.
// Call this function periodically to reclaim disk space.
func (b *Bitcask) Merge() error {
	b.mu.Lock()
	if b.isMerging {
		b.mu.Unlock()
		return ErrMergeInProgress
	}
	b.isMerging = true
	b.mu.Unlock()
	defer func() {
		b.isMerging = false
	}()
	b.mu.RLock()
	err := b.closeCurrentFile()
	if err != nil {
		b.mu.RUnlock()
		return err
	}
	filesToMerge := make([]int, 0, len(b.datafiles))
	for k := range b.datafiles {
		filesToMerge = append(filesToMerge, k)
	}
	err = b.openNewWritableFile()
	if err != nil {
		b.mu.RUnlock()
		return err
	}
	b.mu.RUnlock()
	sort.Ints(filesToMerge)

	// Temporary merged database path
	temp, err := ioutil.TempDir(b.path, "merge")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)

	// Create a merged database
	mdb, err := Open(temp, withConfig(b.config))
	if err != nil {
		return err
	}

	// Rewrite all key/value pairs into merged database
	// Doing this automatically strips deleted keys and
	// old key/value pairs
	err = b.Fold(func(key []byte) error {
		item, _ := b.trie.Search(key)
		// if key was updated after start of merge operation, nothing to do
		if item.(internal.Item).FileID > filesToMerge[len(filesToMerge)-1] {
			return nil
		}
		e, err := b.get(key)
		if err != nil {
			return err
		}
		// prepare entry options
		var opts []PutOptions
		if e.Expiry != nil {
			opts = append(opts, WithExpiry(*(e.Expiry)))
		}

		if err := mdb.Put(key, e.Value, opts...); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}
	if err = mdb.Close(); err != nil {
		return err
	}
	// no reads and writes till we reopen
	b.mu.Lock()
	defer b.mu.Unlock()
	if err = b.close(); err != nil {
		return err
	}

	// Remove data files
	files, err := ioutil.ReadDir(b.path)
	if err != nil {
		return err
	}
	for _, file := range files {
		if file.IsDir() || file.Name() == lockfile {
			continue
		}
		ids, err := internal.ParseIds([]string{file.Name()})
		if err != nil {
			return err
		}
		// if datafile was created after start of merge, skip
		if len(ids) > 0 && ids[0] > filesToMerge[len(filesToMerge)-1] {
			continue
		}
		err = os.RemoveAll(path.Join(b.path, file.Name()))
		if err != nil {
			return err
		}
	}

	// Rename all merged data files
	files, err = ioutil.ReadDir(mdb.path)
	if err != nil {
		return err
	}
	for _, file := range files {
		err := os.Rename(
			path.Join([]string{mdb.path, file.Name()}...),
			path.Join([]string{b.path, file.Name()}...),
		)
		if err != nil {
			return err
		}
	}
	b.metadata.ReclaimableSpace = 0

	// And finally reopen the database
	return b.reopen()
}

// Open opens the database at the given path with optional options.
// Options can be provided with the `WithXXX` functions that provide
// configuration options as functions.
func Open(path string, options ...Option) (*Bitcask, error) {
	var (
		cfg  *config.Config
		err  error
		meta *metadata.MetaData
	)

	configPath := filepath.Join(path, "config.json")
	if internal.Exists(configPath) {
		cfg, err = config.Load(configPath)
		if err != nil {
			return nil, err
		}
	} else {
		cfg = newDefaultConfig()
	}

	if err := checkAndUpgrade(cfg, configPath); err != nil {
		return nil, err
	}

	for _, opt := range options {
		if err := opt(cfg); err != nil {
			return nil, err
		}
	}

	if err := os.MkdirAll(path, cfg.DirFileModeBeforeUmask); err != nil {
		return nil, err
	}

	meta, err = loadMetadata(path)
	if err != nil {
		return nil, err
	}

	bitcask := &Bitcask{
		Flock:      flock.New(filepath.Join(path, lockfile)),
		config:     cfg,
		options:    options,
		path:       path,
		indexer:    index.NewIndexer(),
		ttlIndexer: index.NewTTLIndexer(),
		metadata:   meta,
	}

	locked, err := bitcask.Flock.TryLock()
	if err != nil {
		return nil, err
	}

	if !locked {
		return nil, ErrDatabaseLocked
	}

	if err := cfg.Save(configPath); err != nil {
		return nil, err
	}

	if cfg.AutoRecovery {
		if err := data.CheckAndRecover(path, cfg); err != nil {
			return nil, fmt.Errorf("recovering database: %s", err)
		}
	}
	if err := bitcask.Reopen(); err != nil {
		return nil, err
	}

	return bitcask, nil
}

// checkAndUpgrade checks if DB upgrade is required
// if yes, then applies version upgrade and saves updated config
func checkAndUpgrade(cfg *config.Config, configPath string) error {
	if cfg.DBVersion == CurrentDBVersion {
		return nil
	}
	if cfg.DBVersion > CurrentDBVersion {
		return ErrInvalidVersion
	}
	// for v0 to v1 upgrade, we need to append 8 null bytes after each encoded entry in datafiles
	if cfg.DBVersion == uint32(0) && CurrentDBVersion == uint32(1) {
		log.Warn("upgrading db version, might take some time....")
		cfg.DBVersion = CurrentDBVersion
		return migrations.ApplyV0ToV1(filepath.Dir(configPath), cfg.MaxDatafileSize)
	}
	return nil
}

// Backup copies db directory to given path
// it creates path if it does not exist
func (b *Bitcask) Backup(path string) error {
	if !internal.Exists(path) {
		if err := os.MkdirAll(path, b.config.DirFileModeBeforeUmask); err != nil {
			return err
		}
	}
	return internal.Copy(b.path, path, []string{lockfile})
}

// saveIndex saves index and ttl_index currently in RAM to disk
func (b *Bitcask) saveIndexes() error {
	tempIdx := "temp_index"
	if err := b.indexer.Save(b.trie, filepath.Join(b.path, tempIdx)); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(b.path, tempIdx), filepath.Join(b.path, "index")); err != nil {
		return err
	}
	if err := b.ttlIndexer.Save(b.ttlIndex, filepath.Join(b.path, tempIdx)); err != nil {
		return err
	}
	return os.Rename(filepath.Join(b.path, tempIdx), filepath.Join(b.path, ttlIndexFile))
}

// saveMetadata saves metadata into disk
func (b *Bitcask) saveMetadata() error {
	return b.metadata.Save(filepath.Join(b.path, "meta.json"), b.config.DirFileModeBeforeUmask)
}

// Reclaimable returns space that can be reclaimed
func (b *Bitcask) Reclaimable() int64 {
	return b.metadata.ReclaimableSpace
}

// isExpired returns true if a key has expired
// it returns false if key does not exist in ttl index
func (b *Bitcask) isExpired(key []byte) bool {
	expiry, found := b.ttlIndex.Search(key)
	if !found {
		return false
	}
	return expiry.(time.Time).Before(time.Now().UTC())
}

func loadDatafiles(path string, maxKeySize uint32, maxValueSize uint64, fileModeBeforeUmask os.FileMode) (datafiles map[int]data.Datafile, lastID int, err error) {
	fns, err := internal.GetDatafiles(path)
	if err != nil {
		return nil, 0, err
	}

	ids, err := internal.ParseIds(fns)
	if err != nil {
		return nil, 0, err
	}

	datafiles = make(map[int]data.Datafile, len(ids))
	for _, id := range ids {
		datafiles[id], err = data.NewDatafile(path, id, true, maxKeySize, maxValueSize, fileModeBeforeUmask)
		if err != nil {
			return
		}

	}
	if len(ids) > 0 {
		lastID = ids[len(ids)-1]
	}
	return
}

func getSortedDatafiles(datafiles map[int]data.Datafile) []data.Datafile {
	out := make([]data.Datafile, len(datafiles))
	idx := 0
	for _, df := range datafiles {
		out[idx] = df
		idx++
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].FileID() < out[j].FileID()
	})
	return out
}

// loadIndexes loads index from disk to memory. If index is not available or partially available (last bitcask process crashed)
// then it iterates over last datafile and construct index
// we construct ttl_index here also along with normal index
func loadIndexes(b *Bitcask, datafiles map[int]data.Datafile, lastID int) (art.Tree, art.Tree, error) {
	t, found, err := b.indexer.Load(filepath.Join(b.path, "index"), b.config.MaxKeySize)
	if err != nil {
		return nil, nil, err
	}
	ttlIndex, _, err := b.ttlIndexer.Load(filepath.Join(b.path, ttlIndexFile), b.config.MaxKeySize)
	if err != nil {
		return nil, nil, err
	}
	if found && b.metadata.IndexUpToDate {
		return t, ttlIndex, nil
	}
	if found {
		if err := loadIndexFromDatafile(t, ttlIndex, datafiles[lastID]); err != nil {
			return nil, ttlIndex, err
		}
		return t, ttlIndex, nil
	}
	sortedDatafiles := getSortedDatafiles(datafiles)
	for _, df := range sortedDatafiles {
		if err := loadIndexFromDatafile(t, ttlIndex, df); err != nil {
			return nil, ttlIndex, err
		}
	}
	return t, ttlIndex, nil
}

func loadIndexFromDatafile(t art.Tree, ttlIndex art.Tree, df data.Datafile) error {
	var offset int64
	for {
		e, n, err := df.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		// Tombstone value  (deleted key)
		if len(e.Value) == 0 {
			t.Delete(e.Key)
			offset += n
			continue
		}
		item := internal.Item{FileID: df.FileID(), Offset: offset, Size: n}
		t.Insert(e.Key, item)
		if e.Expiry != nil {
			ttlIndex.Insert(e.Key, *e.Expiry)
		}
		offset += n
	}
	return nil
}

func loadMetadata(path string) (*metadata.MetaData, error) {
	if !internal.Exists(filepath.Join(path, "meta.json")) {
		meta := new(metadata.MetaData)
		return meta, nil
	}
	return metadata.Load(filepath.Join(path, "meta.json"))
}
