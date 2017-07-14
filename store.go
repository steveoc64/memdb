// Package memdb is designed to allow configurable indexing of values from a structure
package memdb

import (
	"github.com/google/btree"
	"github.com/nedscode/memdb/persist"

	"fmt"
	"strings"
	"sync"
)

// Store implements an indexed storage for an Indexer item
//
// Just like a real database, if you update an item such that it's index keys would change, you must Put it back in to
// update the items indexes in the database, and also to cause update notifications to be sent.
//
// DO NOT under any circumstances update the PRIMARY KEYs (ie keys used to determine the output of the Less()
// comparator) without first removing the existing item. Such an act would leave the item stranded in an unknown
// location within the index.
type Store struct {
	sync.RWMutex

	backing *btree.BTree
	indexes map[string]*Index
	cIndex  *Index
	index   map[string]map[string][]Indexer
	happens chan *happening
	used    bool

	persister persist.Persister

	insertNotifiers []NotifyFunc
	updateNotifiers []NotifyFunc
	removeNotifiers []NotifyFunc
	expiryNotifiers []NotifyFunc
}

type happening struct {
	event Event
	old   Indexer
	new   Indexer
}

// Index represent a list of indexes
type Index struct {
	n      int
	id     string
	fields []string
	store  *Store
	unique bool
}

// Event is a type of event emitted by the class, see the On() method
type Event int

// String describes the event type
func (e Event) String() string {
	switch e {
	case Insert:
		return "Insert event"
	case Update:
		return "Update event"
	case Remove:
		return "Remove event"
	case Expiry:
		return "Expiry event"
	default:
		break
	}
	return "Unknown event"
}

const (
	// Insert Events happen when an item is inserted for the first time
	Insert Event = iota

	// Update Events happen when an existing item is replaced with an new item
	Update

	// Remove Events happen when an existing item is deleted
	Remove

	// Expiry Events happen when items are removed due to being expired
	Expiry
)

type noIndexer struct{}

func (x *noIndexer) Less(_ Indexer) bool {
	return true
}
func (x *noIndexer) IsExpired() bool {
	return false
}
func (x *noIndexer) GetField(_ string) string {
	return ""
}

var (
	none = &noIndexer{}
)

// NotifyFunc is an event receiver that gets called when events happen
type NotifyFunc func(event Event, old, new Indexer)

// NewStore returns an initialized store for you to use
func NewStore() *Store {
	happens := make(chan *happening, 100000)
	s := &Store{
		backing: btree.New(2),
		index:   map[string]map[string][]Indexer{},
		indexes: map[string]*Index{},
		happens: happens,
	}

	go func() {
		for h := range happens {
			s.emit(h.event, h.old, h.new)
		}
	}()

	return s
}

// CreateIndex adds a new index to the list of indexes before the store is populated
func (s *Store) CreateIndex(fields ...string) *Store {
	if s.used {
		panic("Cannot create index on in-use store")
	}

	id := strings.Join(fields, "\000")
	index := &Index{
		n:      len(s.indexes),
		id:     id,
		fields: fields,
		store:  s,
	}
	s.indexes[id] = index
	s.cIndex = index
	return s
}

// Unique makes the current index unique
// Making an index unique will force the delete of all but the last inserted item in the index upon Put()
func (s *Store) Unique() *Store {
	if s.used {
		panic("Cannot create index on in-use store")
	}
	if s.cIndex != nil {
		s.cIndex.unique = true
	}
	return s
}

// Persistent adds a persister to the database and loads up the existing records, call after all indexes are setup but
// before you begin using it.
func (s *Store) Persistent(persister persist.Persister) error {
	if s.used {
		panic("Cannot make persist on in-use store")
	}

	s.used = true
	s.persister = persister

	s.Lock()
	defer s.Unlock()

	var loaderErr error
	err := persister.Load(func(id string, indexer interface{}) {
		if idx, ok := indexer.(Indexer); ok {
			w := s.wrapIt(idx)
			w.uid = UID(id)
			s.addWrap(w)
		} else {
			loaderErr = fmt.Errorf("Error converting item %T to Indexer", indexer)
		}
	})

	if err == nil {
		err = loaderErr
	}

	return err
}

// Get returns an item equal to the passed item from the store
func (s *Store) Get(search Indexer) Indexer {
	s.RLock()
	defer s.RUnlock()

	found := s.backing.Get(&wrap{indexer: search})
	if found == nil {
		return nil
	}

	if w, ok := found.(*wrap); ok {
		return w.indexer
	}

	return nil
}

// In finds a simple or compound index to perform queries upon
func (s *Store) In(fields ...string) *Index {
	s.RLock()
	defer s.RUnlock()

	id := strings.Join(fields, "\000")
	if f, ok := s.indexes[id]; ok {
		return f
	}

	return nil
}

