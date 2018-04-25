/*
Copyright (c) 2018 Simon Schmidt

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/


package mmdb2

import "unsafe"
import "github.com/steveyen/gtreap"
import "time"
import "bytes"
import "github.com/dgryski/go-farm"
import "sync/atomic"

type LoadFlags uint

const (
	L_NONE LoadFlags = 0
	
	L_HASHED LoadFlags = 1<<iota
	
	// If this flag is set, the load process stops, once it encounters an invalid record.
	L_STOP_INVALID
	
	// If the log contains an invalid tail, it will be discarded using this flag.
	L_DISCARD_INVALID_TAIL
)
func (l LoadFlags) has(f LoadFlags) bool { return (l&f)!=0 }

type iKey struct{
	hash uint32
	data []byte
}

func decodeItem(a interface{}) (uint32,[]byte) {
	switch v := a.(type) {
	case **Value:
		return (*v).GHash(),(*v).GKey()
	case iKey:
		return v.hash,v.data
	case *iKey:
		return v.hash,v.data
	}
	panic("...")
}

func compareItem(a, b interface{}) int {
	Ai,Ab := decodeItem(a)
	Bi,Bb := decodeItem(b)
	
	switch {
	case Ai > Bi: return 1
	case Ai < Bi: return -1
	default: return bytes.Compare(Ab,Bb)
	}
}

type DB struct{
	tree unsafe.Pointer // *gtreap.Treap
	Ch *Chunk
	seed uint64
	flags LoadFlags
}

/*
This method initializes the search-tree and loads the Database.

".Ch" must be set to a properly initialized *Chunk-object.
*/
func (db *DB) Open(flags LoadFlags) {
	db.seed = uint64(time.Now().Unix())
	db.tree = unsafe.Pointer(gtreap.NewTreap(compareItem))
	db.flags = flags
	// Load the Key-Value records
	db.load()
}
func (db *DB) hash(b []byte) uint32 {
	if !db.flags.has(L_HASHED) { return 0 }
	return farm.Fingerprint32(b)
}
func (db *DB) load() {
	mem,pos := db.Ch.GetCommitted()
	
	
	t := (*gtreap.Treap)(atomic.LoadPointer(&(db.tree)))
	vv := new(*Value)
	
	// The end of the last valid record in the log.
	usedPos := pos
	for {
		v := new(Value)
		err := v.Decode(mem)
		if err==EShortRecord { break }
		if err==EInvalidRecord && db.flags.has(L_STOP_INVALID) { break }
		mem = mem[len(v.Record):]
		pos += int64(len(v.Record))
		
		if err!=nil { continue } // Do not use invalid or short records!
		
		*vv = v
		
		v.sHash(db.hash(v.GKey()))
		
		// Empty value means removal
		if len(v.GValue())==0 {
			/* Touch the treap, only if it already contains this item. */
			if t.Get(vv)!=nil {
				t = t.Delete(vv)
			}
		} else if oo := t.Get(vv); oo!=nil {
			(*(oo.(**Value))) = v
		} else {
			/*
			The pseudo-random priority is generated by a siphash'esce
			hash-function, that is keyed by a seed choosen at startup-time.
			That approach is very scalable and poses no lock-contention.
			*/
			priority := int(farm.Hash64WithSeed(v.Record, db.seed))
			if priority<0 { priority = ^priority }
			t = t.Upsert(vv,priority)
			vv = new(*Value)
		}
		
		// Update the end of the last record.
		usedPos = pos
	}
	atomic.StorePointer(&(db.tree),unsafe.Pointer(t))
	if db.flags.has(L_DISCARD_INVALID_TAIL) {
		db.Ch.SetLast(usedPos)
	}
}
func (db *DB) indexRecord(record []byte) (err error){
	v := new(Value).Set(record)
	v.sHash(db.hash(v.GKey()))
	
	vv := new(*Value)
	*vv = v
	priority := -1
	
	restart:
	
	ptr := atomic.LoadPointer(&(db.tree))
	tree := (*gtreap.Treap)(ptr)
	
	if len(v.GValue())==0 {
		/* Touch the treap, only if it already contains this item. */
		if tree.Get(vv)!=nil {
			tree = tree.Delete(vv)
			nptr := unsafe.Pointer(tree)
			if !atomic.CompareAndSwapPointer(&(db.tree),ptr,nptr) { goto restart }
		}
		/* Free(vv) */
	} else if oo := tree.Get(vv); oo!=nil {
		(*(oo.(**Value))) = v
		/* Free(vv) */
	} else {
		if priority<0 {
			/*
			The pseudo-random priority is generated by a siphash'esce
			hash-function, that is keyed by a seed choosen at startup-time.
			That approach is very scalable and poses no lock-contention.
			*/
			priority = int(farm.Hash64WithSeed(record, db.seed))
			if priority<0 { priority = ^priority }
		}
		tree = tree.Upsert(vv, priority)
		nptr := unsafe.Pointer(tree)
		if !atomic.CompareAndSwapPointer(&(db.tree),ptr,nptr) { goto restart }
	}
	
	return
}
// Inserts a raw Record. Useful if you copy or compact a database.
func (db *DB) PutRAW(record []byte) (err error) {
	record,_,err = db.Ch.Append(record)
	if err!=nil { return }
	err = db.indexRecord(record)
	return
}
func (db *DB) Delete(key []byte) (err error) {
	return db.Put(key,nil)
}
func (db *DB) Put(key, value []byte) (err error) {
	var reclen int
	var record []byte
	reclen,err = recordLength(len(key),len(value))
	if err!=nil { return }
	record,_,err = db.Ch.Skip(int64(reclen))
	if err!=nil { return }
	encodeRecordInto(record,key,value)
	err = db.indexRecord(record)
	return
}

/*
Obtains a record object (*Value).

The argument "synced" is handled as follows:
	if synced {
		ptr = atomic.LoadPointer(&(db.tree))
	} else {
		ptr = db.tree
	}
*/
func (db *DB) GetRecord(key []byte, synced bool) (v *Value,ok bool) {
	var ptr unsafe.Pointer
	var vv **Value
	if synced {
		ptr = atomic.LoadPointer(&(db.tree))
	} else {
		ptr = db.tree
	}
	
	tree := (*gtreap.Treap)(ptr)
	oo := tree.Get(iKey{db.hash(key),key})
	vv,ok = oo.(**Value)
	if ok { v = *vv }
	return
}
/*
Obtains a value. The memory mapped version of its key is also returned.

The argument "synced" is handled as follows:
	if synced {
		ptr = atomic.LoadPointer(&(db.tree))
	} else {
		ptr = db.tree
	}
*/
func (db *DB) Get(key []byte, synced bool) (mkey,value []byte,ok bool) {
	var v *Value
	v, ok = db.GetRecord(key,synced)
	if ok {
		mkey = v.GKey()
		value = v.GValue()
	}
	return
}
// .Commit() should be called by only one thread at the time.
func (db *DB) Commit() {
	db.Ch.Snapshot()
}

