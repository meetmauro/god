
package god

import (
	"github.com/zond/gotomic"
	"regexp"
	"fmt"
	"time"
	"io"
	"os"
	"log"
	"encoding/gob"
	"sort"
	"path/filepath"
)

const (
	backlog = 1 << 10
	snapshot = "snapshot.god"
	shards = 1 << 9
	arity_error = "Illegal number of parameters. Wanted %v but got %v."
)

const (
	GET = Command(iota)
	PUT 
	DELETE
	KEYS
)

const (
	OK = Result(1 << iota)
	MISSING
	EXISTS
	UNKNOWN
	BAD
	ARITY
)

type Command int
func (self Command) String() string {
	switch self {
	case GET:
		return "GET"
	case PUT:
		return "PUT"
	case DELETE:
		return "DELETE"
	}
	return "UNKNOWN"
}

type Result int
func (self Result) String() string {
	var rval []string
	if self & OK == OK {
		rval = append(rval, "OK")
	}
	if self & MISSING == MISSING {
		rval = append(rval, "MISSING")
	}
	if self & EXISTS == EXISTS {
		rval = append(rval, "EXISTS")
	}
	if self & UNKNOWN == UNKNOWN {
		rval = append(rval, "UNKNOWN")
	}
	if self & BAD == BAD {
		rval = append(rval, "BAD")
	}
	if self & ARITY == ARITY {
		rval = append(rval, "ARITY")
	}
	return fmt.Sprint(rval)
}

type Operation struct {
	Command Command
	Parameters []string
}

type Response struct {
	Result Result
	Parts []string
}

type God struct {
	hashes []*gotomic.Hash
	dir string
	logChannel chan Operation
	doLog bool
}
func NewGod(dir string) (*God, error) {
	os.MkdirAll(dir, 0700)
	rval := &God{make([]*gotomic.Hash, shards), dir, make(chan Operation, backlog), false}
	for i := 0; i < len(rval.hashes); i++ {
		rval.hashes[i] = gotomic.NewHash()
	}
	if err := rval.loadAll(); err != nil {
		return nil, err
	}
	rval.doLog = true
	go rval.log()
	return rval, nil
}
func (self *God) loadAll() error {
	if err := self.load(filepath.Join(self.dir, snapshot)); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	directory, err := os.Open(self.dir)
	if err != nil {
		return err
	}
	children, err := directory.Readdirnames(-1)
	if err != nil {
		return err
	}
	sort.Sort(sort.StringSlice(children))
	for _, child := range children {
		if match, err := regexp.Match("\\.log$", []byte(child)); err == nil && match {
			if err = self.load(filepath.Join(self.dir, child)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (self *God) log() {
	logfile, err := os.Create(filepath.Join(self.dir, fmt.Sprint(time.Now().UnixNano(), ".log")))
	if err != nil {
		panic(err)
	}
	defer logfile.Close()
	encoder := gob.NewEncoder(logfile)
	for operation := range self.logChannel {
		if err = encoder.Encode(operation); err != nil {
			panic(err)
		}
	}
}
func (self *God) load(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := gob.NewDecoder(file)
	operation := Operation{}
	response := Response{}
	err = decoder.Decode(&operation)
	for err == nil {
		self.Perform(operation, &response)
		err = decoder.Decode(&operation)
	}
	if err != io.EOF {
		log.Println(err)
	}
	return nil
}
func (self *God) okArity(o Operation, wanted int, r *Response) bool {
	if len(o.Parameters) != wanted {
		r.Result = BAD | ARITY
		r.Parts = []string{fmt.Sprint(arity_error, wanted, len(o.Parameters))}
		return false
	}
	return true
}
func (self *God) get(o Operation, r *Response) {
	if !self.okArity(o, 1, r) {
		return
	}
	key := gotomic.StringKey(o.Parameters[0])
	hash, hc := self.shard(key)
	if t, ok := hash.GetHC(hc, key); ok {
		r.Result = OK | EXISTS
		r.Parts = []string{t.(string)}
		return
	}
	r.Result = OK | MISSING
	r.Parts = nil
}
func (self *God) put(o Operation, r *Response) {
	if !self.okArity(o, 2, r) {
		return
	}
	if self.doLog {
		self.logChannel <- o
	}
	key := gotomic.StringKey(o.Parameters[0])
	hash, hc := self.shard(key)
	if t, ok := hash.PutHC(hc, key, o.Parameters[1]); ok {
		r.Result = OK | EXISTS
		r.Parts = []string{t.(string)}
		return
	} else {
		r.Result = OK | MISSING
		r.Parts = []string{""}
	}
}
func (self *God) keys(o Operation, r *Response) {
	if !self.okArity(o, 0, r) {
		return
	}
	var keys []string
	for _,shard := range self.hashes {
		shard.Each(func(k gotomic.Hashable, v gotomic.Thing) {
			keys = append(keys, string(k.(gotomic.StringKey)))
		})
	}
	r.Result = OK
	r.Parts = keys
}
func (self *God) del(o Operation, r *Response) {
	if !self.okArity(o, 1, r) {
		return
	}
	key := gotomic.StringKey(o.Parameters[0])
	hash, hc := self.shard(key)
	if t, ok := hash.DeleteHC(hc, key); ok {
		r.Result = OK | EXISTS
		r.Parts = []string{t.(string)}
	} else {
		r.Result = OK | MISSING
		r.Parts = []string{""}
	}
}
func (self *God) Perform(o Operation, r *Response) {
	switch o.Command {
	case GET:
		self.get(o, r)
	case PUT:
		self.put(o, r)
	case DELETE:
		self.del(o, r)
	case KEYS:
		self.keys(o, r)
	default:
		r.Result = UNKNOWN
		r.Parts = nil
	}
}
func (self *God) shard(h gotomic.Hashable) (hash *gotomic.Hash, hashCode uint32)  {
	hashCode = h.HashCode()
	hash = self.hashes[hashCode & (shards - 1)]
	return
}