// Each calls iterator for every matched element
// Items are not guaranteed to be in any particular order
func (idx *Index) Each(cb Iterator, keys ...string) {
	if idx == nil {
		return
	}

	idx.store.RLock()
	defer idx.store.RUnlock()

	values := idx.find(keys)
	if values == nil {
		return
	}
	for _, indexer := range values {
		if !cb(indexer) {
			return
		}
	}
}

// Lookup returns the list of items from the index that match given key
// Returned items are not guaranteed to be in any particular order
func (idx *Index) Lookup(keys ...string) []Indexer {
	if idx == nil {
		return nil
	}

	idx.store.RLock()
	defer idx.store.RUnlock()

	values := idx.find(keys)
	if values == nil {
		return nil
	}
	c := make([]Indexer, len(values))
	copy(c, values)
	return c
}

func (idx *Index) find(keys []string) []Indexer {
	if idx == nil {
		return nil
	}

	if len(keys) != len(idx.fields) {
		return nil
	}

	s := idx.store

	index, ok := s.index[idx.id]
	if !ok {
		return nil
	}

	key := strings.Join(keys, "\000")

	values, ok := index[key]
	if !ok {
		return nil
	}

	return values
}

func cbWrap(cb Iterator) btree.ItemIterator {
	return func(i btree.Item) bool {
		if w, ok := i.(*wrap); ok {
			return cb(w.indexer)
		}
		return true
	}
}

func traverse(traverse func(btree.Item, btree.Item, btree.ItemIterator), a, b btree.Item, iterator btree.ItemIterator) {
	traverse(a, b, iterator)
}

// Ascend calls provided callback function from start (lowest order) of items until end or iterator function returns
// false
func (s *Store) Ascend(cb Iterator) {
	s.RLock()
	defer s.RUnlock()
	traverse(s.backing.AscendRange, nil, nil, cbWrap(cb))
}

// AscendStarting calls provided callback function from item equal to at until end or iterator function returns false
func (s *Store) AscendStarting(at Indexer, cb Iterator) {
	s.RLock()
	defer s.RUnlock()
	traverse(s.backing.AscendRange, &wrap{indexer: at}, nil, cbWrap(cb))
}

// Descend calls provided callback function from end (highest order) of items until start or iterator function returns
// false
func (s *Store) Descend(cb Iterator) {
	s.RLock()
	defer s.RUnlock()
	traverse(s.backing.DescendRange, nil, nil, cbWrap(cb))
}

// DescendStarting calls provided callback function from item equal to at until start or iterator function returns false
func (s *Store) DescendStarting(at Indexer, cb Iterator) {
	s.RLock()
	defer s.RUnlock()
	traverse(s.backing.DescendRange, &wrap{indexer: at}, nil, cbWrap(cb))
}

func (s *Store) findExpired() []Indexer {
	s.RLock()
	defer s.RUnlock()

	var rm []Indexer
	s.backing.Ascend(func(item btree.Item) bool {
		if w, ok := item.(*wrap); ok {
			if w.indexer.IsExpired() {
				rm = append(rm, w.indexer)
			}
		}
		return true
	})

	return rm
}

// Expire finds all expiring items in the store and deletes them
func (s *Store) Expire() int {
	rm := s.findExpired()

	s.Lock()
	defer s.Unlock()

	for _, v := range rm {
		old, _ := s.rm(v)
		if old != nil {
			s.happens <- &happening{
				event: Expiry,
				old:   old,
			}
		}
	}

	return len(rm)
}

// Put places an indexer item into the store
func (s *Store) Put(indexer Indexer) (Indexer, error) {
	s.Lock()
	defer s.Unlock()

	old, err := s.add(indexer)

	if old == nil {
		s.happens <- &happening{
			event: Insert,
			new:   indexer,
		}
	} else if old != none {
		s.happens <- &happening{
			event: Update,
			old:   old,
			new:   indexer,
		}
	}

	return old, err
}

// Delete removes an item equal to the search item
func (s *Store) Delete(search Indexer) (Indexer, error) {
	s.Lock()
	defer s.Unlock()

	old, err := s.rm(search)
	if old != nil {
		s.happens <- &happening{
			event: Remove,
			old:   old,
		}
	}
	return old, err
}

// Len returns the number of items in the database
func (s *Store) Len() int {
	s.RLock()
	defer s.RUnlock()

	return s.backing.Len()
}

// Indexes returns the list of indexed indexes
func (s *Store) Indexes() [][]string {
	s.RLock()
	defer s.RUnlock()

	c := make([][]string, len(s.indexes))
	for _, f := range s.indexes {
		fc := make([]string, len(f.fields))
		copy(fc, f.fields)
		c[f.n] = fc
	}
	return c
}

// Keys returns the list of distinct keys for an index
func (s *Store) Keys(fields ...string) []string {
	f := s.In(fields...)
	if f == nil {
		return nil
	}

	s.RLock()
	defer s.RUnlock()

	index, ok := s.index[f.id]
	if !ok {
		return nil
	}

	keys := make([]string, len(index))
	i := 0
	for key := range index {
		keys[i] = key
		i++
	}
	return keys
}

// On registers an event handler for an event type
func (s *Store) On(event Event, notify NotifyFunc) {
	switch event {
	case Insert:
		s.insertNotifiers = append(s.insertNotifiers, notify)
	case Update:
		s.updateNotifiers = append(s.updateNotifiers, notify)
	case Remove:
		s.removeNotifiers = append(s.removeNotifiers, notify)
	case Expiry:
		s.expiryNotifiers = append(s.expiryNotifiers, notify)
	default:
		return
	}
}

func (s *Store) emit(event Event, old, new Indexer) {
	var handlers []NotifyFunc
	switch event {
	case Insert:
		handlers = s.insertNotifiers
	case Update:
		handlers = s.updateNotifiers
	case Remove:
		handlers = s.removeNotifiers
	case Expiry:
		handlers = s.expiryNotifiers
	default:
		return
	}

	if len(handlers) > 0 {
		for _, handler := range handlers {
			handler(event, old, new)
		}
	}
}

func (s *Store) add(indexer Indexer) (Indexer, error) {
	w := s.wrapIt(indexer)
	ret := s.addWrap(w)

	var err error
	if s.persister != nil {
		err = s.persister.Save(string(w.UID()), indexer)
	}

	return ret, err
}

func (s *Store) addWrap(w *wrap) Indexer {
	s.used = true
	found := s.backing.ReplaceOrInsert(w)

	var ow *wrap
	if found != nil {
		ow = found.(*wrap)
	}

	var emitted bool
	for _, index := range s.indexes {
		key := w.values[index.n]
		if ow != nil {
			oldKey := ow.values[index.n]
			if oldKey != key {
				s.rmFromIndex(index.id, oldKey, ow.indexer)
				emitted = s.addToIndex(index.id, key, w.indexer)
			}
		} else {
			emitted = s.addToIndex(index.id, key, w.indexer)
		}
	}

	if ow != nil {
		return ow.indexer
	}
	if emitted {
		return none
	}
	return nil
}

func (s *Store) addToIndex(indexID string, key string, indexer Indexer) (emitted bool) {
	index, ok := s.indexes[indexID]
	if !ok {
		return
	}

	indexItems, ok := s.index[indexID]
	if !ok {
		indexItems = map[string][]Indexer{}
		s.index[indexID] = indexItems
	}

	items := indexItems[key]
	if index.unique && len(items) > 0 {
		// Items have been replaced!
		for _, item := range indexItems[key] {
			rm, _ := s.rm(item)
			if rm != nil {
				s.happens <- &happening{
					event: Update,
					old:   rm,
					new:   indexer,
				}
				emitted = true
			}
		}
		items = nil
	}
	indexItems[key] = append(items, indexer)
	return
}

func (s *Store) rm(indexer Indexer) (Indexer, error) {
	removed := s.backing.Delete(&wrap{indexer: indexer})

	var err error
	if removed != nil {
		w := removed.(*wrap)
		if s.persister != nil {
			err = s.persister.Remove(string(w.UID()))
		}

		for _, index := range s.indexes {
			key := w.values[index.n]
			s.rmFromIndex(index.id, key, w.indexer)
		}
	}

	if removed != nil {
		return removed.(*wrap).indexer, err
	}
	return nil, err
}

func (s *Store) rmFromIndex(indexID string, key string, indexer Indexer) {
	index, ok := s.index[indexID]
	if !ok {
		return
	}

	values, ok := index[key]
	if !ok {
		return
	}

	for i, value := range values {
		if indexer == value {
			n := len(values)
			if n == 1 && i == 0 {
				index[key] = nil
				return
			}
			values[i] = values[n-1]
			index[key] = values[:n-1]
			return
		}
	}
}

func getIndexValue(indexer Indexer, index *Index) string {
	components := make([]string, len(index.fields))
	for i, field := range index.fields {
		components[i] = indexer.GetField(field)
	}
	return strings.Join(components, "\000")
}

func (s *Store) wrapIt(indexer Indexer) *wrap {
	values := make([]string, len(s.indexes))
	for _, index := range s.indexes {
		values[index.n] = getIndexValue(indexer, index)
	}

	return &wrap{
		indexer: indexer,
		values:  values,
	}
}
